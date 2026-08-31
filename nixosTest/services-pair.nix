# TWO services on one node, which is the claim [V3b.3] made and nothing measured ([V3b.4]).
#
# The coordination went plural — one promoter, N services rendered from the volume by
# briard-services at promotion — but with one service in the catalog every test of it was a test
# of N=1, and the properties that only exist at N=2 were reasoning rather than measurement:
#
#   1. BOTH services converge from the volume and run together, on a node that installed them and
#      on a survivor that was never told about either.
#   2. Upgrading ONE leaves the OTHER ALONE. converge bounces only the services whose rendered
#      bytes changed, and the alternative — restarting everything because one service moved — is
#      an outage for every other service in the house, invisible in any single-service test.
#   3. The pre-upgrade snapshot is a valid rollback point for the service that moved, and taking
#      it back does not disturb the one that did not.
#   4. A failover carries BOTH, with the data each of them wrote.
#
# The broker is the honest second service: it has real data (retained messages), it publishes its
# own version over its management API so an upgrade is observable from outside the container, and
# it is not Home Assistant — a pair of HA-shaped services would prove less than a pair that
# differs.
#
# HARNESS SCOPE, stated the way the other lib.nix rigs state it: the host agent's orchestration
# cannot run here (the node IS the guest, no host on the wire), so the install path's Primary-only
# half is install_fixture and the rollback is driven the way the product's bracket drives it — a
# btrfs snapshot taken before the change, restored after. What is REAL here is everything below
# that line: the renderer, converge, the promoter chain, and DRBD.
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
      # An MQTT client on every node: the broker's data is read from whichever node is NOT the one
      # under test at that moment, and after the kill that set changes. service-probe is the REAL
      # S1 probe as a command, so the rig drives the shipping code rather than a lookalike.
      environment.systemPackages = [
        pkgs.mosquitto
        (pkgs.callPackage ./service-probe-pkg.nix { })
      ];
    };
  # The broker's rollback point, in the product's own layout: a read-only sibling of the service's
  # subvolume under the btrfs root's .snapshots dir (agent/quadlet.SnapshotPath), so it replicates
  # with the volume rather than living on the node that took it.
  subvol = "/var/lib/briard/${mosquitto.name}";
  snap = "/var/lib/briard/.snapshots/${mosquitto.name}-preupgrade";
