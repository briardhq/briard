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
      # under test at that moment, and after the kill that set changes.
      environment.systemPackages = [ pkgs.mosquitto ];
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

    # ---- (2) UPGRADE ONE, LEAVE THE OTHER ALONE -----------------------------------------------
    dummy_unit = container_unit(primary)
    dummy_started = started_at(primary, dummy_unit)
    # The rollback point, taken the way the maintenance bracket takes it: a read-only snapshot of
    # the SERVICE's subvolume, before anything changes.
    primary.succeed("mkdir -p /var/lib/briard/.snapshots")
    primary.succeed("btrfs subvolume snapshot -r ${subvol} ${snap}")

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

    # Single-primary preserved: exactly one survivor holds the DRBD primary role.
    primaries = [m.name for m in survivors if role(m) == "Primary"]
    assert len(primaries) == 1, f"expected one primary among survivors, got {primaries}"
  '';
}
