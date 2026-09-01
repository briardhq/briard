# The broker as a catalogued service: it installs the shipped way, runs on Briard's config rather
# than its image's, and keeps what it was told to keep ([V3b.4]).
#
# WHAT THIS EXISTS TO CATCH, and none of it is visible to a test that only asks "is it up".
# mosquitto's own image ships with persistence OFF and its management API on every interface, so
# a broker that starts is not evidence of anything a household needs. The product replaces that
# config from `agent/services`, and every claim in this file is about the difference:
#
#   1. the config is IN EFFECT (not merely rendered) -- the API answers on the guest's loopback;
#   2. the API is NOT on the LAN -- asserted from ANOTHER MACHINE, which is the only place the
#      claim can be made honestly ([[verification-assertions-must-fail]]: observe from outside);
#   3. MQTT itself IS on the LAN -- from that same off-box client, because a broker only nobody
#      can reach would pass (1) and (2) perfectly;
#   4. a retained message SURVIVES a restart of the container, and lands in the file on the
#      REPLICATED subvolume -- which is what makes the broker a service with data at all, and
#      what the failover half of [V3b.4] then carries across a node;
#   5. the broker is FINDABLE -- it answers a `_mqtt._tcp` browse from that same off-box client,
#      under the flock's name and on the port MQTT actually listens on, and the browse result is
#      then connected to. An image that advertises nothing is invisible to every appliance in the
#      house, which is a broker nobody without our documentation can use.
#
# Two nodes for one broker: node2 is the off-box client. It holds no service and never promotes
# -- it is the outside from which (2), (3) and (5) are observed.
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
  # The client is the OUTSIDE. It is the same mosquitto build the image would have, but that is
  # incidental: what matters is that the publish, the subscribe and the browse all happen from a
  # machine that is not the one running the broker.
  client =
    { ... }:
    {
      imports = [ node ];
      # avahi-browse as well: this node is also where the broker's own announcement is OBSERVED,
      # and observing it from the machine that publishes it would prove nothing.
      environment.systemPackages = [ pkgs.mosquitto pkgs.avahi ];
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
        # Named BEFORE the reactor promotes, which is the order a real node runs in: the agent
        # mints the flock name at bring-up, and the mDNS publishers are pulled up by briard-vip.
        # Naming afterwards would leave briard-mdns-services inactive on its ConditionPathExists.
        name_the_flock(m)
        m.succeed("modprobe drbd")
        m.succeed("drbdadm create-md --force r0")
        m.succeed("systemctl start drbd@r0.target")

    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 1")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    # Format the fresh volume the way the INSTALLER now does ([B.126]): the product stopped
    # formatting on the promotion path, so a harness that seeds a resource by hand states it.
    node1.succeed("drbdadm primary r0 && mkfs.btrfs -f $(drbdadm sh-dev r0/0) && drbdadm secondary r0")
    for m in [node1, node2]:
        m.succeed("systemctl start drbd-reactor.service")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    node1.wait_until_succeeds(f"curl -fsS http://{VIP}/healthz", timeout=300)
    primary = next(m for m in [node1, node2] if role(m) == "Primary")
    other = node2 if primary == node1 else node1
    print(f"primary={primary.name} client={other.name}")
    dataroot = install_fixture(primary)

    # The units the RENDERER produced are the ones that must be running -- never a list this test
    # restates.
    for unit in fixture_units(primary):
        primary.wait_for_unit(unit, timeout=180)

    # (1) THE CONFIG IS IN EFFECT. The bind is what the product added (agent/mosquitto), and the
    # proof that it took is the broker answering on the port only OUR config opens: the image's
    # own config would leave 9883 open to every interface, and no config at all would leave the
    # broker with no management listener for the health floor to probe.
    #
    # ⚠️ REACHED AT THE POD'S ADDRESS, NOT THE GUEST'S LOOPBACK ([B.48](a)). The broker runs on a
    # PRIVATE network now, so 127.0.0.1 in here is the guest's namespace and the broker is not in
    # it. The address comes from the routing table -- the same place the product's own health probe
    # resolves it -- so this test cannot pass against an address nothing else uses.
    import json as _mj
    primary.succeed("test -f /run/briard/mosquitto/mosquitto.conf")
    table = _mj.loads(primary.succeed("cat /run/briard/routes.json"))
    broker = next(s for s in table["services"] if s["name"] == "mosquitto")
    pod = broker["address"]
    assert pod.startswith("10.12."), f"the broker is not on the pod pool: {broker}"
    mgmt = f"http://{pod}:9883/api/v1/listeners"
    primary.wait_until_succeeds(f"curl -fsS {mgmt}", timeout=120)
    # The broker reports its own MQTT listener open -- the health path is not a static file, so a
    # 200 here is the process answering rather than a directory being served.
    primary.succeed(f"curl -fsS {mgmt} | grep -q '\"port\":.*1883'")

    # (2) AND IT IS NOT ON THE LAN. From the other machine, to the address the household uses.
    # This is the assertion that fails if anyone simplifies the config back to the image's -- and
    # it now has a second guard behind it: the management port is not in the manifest's `ports`, so
    # nothing publishes it out of the pod at all.
    other.fail(f"curl -fsS --max-time 5 http://{VIP}:9883/api/v1/listeners")
    other.fail(f"curl -fsS --max-time 5 http://{primary.name}:9883/api/v1/listeners")
    # Nor is the pod's own address reachable from off the box: the pod pool is guest-internal, and
    # no host anywhere holds a route to it.
    other.fail(f"curl -fsS --max-time 5 {mgmt}")

    # (3) MQTT IS. An off-box client publishes and reads its own retained message back -- which is
    # also the control for (2): a broker nothing could reach would satisfy (2) vacuously.
    other.succeed(f"mosquitto_pub -h {VIP} -t briard/test -m 'before-restart' -r")
    got = other.succeed(f"mosquitto_sub -h {VIP} -t briard/test -C 1 -W 10").strip()
    assert got == "before-restart", f"the broker did not return the retained message: {got!r}"

    # (4) IT KEEPS WHAT IT WAS TOLD TO KEEP -- across a clean bounce and across a crash.
    #
    # THE BOUNCE IS THE PRODUCT'S OWN SEQUENCE, and it has to be: `systemctl restart` on a
    # container unit does not work at all in a pod, MEASURED here on the first run of this test.
    # Stopping the container takes the POD down with it, and the start half then fails with
    # `Bound to unit briard-mosquitto-pod.service, but unit isn't active`. So the units are
    # stopped containers-first and started pod-first -- exactly what converge does
    # (agent/guestagent) and the reason it never spells this as a restart.
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

    # And the file is on the REPLICATED subvolume, not in the container's own layer -- the
    # difference between a broker whose state fails over and one whose state is lost with the node.
    # mosquitto writes it on a clean stop, so by here it exists and holds what was published.
    primary.succeed(f"test -s {dataroot}/broker/mosquitto.db")

    # THE CRASH, which is the household's actual failure mode and a claim [V3b.3](f) rests on:
    # service units are NOT promoter chain members, so nothing outside the unit brings a dead
    # container back -- its own `Restart=always` is the whole recovery. A SIGKILL is the honest
    # test of that, and the retained message is already on disk from the clean stop above, so what
    # comes back is answering from the replicated volume rather than from memory.
    primary.succeed(f"systemctl kill --signal=SIGKILL {containers[0]}")
    primary.wait_until_succeeds(f"systemctl is-active {containers[0]}", timeout=180)
    primary.wait_until_succeeds(f"curl -fsS -o /dev/null {mgmt}", timeout=180)
    got = other.succeed(f"mosquitto_sub -h {VIP} -t briard/test -C 1 -W 10").strip()
    assert got == "before-restart", f"the broker did not recover its state after a crash: {got!r}"

    # The node did NOT demote for it: a crashed service alerts, it never fails the household over
    # ([V3b.3](f)). The primary is still primary and the front door still answers.
    assert role(primary) == "Primary", "a crashed container demoted the node"
    primary.succeed(f"curl -fsS -o /dev/null http://{VIP}/healthz")

    # (5) AND THE HOUSEHOLD'S OTHER DEVICES CAN FIND IT ([V3b.30](a)). The broker is the one
    # catalogued service whose clients are appliances rather than people: Tasmota- and
    # ESPHome-class firmware browses `_mqtt._tcp` and never types a name. mosquitto's own image
    # advertises nothing (measured -- there is no mDNS code in it at all), so the record comes
    # from the guest's avahi, off the same routing table the front door reads.
    #
    # FROM THE OTHER MACHINE AGAIN, because that is the only place the claim means anything: a
    # record published to a LAN nobody hears is not an announcement.
    #
    # EVERY EXPECTED VALUE IS READ FROM THE NODE, never composed here: the name from the routing
    # table (routed_host) and the announcement the product itself wrote beside it. So this asserts
    # what the node decided, and a wrong decision fails rather than being restated.
    host = routed_host(primary, "mosquitto")
    ann = next(s for s in _mj.loads(primary.succeed("cat /run/briard/routes.json"))["services"]
               if s["name"] == "mosquitto")["announce"][0]
    assert ann["port"] == 1883, f"the node announces port {ann['port']}, not MQTT's"

    # (5a) THE SRV TARGET RESOLVES ON THE LAN. Also the first assertion anywhere to resolve a
    # [B.48] per-service name over mDNS at all: every earlier one reached those names with
    # `curl -H 'Host: …'`, which never touches a resolver, so a name that resolved nowhere would
    # have passed all of them.
    other.wait_until_succeeds(f"avahi-resolve -n {host} | grep -q {VIP}", timeout=120)

    # (5b) AND THE SERVICE RECORD IS THERE, under the type and instance the node chose.
    seen = f"';{ann['name']};{ann['type']};'"
    other.wait_until_succeeds(f"avahi-browse -atp | grep -q {seen}", timeout=60)

    # (5c) AND THE ANNOUNCED ENDPOINT SERVES, from off the box. The port is the one the NODE
    # announced, not one restated here -- so an announcement carrying the pod-internal management
    # port fails at this line, because 9883 does not speak MQTT.
    got = other.succeed(f"mosquitto_sub -h {VIP} -p {ann['port']} -t briard/test -C 1 -W 10").strip()
    assert got == "before-restart", f"the announced endpoint did not serve MQTT: {got!r}"
    print(f"a browsing device finds {ann['name']} under {ann['type']} at {host} -> {VIP}:{ann['port']}")

    # ⚠️ WHAT THIS DELIBERATELY DOES NOT ASSERT, and why, so nobody adds it back and spends the
    # day: the SRV record's own target/port fields as read by `avahi-browse -r`. Those fields ARE
    # correct -- observed resolving to `briard-brave-elf-mosquitto.local / 192.168.1.100 / 1883`
    # in runs 33445936181, 33448573206 and 33449242156 -- but avahi's SERVICE resolver cannot be
    # made to produce them on demand. MEASURED: it does not fetch the SRV target's address itself,
    # so `-r` returns "Failed to resolve … Timeout reached" indefinitely -- through 120s of
    # repeated short browses, a single 90s browse, on the off-box client AND on the node that
    # publishes the records, with both records established and the name resolvable on that very
    # box. Resolving the host first fixes it sometimes (33449242156) and not others (33475931135,
    # 180s of exactly that sequence). It is avahi's client, not our records, and the three claims
    # above cover what a household actually needs: the name is on the LAN, the service is on the
    # LAN under its type, and the endpoint serves.
  '';
}
