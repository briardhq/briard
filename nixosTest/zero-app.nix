# Zero apps is the shipped state. The guest boots exactly as the downloadable artifact is built
# (guest-image/configuration.nix, no workload), and a node with nothing installed must work: the
# volume mounts, the promoter chain completes, the VIP comes up and the reverse proxy answers.
# Nothing bakes a workload into a guest, so this is what every node boots as; the tests here that
# install an app (a catalogued fixture) after promoting add to it.
#
# The failure guarded: with a workload unit unconditionally in the promoter chain, a node with
# nothing installed names a unit that does not exist, drbd-reactor fails the whole ordered unit,
# and the VIP never comes up -- a node that installs cleanly and then serves nothing at all.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # Three nodes for the same reason every other failover test uses three: a crashed primary
  # leaves the survivor at 1/2 without a witness, and quorum correctly refuses to promote.
  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  node = h.mkNode { inherit resource; }; # no fixture: the shipped shape, running nothing
  witnessNode = h.mkNode { inherit resource; diskless = true; promoter = false; };
in
pkgs.testers.runNixOSTest {
  name = "zero-app";
  skipTypeCheck = true;

  nodes = {
    node1 = node;
    node2 = node;
    witness = witnessNode;
  };

  testScript = ''
    disk_nodes = [node1, node2]
    machines = [node1, node2, witness]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
    for m in disk_nodes:
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")
    # The witness has no tier to build but still needs its `.res` and its attach:
    # briard-node-storage runs on every node, so one call covers both.
    witness.succeed("briard-test-storage")
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")

    # No app unit exists at all, not merely stopped: units are rendered (into /run) by an
    # install, so the directory quadlet reads is empty. That keeps the shipped closure free of a
    # workload rather than carrying a dormant one.
    for m in disk_nodes:
        m.fail("ls /run/containers/systemd/briard-* >/dev/null 2>&1")
        m.fail("systemctl list-units --all 'briard-*-app.service' | grep -q briard-")

    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")

    # The assertion: the promoter chain completes and the VIP answers, with nothing installed.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=60)

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    primary = next(m for m in disk_nodes if role(m) == "Primary")
    survivor = next(m for m in disk_nodes if m != primary)
    print(f"primary={primary.name} survivor={survivor.name}")

    # Converge ran and converged to nothing. briard-apps is a chain member on every data node, so
    # its failure would take the VIP down on the node shape every stranger installs -- the failure
    # this test guards, via a new unit. Succeeding on an empty volume is the shipped state, not a
    # degraded one.
    primary.succeed("systemctl is-active briard-apps.service")
    left = primary.succeed("ls -A /run/containers/systemd 2>/dev/null || true").strip()
    assert left == "", f"converge wrote app units on a node with nothing installed: {left!r}"

    # Ready, not sick: without the reverse proxy answering /healthz itself, a fresh install would
    # sit unhealthy forever, its probe pointed at an app port nobody listens on.
    health = primary.succeed("curl -fsS http://192.168.1.100/healthz")
    assert "no apps routed" in health, f"/healthz on an empty node said: {health!r}"

    # A human opening the VIP gets an answer, not a connection refused, but one that tells them
    # nothing until they are trusted: the bare address is a name the reverse proxy does not route,
    # so it forwards to the household dashboard, which refuses a browser with no session (401, one
    # instruction, no inventory). Reaching the VIP is not authentication; no node page ("Nothing
    # is installed") is served, since what a node runs is what the session gate withholds. No
    # `-f`: 401 would fail curl itself, and asserting the code keeps "the VIP is down" and "the VIP
    # refused me" distinguishable.
    refused = primary.succeed("curl -sS -o /tmp/vip.html -w '%{http_code}' http://192.168.1.100/").strip()
    page = primary.succeed("cat /tmp/vip.html")
    assert refused == "401" and "not yet trusted" in page, f"the VIP served {refused}: {page!r}"
    # An empty node has nothing to leak, so this cannot pass by there being nothing to name. The
    # inventory assertion that matters lives in app-install.nix, where an app IS installed.
    assert "Nothing is installed" not in page, f"the refusal still renders the node's page: {page!r}"

    # The routing table exists and is empty, not absent: converge writes it on every promotion,
    # so "routes nothing" is something the node said. An absent file would mean the reverse proxy
    # was reading whatever the last install left behind.
    import json
    assert json.loads(primary.succeed("cat /run/briard/routes.json")) == {"apps": []}, \
        f"the routing table on an empty node: {primary.succeed('cat /run/briard/routes.json')!r}"
    primary.fail("test -e /run/briard/routes.hosts")  # one file: the publisher reads the table itself

    # The data volume is mounted and empty of app data (data/.snapshots holds History members,
    # not an app).
    primary.succeed("mountpoint -q /var/lib/briard")
    primary.fail("ls -d /var/lib/briard/.apps/*.json >/dev/null 2>&1")

    # Failover works with nothing installed: the node replicates and can take over before it has
    # a workload, so a later install lands on a substrate already proven.
    #
    # The crash must find an UpToDate replica. The seed path syncs a joiner for real, so a replica
    # is a SyncTarget, which cannot promote, until the initial resync completes; "replicated" is
    # the claim under test, so the disk state is asserted before the only UpToDate copy goes. Of
    # the rigs that crash a primary this one is the most exposed: it installs nothing, so the
    # crash arrives seconds after the full resync starts, with no install between them to hide it.
    survivor.wait_until_succeeds("drbdadm dstate r0 | grep -q '^UpToDate'", timeout=300)
    primary.crash()
    survivor.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
    print("an empty node fails over and still answers at the VIP — the substrate is the product")

    # The clear key, proved by a power cut: the node lost power with its volume encrypted and
    # opens it again with nothing supplied by anybody, because slot 0's passphrase is in a LUKS2
    # token on the same disk. That makes encryption-by-default possible on a stranger's LAN, where
    # no unlock scheme generalises, and is exactly why the honest claim is "ready to be armed",
    # never "protected". It rides this crash: only a boot asks the question, and this is the one
    # rig that already pays for one.
    primary.start()
    primary.wait_for_unit("multi-user.target")
    # The host starts the unit, never boot, so the harness starts it as it did the first time;
    # the node opens its own volume from the token. Re-running is the assertion: nothing here
    # supplies a key.
    primary.succeed("briard-test-storage")
    primary.succeed("cryptsetup isLuks /dev/vdb")
    primary.succeed("test -b /dev/mapper/briardvol-data")
    assert "briardvol-crypt" in primary.succeed("dmsetup deps -o devname /dev/mapper/briardvol-data"), \
        "the volume came back unencrypted after a power cut"
    print("the node opened its own encrypted volume after a power cut, unattended")
  '';
}
