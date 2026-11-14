# The broker as a catalogued app: it installs the shipped way, runs on Briard's config rather
# than its image's, and keeps what it was told to keep.
#
# None of what this catches is visible to a test that only asks "is it up": the mosquitto image
# ships with persistence off and its management API on every interface, so a broker that starts
# proves nothing a household needs. The product replaces that config from `agent/apps`, and
# every claim here is about the difference:
#
#   1. the config is in effect (not merely rendered): the API answers at the pod address;
#   2. the API is not on the LAN, asserted from another machine, the only honest place to observe;
#   3. MQTT itself is on the LAN, from that same client: a broker nobody can reach would pass
#      (1) and (2) perfectly;
#   4. a retained message survives a container restart and lands in the file on the replicated
#      subvolume, which makes the broker an app with data, carried across a node by apps-pair;
#   5. the broker is findable: it answers a `_mqtt._tcp` browse from that client, under the flock
#      name and on the port MQTT listens on, and the result is connected to. An image that
#      advertises nothing is invisible to every appliance in the house, a broker nobody without
#      our documentation can use.
#
# Two nodes: node2 is the off-box client, the outside from which (2), (3) and (5) are observed.
# node1 seeds the volume; the script takes whichever node promoted as the broker and the other
# as the client.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [
      { name = "node1"; id = 0; }
      { name = "node2"; id = 1; }
    ];
  };
  # The client is the outside: what matters is that publish, subscribe and browse happen from a
  # machine not running the broker (that its mosquitto is the image build is incidental).
  client =
    { ... }:
    {
      imports = [ node ];
      # avahi too: the broker announcement is observed here, since observing it from the
      # publishing machine would prove nothing.
      environment.systemPackages = [ pkgs.mosquitto pkgs.avahi ];
      # ⚠️ And the daemon those tools talk to, run by this node for itself. `avahi-resolve` and
      # `avahi-browse` are D-Bus clients: with no local daemon they fail with "Failed to create
      # client object: Daemon not running", which reads as an unresolved name while proving
      # nothing about the LAN. The other household machine speaks mDNS; stating that here makes
      # the dependency visible, as the install-macvtap client does, rather than inherited from
      # whatever the product ships.
      services.avahi = { enable = true; nssmdns4 = true; };
    };
