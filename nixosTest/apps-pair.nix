# Two apps on one node, the claim converge-at-promotion makes (one promoter, N apps rendered from
# the volume by briard-apps at promotion). A single-app test is N=1; these hold only at N=2:
#
#   1. Both apps converge from the volume and run together, on the node that installed them and
#      on a survivor never told about either.
#   2. Upgrading one leaves the other alone: converge bounces only apps whose rendered bytes
#      changed; restarting everything would be an outage for every other app in the house,
#      invisible in any single-app test.
#   3. The pre-upgrade snapshot is a valid rollback point for the app that moved, and taking it
#      back does not disturb the one that did not.
#   4. A failover carries both, with the data each wrote.
#
# The broker is the honest second app: real data (retained messages), its own version published
# over its management API (an upgrade observable from outside the container), and not Home
# Assistant: a pair of HA-shaped apps would prove less than a pair that differs.
#
# Harness scope, as in the other lib.nix rigs: host agent orchestration cannot run here (the node
# is the guest, no host on the wire), so the Primary-only install half is install_fixture and the
# rollback is driven as the product bracket does (a btrfs snapshot before the change, restored
# after). Real here is everything below that line: the renderer, converge, the promoter chain, DRBD.
{ pkgs, guestModule, fixture, mosquitto }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  baseNode = h.mkNode {
    fixtures = [
      fixture
      mosquitto
    ];
    resource = h.mkResource [
      { name = "node1"; id = 0; }
      { name = "node2"; id = 1; }
      { name = "node3"; id = 2; }
    ];
  };
  node =
    { ... }:
    {
      imports = [ baseNode ];
      # An MQTT client on every node: broker data is read from a node not under test, a set the
      # kill changes. app-probe is the real S1 probe as a command: shipping code, not a lookalike.
      environment.systemPackages = [
        pkgs.mosquitto
        (pkgs.callPackage ./app-probe-pkg.nix { })
      ];
    };
  # The broker rollback point in the product layout: a read-only sibling of the app subvolume
  # under the data tier .snapshots dir (agent/quadlet SnapshotsDir), so it replicates with the
  # volume rather than living on the node that took it.
  subvol = "/var/lib/briard/data/${mosquitto.name}";
  snap = "/var/lib/briard/data/.snapshots/${mosquitto.name}-preupgrade";