in
pkgs.testers.runNixOSTest {
  name = "services-pair";

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
        m.succeed("drbdadm create-md --force r0")
        m.succeed("systemctl start drbd@r0.target")

    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    def broker_version(m):
        """The broker's own answer, over the management API on the guest's loopback. Reading the
        version from the SERVICE rather than from the manifest is what makes an upgrade
        observable: a document naming a version proves nothing about what is running."""
        return m.succeed("curl -fsS http://127.0.0.1:9883/api/v1/version").strip()

    def container_unit(m, service=None):
        units = [u for u in fixture_units(m, service) if not u.endswith("-pod.service")]
        assert len(units) == 1, f"expected one container unit for {service}, got {units}"
        return units[0]

    def started_at(m, unit):
        """When systemd last started this unit. It is how "was it bounced?" is asked without
        trusting a log line."""
        return m.succeed(f"systemctl show -p ActiveEnterTimestamp --value {unit}").strip()

    node1.wait_until_succeeds(f"curl -fsS http://{VIP}/healthz", timeout=300)
    primary = next(m for m in machines if role(m) == "Primary")
    client = next(m for m in machines if m != primary)
    print(f"primary={primary.name} client={client.name}")

    # ---- (1) BOTH SERVICES, ONE NODE ----------------------------------------------------------
    install_fixture(primary)
    broker_root = install_fixture(primary, service="${mosquitto.name}")
    for service in [None, "${mosquitto.name}"]:
        for unit in fixture_units(primary, service):
            primary.wait_for_unit(unit, timeout=300)
    primary.wait_until_succeeds(f"curl -fsS -o /dev/null http://{VIP}:${toString fixture.port}${fixture.healthPath}", timeout=180)
    primary.wait_until_succeeds("curl -fsS -o /dev/null http://127.0.0.1:9883/api/v1/listeners", timeout=180)

    # WARM ON EVERY NODE, both services, by the DIGEST each manifest pins -- the standby holds the
    # images before it ever promotes, which is what makes a promotion that cannot pull survivable.
    for m in machines:
        m.succeed('podman image exists "$(cat /run/briard/fixtures/${fixture.name}/ref)"')
        m.succeed('podman image exists "$(cat /run/briard/fixtures/${mosquitto.name}/ref)"')
        m.succeed('podman image exists "$(cat /run/briard/fixtures/${mosquitto.name}/variants/to/ref)"')

    # Real data in the broker, written from OFF the box and read back from off the box.
    client.succeed(f"mosquitto_pub -h {VIP} -t briard/pair -m 'written-on-2.1.1' -r")
    assert broker_version(primary) == "2.1.1", "the pair did not start on its `from` version"


    # ---- (1b) TWO SERVICES, TWO NAMES, ONE DOOR -- AND THE BROKER IS NOT BEHIND IT ([B.48]) ----
    # N=1 could not test any of this. The properties that only exist at N=2 are that each name
    # reaches its OWN service, and that "fronted" is a per-service decision rather than a property
    # of there being exactly one thing to forward to.
    name_the_flock(primary)
    primary.succeed("briard-agent --converge")
    import json as _rj
    table = _rj.loads(primary.succeed("cat /run/briard/routes.json"))
    routed = {s["name"]: s for s in table["services"]}
    assert set(routed) == {"${fixture.name}", "${mosquitto.name}"}, f"routing table = {table}"
    assert routed["${fixture.name}"]["hosts"] != routed["${mosquitto.name}"]["hosts"], \
        f"the two services share a name: {table}"

    # The ordinary service: reachable through the door, on :80, by its own name.
    app_host = routed["${fixture.name}"]["hosts"][0]
    body = primary.succeed(f"curl -fsS -H 'Host: {app_host}' http://{VIP}${fixture.healthPath}")
    assert "ok" in body and "front door" not in body, f"the fixture through the door answered {body!r}"

    # THE BROKER IS NAMED BUT NOT FRONTED, and this is a security property rather than a nicety.
    # Its manifest port is the MANAGEMENT API, which mosquitto.conf binds to 127.0.0.1 on purpose
    # ("nothing outside the guest has any business reading it") -- and the front door runs inside
    # that guest, so a table that fronted it would republish a loopback-only endpoint on the LAN
    # through a mechanism that never mentions the bind.
    mq_host = routed["${mosquitto.name}"]["hosts"][0]
    assert not routed["${mosquitto.name}"].get("routes"), f"the broker has a route: {table}"
    # It keeps the name and the address: the name resolves to the VIP where MQTT listens, and the
    # address is what the health floor probes from inside the guest. Not-fronted is one of three.
    assert routed["${mosquitto.name}"]["health"], f"the broker has no health endpoint to probe: {table}"
    primary.succeed("curl -fsS -o /dev/null http://127.0.0.1:9883/api/v1/listeners")

    # The regression this guards, asserted from OFF the box, which is where it would matter.
    client.fail(f"curl -fsS -m 5 -o /dev/null http://{VIP}:9883/api/v1/listeners")
    denied = client.succeed(
        f"curl -sS -m 5 -H 'Host: {mq_host}' http://{VIP}/api/v1/listeners || true"
    )
    assert "listeners" not in denied, f"the front door served the broker's management API: {denied!r}"
    assert "${mosquitto.name}" in denied, f"the door did not name the service it declined to serve: {denied!r}"
    # NOT asserted by NAME from off the box: resolving `.local` needs nss-mdns on the client, which
    # is avahi's property and not this item's -- install-macvtap owns the does-a-stranger's-machine
    # resolve-it question. What is asserted here is the door's decision, which is what changed.

    # ---- (2) UPGRADE ONE, LEAVE THE OTHER ALONE -----------------------------------------------
    dummy_unit = container_unit(primary)
    dummy_started = started_at(primary, dummy_unit)
    # THE ROLLBACK POINT, TAKEN QUIESCED -- which is [B.121]'s rule, and this rig measured why it
    # is one. Taken LIVE (which is what applyServiceInstall does today) the snapshot did not
    # contain the message the broker had already accepted: mosquitto holds retained state in memory
    # and writes it on a clean stop or every autosave_interval, so a live snapshot of a service
    # that has just been written to is a rollback point missing exactly the data someone would roll
    # back FOR. The run that showed it: the upgrade carried the message across (2.1.2 logged
    # "Restored 1 retained messages"), and the rollback then restored a broker with nothing in it.
    #
    # So the bracket's own order is used here: stop the service, take the point, start it again.
    # Application-consistent by construction, and nothing writes between the stop and the snapshot.
    mq_units = fixture_units(primary, "${mosquitto.name}")
    mq_containers = [u for u in mq_units if not u.endswith("-pod.service")]
    for u in reversed(mq_containers):
        primary.succeed(f"systemctl stop {u}")
    primary.succeed("mkdir -p /var/lib/briard/.snapshots")
    primary.succeed("btrfs subvolume snapshot -r ${subvol} ${snap}")
    for u in mq_units:
        primary.succeed(f"systemctl start {u}")
    primary.wait_until_succeeds("curl -fsS -o /dev/null http://127.0.0.1:9883/api/v1/version", timeout=180)

    install_fixture(primary, variant="to", service="${mosquitto.name}")
    primary.wait_until_succeeds("curl -fsS -o /dev/null http://127.0.0.1:9883/api/v1/version", timeout=180)
    assert broker_version(primary) == "2.1.2", "the broker did not move to the upgraded version"

    # THE PROPERTY THAT ONLY EXISTS AT N=2: the other service was not touched. Same unit start
    # instant, and it is still answering -- a converge that bounced everything because one service
    # changed would fail here and nowhere else.
    assert started_at(primary, dummy_unit) == dummy_started, \
        "upgrading the broker restarted the other service"
    primary.succeed(f"curl -fsS -o /dev/null http://{VIP}:${toString fixture.port}${fixture.healthPath}")

    # The broker's data crossed its own upgrade: same retained message, new version serving it.
    got = client.succeed(f"mosquitto_sub -h {VIP} -t briard/pair -C 1 -W 15").strip()
    assert got == "written-on-2.1.1", f"the upgrade lost the broker's retained state: {got!r}"

    # ---- (3) ROLLBACK, AND ONLY OF THE SERVICE THAT MOVED --------------------------------------
    # The bracket's shape: stop the service, put its subvolume back, re-pin the previous manifest,
    # start again. Only the container units are stopped -- never the pod -- because stopping the
    # pod kills the members out from under their own units (agent/quadlet).
    primary.succeed(f"systemctl stop {container_unit(primary, '${mosquitto.name}')}")
    primary.succeed("btrfs subvolume delete ${subvol}")
    primary.succeed("btrfs subvolume snapshot ${snap} ${subvol}")
    install_fixture(primary, service="${mosquitto.name}")
    primary.wait_until_succeeds("curl -fsS -o /dev/null http://127.0.0.1:9883/api/v1/version", timeout=180)
    assert broker_version(primary) == "2.1.1", "the rollback did not put the previous version back"
    got = client.succeed(f"mosquitto_sub -h {VIP} -t briard/pair -C 1 -W 15").strip()
    assert got == "written-on-2.1.1", f"the rollback lost the data it exists to restore: {got!r}"
    assert started_at(primary, dummy_unit) == dummy_started, \
        "rolling back the broker restarted the other service"

    # ---- (4) FAILOVER WITH BOTH RIDING ---------------------------------------------------------
    # Everything the survivor runs, it renders from the VOLUME: it never installed either service.
    client.succeed(f"mosquitto_pub -h {VIP} -t briard/pair -m 'before-the-kill' -r")
    # WAIT FOR THE BROKER TO HAVE WRITTEN IT, then cut the power. mosquitto persists on a clean
    # stop and every autosave_interval (30s, agent/mosquitto) -- so an abrupt loss can cost up to
    # that much retained state, and a test that crashed the node the instant after a publish would
    # be asserting on a race rather than on replication. Waiting for the bytes to reach the file
    # is what makes this a claim about DRBD carrying what the service durably wrote.
    primary.wait_until_succeeds(f"grep -aq before-the-kill {broker_root}/broker/mosquitto.db", timeout=120)
    primary.succeed("sync")
    primary.crash()
    survivors = [m for m in machines if m != primary]

    survivors[0].wait_until_succeeds(f"curl -fsS -o /dev/null http://{VIP}:${toString fixture.port}${fixture.healthPath}", timeout=400)
    new_primary = next(m for m in survivors if role(m) == "Primary")
    for service in [None, "${mosquitto.name}"]:
        for unit in fixture_units(new_primary, service):
            new_primary.wait_for_unit(unit, timeout=300)

    # The broker came back on the version the volume names -- the rolled-back one, not the one the
    # dead node was upgraded to and reverted from.
    new_primary.wait_until_succeeds("curl -fsS -o /dev/null http://127.0.0.1:9883/api/v1/version", timeout=180)
    assert broker_version(new_primary) == "2.1.1", "the survivor promoted onto the wrong version"
    reader = next(m for m in survivors if m != new_primary)
    got = reader.succeed(f"mosquitto_sub -h {VIP} -t briard/pair -C 1 -W 20").strip()
    assert got == "before-the-kill", f"the broker's retained state did not cross the failover: {got!r}"
    new_primary.succeed(f"test -s {broker_root}/broker/mosquitto.db")


    # ---- (5) THE S1 PROBE, ON A REAL BROKER ---------------------------------------------------
    # The gate above the liveness floor ([V3b.4](d)): a token left in the service's own durable
    # state before a change and looked for after. What is asserted here is the SIGNAL -- that the
    # real probe discriminates the three states a broker can be in -- because a lib.nix rig cannot
    # run the host's install path at all. The verdicts those states map to are unit-tested
    # (agent/host/readiness.go), the same split hass-upgrade-rollback.nix makes for HA.
    broker_ctr = "briard-${mosquitto.name}-${mosquitto.container}"

    def probe(m, token=""):
        return m.succeed(f"service-probe {broker_ctr} {token}").strip()

    # (i) A REAL UPGRADE PRESERVES IT. The token is written through the product's own code path --
    # published retained and persisted with SIGUSR1 -- so what crosses the upgrade is what the
    # gate would have written, not a message a test left lying around.
    out = probe(new_primary, "briard-rig-token")
    assert out == "serving=true token=briard-rig-token", f"the probe did not store its token: {out}"
    install_fixture(new_primary, variant="to", service="${mosquitto.name}")
    new_primary.wait_until_succeeds("curl -fsS -o /dev/null http://127.0.0.1:9883/api/v1/version", timeout=180)
    assert broker_version(new_primary) == "2.1.2", "the broker did not move to the upgraded version"
    out = probe(new_primary)
    assert out == "serving=true token=briard-rig-token", \
        f"the token did not survive a real upgrade, so the gate would have rolled a good one back: {out}"

    # (ii) A BROKER THAT CAME BACK EMPTY IS SEEN. This is the failure no floor can catch: the
    # service answers its health endpoint and has lost every retained message the household had.
    # The store is emptied under it, which is what a mount that no longer covers the data
    # directory, or a database format the new version cannot read, produces.
    units = fixture_units(new_primary, "${mosquitto.name}")
    ctrs = [u for u in units if not u.endswith("-pod.service")]
    for u in reversed(ctrs):
        new_primary.succeed(f"systemctl stop {u}")
    new_primary.succeed(f"rm -f {broker_root}/broker/mosquitto.db")
    for u in units:
        new_primary.succeed(f"systemctl start {u}")
    new_primary.wait_until_succeeds("curl -fsS -o /dev/null http://127.0.0.1:9883/api/v1/version", timeout=180)
    out = probe(new_primary)
    assert out == "serving=true token=", f"a broker that lost everything still looked healthy to the probe: {out}"

    # (iii) AND A BROKER NOBODY CAN CONNECT TO IS A DIFFERENT ANSWER, not the same one. The two
    # are separate findings with separate reasons, so the probe has to tell them apart.
    for u in reversed(ctrs):
        new_primary.succeed(f"systemctl stop {u}")
    out = probe(new_primary)
    assert out == "serving=false token=", f"a stopped broker was not reported as unreachable: {out}"

    # Single-primary preserved: exactly one survivor holds the DRBD primary role.
    primaries = [m.name for m in survivors if role(m) == "Primary"]
    assert len(primaries) == 1, f"expected one primary among survivors, got {primaries}"
  '';
}