in
pkgs.testers.runNixOSTest {
  name = "mosquitto";

  nodes = {
    node1 = node;
    node2 = client;
  };

  testScript = ''
    ${h.fixtureHelpers}
    VIP = "192.168.1.100"

    start_all()
    for m in [node1, node2]:
        m.wait_for_unit("multi-user.target")
        m.wait_for_unit("briard-test-fixture-install.service", timeout=600)
        # Named before the reactor promotes, as on a real node (the agent mints the flock name at
        # bring-up, the reverse proxy publishes it on promotion). Naming afterwards still reaches
        # the wire (the reverse proxy re-reads the name on its tick) but tests a path no real
        # node takes.
        name_the_flock(m)
        m.succeed("modprobe drbd")
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")

    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 1")
    for m in [node1, node2]:
        m.succeed("systemctl start drbd-reactor.service")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    node1.wait_until_succeeds(f"curl -fsS http://{VIP}/healthz", timeout=300)
    primary = next(m for m in [node1, node2] if role(m) == "Primary")
    other = node2 if primary == node1 else node1
    print(f"primary={primary.name} client={other.name}")
    dataroot = install_fixture(primary)

    # The units the renderer produced must be running, never a list restated here.
    for unit in fixture_units(primary):
        primary.wait_for_unit(unit, timeout=180)

    # (1) The config is in effect: the bind the product adds (agent/mosquitto) answers on the port
    # only our config opens. The image config would leave 9883 open on every interface, and no
    # config would leave no management listener for the health floor to probe.
    #
    # ⚠️ Reached at the pod address, not the guest loopback: on a private network 127.0.0.1 here
    # is the guest namespace, without the broker. The address comes from the routing table, where
    # the product health probe resolves it, so the test cannot pass on an address nothing else
    # uses.
    import json as _mj
    primary.succeed("test -f /run/briard/mosquitto/mosquitto.conf")
    table = _mj.loads(primary.succeed("cat /run/briard/routes.json"))
    broker = next(s for s in table["apps"] if s["name"] == "mosquitto")
    pod = broker["address"]
    assert pod.startswith("10.12."), f"the broker is not on the pod pool: {broker}"
    mgmt = f"http://{pod}:9883/api/v1/listeners"
    primary.wait_until_succeeds(f"curl -fsS {mgmt}", timeout=120)
    # The broker reports its own MQTT listener; not a static file, so a 200 is the process.
    primary.succeed(f"curl -fsS {mgmt} | grep -q '\"port\":.*1883'")

    # (2) Not on the LAN, from the other machine at the household address: this fails if the
    # config is simplified back to the image one. Second guard: the management port is not in the
    # manifest `ports`, so nothing publishes it out of the pod.
    other.fail(f"curl -fsS --max-time 5 http://{VIP}:9883/api/v1/listeners")
    other.fail(f"curl -fsS --max-time 5 http://{primary.name}:9883/api/v1/listeners")
    # Nor the pod address from off the box: the pool is guest-internal, no host routes to it.
    other.fail(f"curl -fsS --max-time 5 {mgmt}")

    # (3) MQTT is: the off-box client publishes and reads back a retained message, the control
    # for (2), which an unreachable broker would satisfy vacuously.
    other.succeed(f"mosquitto_pub -h {VIP} -t briard/test -m 'before-restart' -r")
    got = other.succeed(f"mosquitto_sub -h {VIP} -t briard/test -C 1 -W 10").strip()
    assert got == "before-restart", f"the broker did not return the retained message: {got!r}"

    # (4) It keeps what it was told to keep, across a clean bounce and a crash. The bounce has to
    # be the product sequence: `systemctl restart` of a container unit does not work in a pod
    # (measured on this rig: stopping the container takes the pod down, and the start then fails
    # with `Bound to unit briard-mosquitto-pod.service, but unit isn't active`). So units stop
    # containers-first and start pod-first, as converge does (agent/guestagent), never a restart.
    units = fixture_units(primary)
    containers = [u for u in units if not u.endswith("-pod.service")]
    assert len(containers) == 1, f"expected one container unit, got {containers}"
    for u in reversed(containers):
        primary.succeed(f"systemctl stop {u}")
    for u in units:
        primary.succeed(f"systemctl start {u}")
    primary.wait_until_succeeds(f"curl -fsS -o /dev/null {mgmt}", timeout=120)
    got = other.succeed(f"mosquitto_sub -h {VIP} -t briard/test -C 1 -W 10").strip()
    assert got == "before-restart", f"the retained message did not survive a bounce: {got!r}"

    # The file is on the replicated subvolume, not the container layer: state that fails over
    # rather than dies with the node. mosquitto writes it on a clean stop, so it holds the publish.
    primary.succeed(f"test -s {dataroot}/${fixture.dataDir}/mosquitto.db")

    # The crash, the actual household failure mode and a claim converge-at-promotion rests on: app
    # units are not promoter chain members, so their own `Restart=always` is the whole recovery.
    # SIGKILL is the honest test; the message is on disk from the clean stop, so what comes back
    # answers from the replicated volume, not memory.
    primary.succeed(f"systemctl kill --signal=SIGKILL {containers[0]}")
    primary.wait_until_succeeds(f"systemctl is-active {containers[0]}", timeout=180)
    primary.wait_until_succeeds(f"curl -fsS -o /dev/null {mgmt}", timeout=180)
    got = other.succeed(f"mosquitto_sub -h {VIP} -t briard/test -C 1 -W 10").strip()
    assert got == "before-restart", f"the broker did not recover its state after a crash: {got!r}"

    # No demote for it: a crashed app alerts, it never fails the household over.
    assert role(primary) == "Primary", "a crashed container demoted the node"
    primary.succeed(f"curl -fsS -o /dev/null http://{VIP}/healthz")

    # (5) The household devices can find it. The broker is the one catalogued app whose clients
    # are appliances: Tasmota- and ESPHome-class firmware browses `_mqtt._tcp` and never types a
    # name. The mosquitto image advertises nothing (measured: no mDNS code at all), so the record
    # comes from the reverse proxy, off the table it routes on: one map, so a record pointing where
    # the reverse proxy does not route is not expressible. Checked from the other machine again,
    # since a record nobody on the LAN hears is no announcement. Every expected value is read from
    # the node (routed_host, and the announcement written beside it), never composed here, so a
    # wrong decision fails rather than being restated.
    host = routed_host(primary, "mosquitto")
    ann = next(s for s in _mj.loads(primary.succeed("cat /run/briard/routes.json"))["apps"]
               if s["name"] == "mosquitto")["announce"][0]
    assert ann["port"] == 1883, f"the node announces port {ann['port']}, not MQTT's"

    # (5a) The SRV target resolves on the LAN, over mDNS: a per-app name reached with
    # `curl -H 'Host: …'` never touches a resolver, so a name resolving nowhere would pass that.
    other.wait_until_succeeds(f"avahi-resolve -n {host} | grep -q {VIP}", timeout=120)

    # (5b) The service record is there, under the type and instance the node chose.
    seen = f"';{ann['name']};{ann['type']};'"
    other.wait_until_succeeds(f"avahi-browse -atp | grep -q {seen}", timeout=60)

    # (5c) The announced endpoint serves, from off the box, on the port the node announced: an
    # announcement carrying the pod-internal management port (9883, not MQTT) fails here.
    got = other.succeed(f"mosquitto_sub -h {VIP} -p {ann['port']} -t briard/test -C 1 -W 10").strip()
    assert got == "before-restart", f"the announced endpoint did not serve MQTT: {got!r}"
    print(f"a browsing device finds {ann['name']} under {ann['type']} at {host} -> {VIP}:{ann['port']}")

    # ⚠️ Deliberately not asserted, so nobody adds it back and spends the day: the SRV target/port
    # fields as read by `avahi-browse -r`. They are correct (observed resolving to
    # `briard-brave-elf-mosquitto.local / 192.168.1.100 / 1883`), but the avahi service-record
    # resolver does not fetch the SRV target address itself, so `-r` returns "Failed to resolve …
    # Timeout reached" indefinitely (measured: 120s of short browses, one 90s browse, on the
    # client and on the publishing node, with both records established and the name resolvable
    # there). Resolving the host first fixes it only sometimes (180s of exactly that sequence). It
    # is the avahi client, not our records, and the three claims above cover what a household
    # needs: the name and the service instance are on the LAN, and the endpoint serves.
  '';
}