in
pkgs.testers.runNixOSTest {
  name = "apps-pair";

  # crash() is QemuMachine-only and the primary is selected dynamically.
  skipTypeCheck = true;

  nodes = {
    node1 = node;
    node2 = node;
    node3 = node;
  };

  testScript = ''
    ${h.fixtureHelpers}
    VIP = "192.168.1.100"

    machines = [node1, node2, node3]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.wait_for_unit("briard-test-fixture-install.service", timeout=600)
        m.succeed("modprobe drbd")
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")

    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    def broker_mgmt(m, path="/api/v1/listeners"):
        """The broker's management API ON THIS NODE.

        It runs on a PRIVATE pod, so the address is the pod's and not the guest's
        loopback -- and it is per node, because each node allocates from its own pool, which is
        exactly what a failover exercises. Resolved through the routing table, which is where the
        product's own health probe resolves it: a test that hardcoded an address would be asserting
        against a place nothing else looks."""
        import json
        tbl = json.loads(m.succeed("cat /run/briard/routes.json"))
        for s in tbl["apps"]:
            if s["name"] == "${mosquitto.name}":
                assert s["address"].startswith("10.12."), f"the broker is not on the pod pool: {s}"
                return f"http://{s['address']}:9883{path}"
        raise AssertionError(f"the broker is not in {m.name}'s routing table: {tbl}")

    def broker_version(m):
        """The broker's own answer, over the management API on the guest's loopback. Reading the
        version from the APP rather than from the manifest is what makes an upgrade
        observable: a document naming a version proves nothing about what is running."""
        return m.succeed(f"curl -fsS {broker_mgmt(m, '/api/v1/version')}").strip()

    def container_unit(m, app=None):
        units = [u for u in fixture_units(m, app) if not u.endswith("-pod.service")]
        assert len(units) == 1, f"expected one container unit for {app}, got {units}"
        return units[0]

    def started_at(m, unit):
        """When systemd last started this unit. It is how "was it bounced?" is asked without
        trusting a log line."""
        return m.succeed(f"systemctl show -p ActiveEnterTimestamp --value {unit}").strip()

    node1.wait_until_succeeds(f"curl -fsS http://{VIP}/healthz", timeout=300)
    primary = next(m for m in machines if role(m) == "Primary")
    client = next(m for m in machines if m != primary)
    print(f"primary={primary.name} client={client.name}")

    # ---- (1) Both apps, one node --------------------------------------------------------------
    install_fixture(primary)
    broker_root = install_fixture(primary, app="${mosquitto.name}")
    for app in [None, "${mosquitto.name}"]:
        for unit in fixture_units(primary, app):
            primary.wait_for_unit(unit, timeout=300)
    primary.wait_until_succeeds(f"curl -fsS -o /dev/null http://{VIP}:${toString fixture.port}${fixture.healthPath}", timeout=180)
    primary.wait_until_succeeds(f"curl -fsS -o /dev/null {broker_mgmt(primary)}", timeout=180)

    # Warm on every node, both apps, by the digest each manifest pins: a standby holding the
    # images before it promotes is what makes a promotion that cannot pull survivable.
    for m in machines:
        m.succeed('podman image exists "$(cat /run/briard/fixtures/${fixture.name}/ref)"')
        m.succeed('podman image exists "$(cat /run/briard/fixtures/${mosquitto.name}/ref)"')
        m.succeed('podman image exists "$(cat /run/briard/fixtures/${mosquitto.name}/variants/to/ref)"')

    # Real data in the broker, written and read back from off the box.
    client.succeed(f"mosquitto_pub -h {VIP} -t briard/pair -m 'written-on-2.1.1' -r")
    assert broker_version(primary) == "2.1.1", "the pair did not start on its `from` version"


    # ---- (1b) Two apps, two names, one reverse proxy -- and the broker is not behind it ----
    # Only at N=2: each name reaches its own app, and "fronted" is a per-app decision, not a
    # property of there being exactly one thing to forward to.
    name_the_flock(primary)
    primary.succeed("briard-guest-agent --converge")
    import json as _rj
    table = _rj.loads(primary.succeed("cat /run/briard/routes.json"))
    routed = {s["name"]: s for s in table["apps"]}
    assert set(routed) == {"${fixture.name}", "${mosquitto.name}"}, f"routing table = {table}"
    assert routed["${fixture.name}"]["hosts"] != routed["${mosquitto.name}"]["hosts"], \
        f"the two apps share a name: {table}"

    # The ordinary app: reachable through the reverse proxy, on :80, by its own name.
    app_host = routed["${fixture.name}"]["hosts"][0]
    body = primary.succeed(f"curl -fsS -H 'Host: {app_host}' http://{VIP}${fixture.healthPath}")
    assert "ok" in body and "reverse proxy" not in body, f"the fixture through the reverse proxy answered {body!r}"

    # The broker is named but not fronted, a security property rather than a nicety. Its manifest
    # port is the management API, which mosquitto.conf keeps off the LAN on purpose ("nothing
    # outside the guest has any business reading it"; on this private pod it binds the pod
    # namespace). The reverse proxy runs inside that guest, so fronting it would republish a
    # guest-only endpoint on the LAN through a mechanism that never mentions the bind.
    mq_host = routed["${mosquitto.name}"]["hosts"][0]
    assert not routed["${mosquitto.name}"].get("routes"), f"the broker has a route: {table}"
    # It keeps the name (resolving to the VIP, where MQTT listens) and the address (what the
    # health floor probes from inside the guest). Not-fronted is one of three.
    assert routed["${mosquitto.name}"]["health"], f"the broker has no health endpoint to probe: {table}"
    primary.succeed(f"curl -fsS -o /dev/null {broker_mgmt(primary)}")

    # The regression this guards, asserted from off the box, where it would matter.
    client.fail(f"curl -fsS -m 5 -o /dev/null http://{VIP}:9883/api/v1/listeners")
    denied = client.succeed(
        f"curl -sS -m 5 -H 'Host: {mq_host}' http://{VIP}/api/v1/listeners || true"
    )
    assert "listeners" not in denied, f"the reverse proxy served the broker's management API: {denied!r}"
    assert "${mosquitto.name}" in denied, f"the reverse proxy did not name the app it declined to serve: {denied!r}"
    # Not asserted by name from off the box: resolving `.local` needs nss-mdns on the client, an
    # avahi property, not one this test owns; install-macvtap owns whether the machine of a
    # stranger resolves it. Asserted here is the reverse proxy decision.

    # ---- (2) Upgrade one, leave the other alone -----------------------------------------------
    dummy_unit = container_unit(primary)
    dummy_started = started_at(primary, dummy_unit)
    # The rollback point, taken quiesced: the product rule (applyOneAppInstall stops the app
    # first), and this rig shows why. mosquitto holds retained state in memory and writes it on a
    # clean stop or every autosave_interval, so a live snapshot of a just-written app misses
    # exactly the data someone would roll back for: the upgrade carries the message across, and
    # the rollback restores an empty broker. So the bracket order: stop, take the point, start.
    # Application-consistent by construction, nothing writes between the stop and the snapshot.
    mq_units = fixture_units(primary, "${mosquitto.name}")
    mq_containers = [u for u in mq_units if not u.endswith("-pod.service")]
    for u in reversed(mq_containers):
        primary.succeed(f"systemctl stop {u}")
    primary.succeed("mkdir -p /var/lib/briard/data/.snapshots")
    primary.succeed("btrfs subvolume snapshot -r ${subvol} ${snap}")
    for u in mq_units:
        primary.succeed(f"systemctl start {u}")
    primary.wait_until_succeeds(f"curl -fsS -o /dev/null {broker_mgmt(primary, '/api/v1/version')}", timeout=180)

    install_fixture(primary, variant="to", app="${mosquitto.name}")
    primary.wait_until_succeeds(f"curl -fsS -o /dev/null {broker_mgmt(primary, '/api/v1/version')}", timeout=180)
    assert broker_version(primary) == "2.1.2", "the broker did not move to the upgraded version"

    # The N=2 property: the other app was not touched (same unit start instant, still answering).
    # A converge that bounced everything because one app changed fails here and nowhere else.
    assert started_at(primary, dummy_unit) == dummy_started, \
        "upgrading the broker restarted the other app"
    primary.succeed(f"curl -fsS -o /dev/null http://{VIP}:${toString fixture.port}${fixture.healthPath}")

    # The broker's data crossed its own upgrade: same retained message, new version serving it.
    got = client.succeed(f"mosquitto_sub -h {VIP} -t briard/pair -C 1 -W 15").strip()
    assert got == "written-on-2.1.1", f"the upgrade lost the broker's retained state: {got!r}"

    # ---- (3) Rollback, and only of the app that moved ------------------------------------------
    # The bracket shape: stop the app, put its subvolume back, re-pin the previous manifest,
    # start. Container units only, never the pod: stopping the pod kills the members out from
    # under their own units (agent/quadlet).
    primary.succeed(f"systemctl stop {container_unit(primary, '${mosquitto.name}')}")
    primary.succeed("btrfs subvolume delete ${subvol}")
    primary.succeed("btrfs subvolume snapshot ${snap} ${subvol}")
    install_fixture(primary, app="${mosquitto.name}")
    primary.wait_until_succeeds(f"curl -fsS -o /dev/null {broker_mgmt(primary, '/api/v1/version')}", timeout=180)
    assert broker_version(primary) == "2.1.1", "the rollback did not put the previous version back"
    got = client.succeed(f"mosquitto_sub -h {VIP} -t briard/pair -C 1 -W 15").strip()
    assert got == "written-on-2.1.1", f"the rollback lost the data it exists to restore: {got!r}"
    assert started_at(primary, dummy_unit) == dummy_started, \
        "rolling back the broker restarted the other app"

    # ---- (4) Failover with both riding ---------------------------------------------------------
    # The survivor renders everything it runs from the volume: it never installed either app.
    client.succeed(f"mosquitto_pub -h {VIP} -t briard/pair -m 'before-the-kill' -r")
    # Wait for the broker to have written it, then cut the power. mosquitto persists on a clean
    # stop and every autosave_interval (30s, agent/mosquitto), so an abrupt loss can cost up to
    # that much retained state; crashing right after a publish would assert on a race, not on
    # replication. Waiting for the bytes in the file makes this a claim about DRBD carrying what
    # the app durably wrote.
    primary.wait_until_succeeds(f"grep -aq before-the-kill {broker_root}/${mosquitto.dataDir}/mosquitto.db", timeout=120)
    primary.succeed("sync")
    # The crash must find an UpToDate replica: the product seed path really syncs a joiner, which
    # is a SyncTarget, unable to promote, until the initial resync completes. "Replicated" is the
    # claim under test, so assert the disk state before removing the only UpToDate copy.
    for m in machines:
        if m != primary:
            m.wait_until_succeeds("drbdadm dstate r0 | grep -q '^UpToDate'", timeout=300)
    primary.crash()
    survivors = [m for m in machines if m != primary]

    survivors[0].wait_until_succeeds(f"curl -fsS -o /dev/null http://{VIP}:${toString fixture.port}${fixture.healthPath}", timeout=400)
    new_primary = next(m for m in survivors if role(m) == "Primary")
    for app in [None, "${mosquitto.name}"]:
        for unit in fixture_units(new_primary, app):
            new_primary.wait_for_unit(unit, timeout=300)

    # Back on the version the volume names: the rolled-back one, not the one the dead node was
    # upgraded to and reverted from.
    new_primary.wait_until_succeeds(f"curl -fsS -o /dev/null {broker_mgmt(new_primary, '/api/v1/version')}", timeout=180)
    assert broker_version(new_primary) == "2.1.1", "the survivor promoted onto the wrong version"
    reader = next(m for m in survivors if m != new_primary)
    got = reader.succeed(f"mosquitto_sub -h {VIP} -t briard/pair -C 1 -W 20").strip()
    assert got == "before-the-kill", f"the broker's retained state did not cross the failover: {got!r}"
    new_primary.succeed(f"test -s {broker_root}/${mosquitto.dataDir}/mosquitto.db")


    # ---- (5) The S1 probe, on a real broker ---------------------------------------------------
    # The gate above the liveness floor: a token left in the app durable state before a change
    # and looked for after. Asserted is the signal (the real probe tells apart the three states a
    # broker can be in), as a lib.nix rig cannot run the host install path; the verdicts are
    # unit-tested (agent/host/readiness.go), the split hass-upgrade-rollback.nix makes for HA.
    broker_ctr = "briard-${mosquitto.name}-${mosquitto.container}"

    def probe(m, token=""):
        return m.succeed(f"app-probe {broker_ctr} {token}").strip()

    # (i) A real upgrade preserves it. The token goes through the product code path (published
    # retained, persisted with SIGUSR1), so what crosses is what the gate would write.
    out = probe(new_primary, "briard-rig-token")
    assert out == "serving=true token=briard-rig-token", f"the probe did not store its token: {out}"
    install_fixture(new_primary, variant="to", app="${mosquitto.name}")
    new_primary.wait_until_succeeds(f"curl -fsS -o /dev/null {broker_mgmt(new_primary, '/api/v1/version')}", timeout=180)
    assert broker_version(new_primary) == "2.1.2", "the broker did not move to the upgraded version"
    out = probe(new_primary)
    assert out == "serving=true token=briard-rig-token", \
        f"the token did not survive a real upgrade, so the gate would have rolled a good one back: {out}"

    # (ii) A broker that came back empty is seen: the failure no floor catches, healthy endpoint
    # and every retained message lost. The store is emptied under it, as a mount no longer
    # covering the data directory, or a format the new version cannot read, would do.
    units = fixture_units(new_primary, "${mosquitto.name}")
    ctrs = [u for u in units if not u.endswith("-pod.service")]
    for u in reversed(ctrs):
        new_primary.succeed(f"systemctl stop {u}")
    new_primary.succeed(f"rm -f {broker_root}/${mosquitto.dataDir}/mosquitto.db")
    for u in units:
        new_primary.succeed(f"systemctl start {u}")
    new_primary.wait_until_succeeds(f"curl -fsS -o /dev/null {broker_mgmt(new_primary, '/api/v1/version')}", timeout=180)
    out = probe(new_primary)
    assert out == "serving=true token=", f"a broker that lost everything still looked healthy to the probe: {out}"

    # (iii) A broker nobody can connect to is a different answer: separate findings with separate
    # reasons, which the probe must tell apart.
    for u in reversed(ctrs):
        new_primary.succeed(f"systemctl stop {u}")
    out = probe(new_primary)
    assert out == "serving=false token=", f"a stopped broker was not reported as unreachable: {out}"

    # Single-primary preserved: exactly one survivor holds the DRBD primary role.
    primaries = [m.name for m in survivors if role(m) == "Primary"]
    assert len(primaries) == 1, f"expected one primary among survivors, got {primaries}"
  '';
}
