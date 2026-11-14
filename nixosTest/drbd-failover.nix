# 3-node majority topology: kill the primary, VIP + data move to a survivor.
#
# The stock-unit + promoter setup of drbd-promote, with three full nodes and a real failure: kill
# the elected primary; the survivors (2 of 3, still quorate) promote one of their own, reclaim the
# VIP, and serve the dummy's last persisted tick (protocol C). The minority self-fence: drbd-fence.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [
      { name = "node1"; id = 0; }
      { name = "node2"; id = 1; }
      { name = "node3"; id = 2; }
    ];
  };
in
pkgs.testers.runNixOSTest {
  name = "drbd-failover";

  # crash() is QemuMachine-only, and the primary is selected dynamically, so the
  # static checker can't follow it.
  skipTypeCheck = true;

  nodes = {
    node1 = node;
    node2 = node;
    node3 = node;
  };

  testScript = ''
    ${h.fixtureHelpers}
    import json

    machines = [node1, node2, node3]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.wait_for_unit("briard-test-fixture-install.service") # image warm on every node, before any promotion
        m.succeed("modprobe drbd")
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")

    # The outage that triggers a takeover often kills WAN too, so failover must need zero
    # internet. No default route, asserted gone: a local routing fact, not a curl to a public IP
    # (which can fail for unrelated reasons and is vacuous in the WAN-less sandbox). DRBD and the
    # VIP are directly connected, so the takeover is unaffected.
    for m in machines:
        m.succeed("ip route del default 2>/dev/null || true")
        m.fail("ip route show default | grep -q .")

    # Each node reaches both peers. Only the seed skipped its initial sync; the joiners sync
    # from it for real (asserted before the crash below).
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")

    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    # The VIP comes up on the elected primary with nothing installed (the shipped state): the
    # reverse proxy answers its own /healthz.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    primary = next(m for m in machines if role(m) == "Primary")
    # The app is installed onto the volume by whoever holds it. The survivor never runs an
    # install: it renders from that volume when it promotes, the half this test is about.
    dataroot = install_fixture(primary)
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    # A clear threshold makes "data survived" decisive: a lost volume is re-formatted blank and
    # restarts near 0, far below this.
    primary.wait_until_succeeds(f"[ $(grep -oE '[0-9]+' {dataroot}/app/state.json) -ge 3 ]")
    t1 = int(json.loads(primary.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    print(f"primary={primary.name} tick={t1}")

    # The crash must find an UpToDate replica. The seed path syncs a joiner for real (a
    # `--clear-bitmap` with every peer connected would declare them all UpToDate without one),
    # and a SyncTarget cannot promote. "Replicated" is the claim under test, so assert the disk
    # state before removing the only UpToDate copy.
    for m in machines:
        if m != primary:
            m.wait_until_succeeds("drbdadm dstate r0 | grep -qE '^(UpToDate|Diskless)'", timeout=300)
    # Kill the primary abruptly (power-loss shape).
    primary.crash()
    survivors = [m for m in machines if m != primary]

    # The survivors promote one of their own; the VIP answers again from any survivor on the
    # segment (gratuitous ARP moved it).
    surv = survivors[0]
    surv.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")

    # The last persisted tick came across (protocol C); a lost volume would be re-formatted blank
    # and restart near 0.
    t2 = int(json.loads(surv.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    assert t2 >= t1 - 2, f"data lost across failover: t1={t1} t2={t2}"

    # Single-primary preserved: exactly one survivor is now the DRBD primary.
    primaries = [m.name for m in survivors if role(m) == "Primary"]
    assert len(primaries) == 1, f"expected one primary among survivors, got {primaries}"
  '';
}
