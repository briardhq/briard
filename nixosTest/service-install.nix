# INSTALLING A SERVICE AT RUNTIME, on the shape a stranger actually installs.
#
# A shipped node boots with NO service: it mounts, promotes, and serves a landing page
# at the VIP. This test takes that node and puts a service on it the way `briard service install`
# does, then asserts the node ends up serving it — install -> promote -> serve, which is the
# integration bar for the free tier (failover of a runtime-installed service is a flock concern,
# not v3's: on briard free each node is an isolated island).
#
# WHAT IS REAL HERE, because the value of this test is entirely in what it does NOT stub:
#   - a real OCI registry over TLS, and a real DIGEST-PINNED `podman pull` from it (decided registries; a plain-HTTP registry is not an option — containers/image refuses HTTP for every
#     address including localhost, which a probe confirmed, so the test runs a real CA);
#   - the REAL renderer — and since [V3b.3](f) it runs where the product runs it, inside
#     `briard-agent --converge`, off the manifest this test puts on the replicated volume. Nothing
#     copies rendered units in from the side any more;
#   - real quadlet generation (files under /run/containers/systemd becoming units at daemon-reload),
#     real podman, real drbd-reactor driving the static promoter chain, and the real front door;
#   - the node promoting with ZERO services first, then with one — the shipped state, then the
#     installed one, so the install's effect is a change and not a coincidence.
#
# WHAT IS NOT COVERED, named so a green run is not read as more than it is:
#   - the host agent's ORCHESTRATION (fetch/verify/warm/health-gate/revert) cannot run in this
#     harness: it drives the guest over virtio-serial, and here the node IS the guest with no host
#     on the other end. That sequence is unit-tested in agent/host/service_test.go, which is where
#     its bugs have actually surfaced. What the test does by hand is the Primary-only half an
#     install performs on the volume, and then it hands over to converge.
#
# WITH broken = true, it runs the happy path and then keeps going: it UPGRADES the service
# to a manifest whose container never becomes healthy (the dummy's BRIARD_BROKEN mode), proves the
# health gate WOULD trip (the container stays active but its own endpoint stays 503, and the data
# is poisoned), proves the node STAYS PROMOTED through it (a service failure alerts, it does not
# demote — [V3b.3](f)'s failure rule), and then ROLLS BACK to the prior service with its data
# intact — the service-level twin of the {code+data} OS rollback. The host agent's orchestration of
# that sequence is unit-tested in agent/host/service_test.go (it drives the guest over
# virtio-serial, absent here); this proves the REAL mechanisms it relies on — a real broken
# container, a real btrfs snapshot/restore, a real converge re-render, and a real recovery.
{ pkgs, guestModule, quadletRender, broken ? false }:
let
  h = import ./lib.nix { inherit pkgs guestModule; };

  registryIP = "10.0.0.99";
  registryPort = "5000";
  registryHost = "${registryIP}:${registryPort}";

  # A CA + server cert for the registry, minted at build time. The guest nodes trust the CA, so
  # the pull is a REAL TLS pull rather than a verification bypass — `--tls-verify=false` anywhere
  # in this test would quietly delete the thing it is meant to prove.
  certs = pkgs.runCommand "registry-certs" { nativeBuildInputs = [ pkgs.openssl ]; } ''
    mkdir -p $out
    openssl req -x509 -newkey rsa:2048 -nodes -days 36500 \
      -subj "/CN=briard-test-ca" \
      -keyout $out/ca.key -out $out/ca.crt 2>/dev/null
    openssl req -newkey rsa:2048 -nodes \
      -subj "/CN=${registryIP}" \
      -keyout $out/server.key -out $out/server.csr 2>/dev/null
    printf 'subjectAltName=IP:${registryIP}\n' > $out/ext
    openssl x509 -req -in $out/server.csr -CA $out/ca.crt -CAkey $out/ca.key \
      -CAcreateserial -days 36500 -extfile $out/ext -out $out/server.crt 2>/dev/null
    chmod 0644 $out/server.key
  '';

  # The service's service: the existing dummy-service, which serves /healthz and ticks a counter
  # into its data dir. Reused rather than invented — it is already the fixture every other test
  # trusts, and its tick is what proves the mounted subvolume is really the service's.
  fixtureImage = pkgs.dockerTools.buildImage {
    name = "briard-fixture";
    tag = "v0";
    config.Cmd = [ "${pkgs.dummy-service}/bin/dummy-service" ];
  };
  # The fixture's data path is a CONST in its source, not configurable — so the container's mount
  # point is dictated by the fixture, while the host-side directory is the renderer's to choose.
  # That asymmetry is the normal case for a catalogued service (upstream decides where its data
  # lives inside the container), which makes it the right shape to exercise.
  fixtureMount = "/var/lib/briard/dummy";

  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  # Promoter = false: the test writes the snippet itself, at the moment it chooses, so that the
  # node is seen promoting with ZERO services before the install and with one after. The chain it
  # writes is the STATIC production one ([V3b.3](f)) — it is no longer derived from a rendering,
  # by anyone.
  node =
    { config, ... }:
    {
      imports = [ (h.mkNode { inherit resource; promoter = false; }) ];
      # The guest trusts the test CA. Free in this harness — the node is a module composition, not
      # a prebuilt disk — which is exactly why this test is hermetic rather than nested.
      security.pki.certificateFiles = [ "${certs}/ca.crt" ];
      # test-only: in the product these run from unit PATHs, not a shell -- btrfs from the guest
      # agent unit's, converge from briard-services'. This test drives the install path by hand,
      # so it needs all three where a test command can reach them.
      environment.systemPackages = [ quadletRender pkgs.btrfs-progs config.briard.agentPackage ];
    };
  witnessNode = h.mkNode {
    inherit resource;
    diskless = true;
    promoter = false;
  };
in
pkgs.testers.runNixOSTest {
  name = "service-install${pkgs.lib.optionalString broken "-broken"}";
  skipTypeCheck = true;

  nodes = {
    node1 = node;
    node2 = node;
    witness = witnessNode;
    # Named to sort AFTER "witness": the framework assigns node numbers in alphabetical order
    # and lib.nix pins each DRBD peer at 10.0.0.<id+1>, so a node inserted alphabetically before
    # the witness silently shifts its address and DRBD never connects.
    zregistry =
      { ... }:
      {
        networking.interfaces.eth1.ipv4.addresses = [
          {
            address = registryIP;
            prefixLength = 24;
          }
        ];
        # NixOS enables the firewall by default and services.dockerRegistry does not open its own
        # port, so without this the nodes get a plain i/o timeout that reads like a TLS problem.
        networking.firewall.allowedTCPPorts = [ 5000 ];
        services.dockerRegistry = {
          enable = true;
          listenAddress = "0.0.0.0";
          port = 5000;
          extraConfig.http.tls = {
            certificate = "${certs}/server.crt";
            key = "${certs}/server.key";
          };
        };
        # The pusher trusts the same CA, so nothing in this test ever passes --tls-verify=false:
        # a bypass anywhere would quietly delete the property the registry is here to prove.
        security.pki.certificateFiles = [ "${certs}/ca.crt" ];
        # Brings /etc/containers/policy.json, which skopeo requires before it will copy anything.
        # (Preferred over `skopeo --insecure-policy`, whose name reads like a TLS bypass and would
        # make a reader think this test skips verification. It does not.)
        virtualisation.containers.enable = true;
        environment.systemPackages = [ pkgs.skopeo ];
      };
  };

  testScript = ''
    disk_nodes = [node1, node2]
    start_all()
    for m in [node1, node2, witness, zregistry]:
        m.wait_for_unit("multi-user.target")

    # === 1. publish the fixture into the registry, and learn its DIGEST ===
    # The manifest schema accepts only digest-pinned images (a tag is mutable, so a tagged
    # manifest would make the service identity a promise someone else could change), so the
    # digest is not a detail here — it IS the reference the whole install hangs off.
    zregistry.wait_for_open_port(5000)
    zregistry.succeed(
        "skopeo copy docker-archive:${fixtureImage} docker://${registryHost}/briard-fixture:v0"
    )
    digest = zregistry.succeed(
        "skopeo inspect docker://${registryHost}/briard-fixture:v0 --format '{{.Digest}}'"
    ).strip()
    print(f"fixture published at ${registryHost}/briard-fixture@{digest}")

    # === 2. the service manifest, and the REAL renderer ===
    manifest = (
        '{"name":"fixture","version":"v0","network":"host","containers":[{'
        f'"name":"app","image":"${registryHost}/briard-fixture@{digest}",'
        '"mount":"${fixtureMount}","primary":true,"port":8080,"healthPath":"/healthz"}]}'
    )
    for m in disk_nodes:
        m.succeed(f"cat > /tmp/manifest.json <<'EOF'\n{manifest}\nEOF")
        print(m.succeed("quadlet-render /tmp/manifest.json /tmp/rendered"))

    # The renderer's output is what the rest of this test runs on. Show it once — a rendered unit
    # is the artifact a human debugs when an install misbehaves.
    print(node1.succeed("cat /tmp/rendered/briard-fixture.pod /tmp/rendered/briard-fixture-app.container /tmp/rendered/briard-fixture-app.image"))

    # === 3. warm the image on EVERY node ===
    # A REAL pull over TLS from the registry, digest-pinned — the shape the install path uses, and
    # the half of an install that legitimately needs the network. Running it on every node is what
    # makes the failover path able to promise it never will.
    #
    # The rendered units are NOT copied anywhere here any more ([V3b.3](f)): converge writes them
    # itself, from the manifest on the volume, on whichever node promotes. Copying them in from
    # the side would put this harness back in the business of standing in for the product.
    for m in disk_nodes:
        m.succeed("mkdir -p /run/containers/systemd")
        m.succeed("cp /tmp/rendered/briard-fixture-app.image /run/containers/systemd/ && systemctl daemon-reload")
        for unit in m.succeed("cat /tmp/rendered/images").split():
            m.succeed(f"systemctl start {unit}")
        # The pull really happened, and the image is addressable by the digest the manifest pins.
        m.succeed(f"podman image exists ${registryHost}/briard-fixture@{digest}")
    print("images warmed on every node by a real digest-pinned TLS pull")

    # NOTHING IS RUNNING YET, and nothing generated itself: the .image units are boot-time, the
    # pod and container are not, and no node has been given the manifest.
    node1.fail("systemctl cat briard-fixture-app.service")
    print("the image is resident and the service does not exist — the state before an install")

    # === 4. bring DRBD up, and give the promoter the RENDERED chain ===
    for m in [node1, node2, witness]:
        m.succeed("modprobe drbd")
    for m in disk_nodes:
        m.succeed("drbdadm create-md --force r0")
    for m in [node1, node2, witness]:
        m.succeed("systemctl start drbd@r0.target")
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    # Arm the one-time format the way BRING-UP does ([B.126]): the product no longer formats on
    # the promotion path, so a harness that seeds a resource by hand leaves the same marker.
    node1.succeed("mkdir -p /run/briard && touch /run/briard/data.format")

    # THE CHAIN IS STATIC, and no longer derived from the rendering at all ([V3b.3](f)). The chain
    # is what drbd-reactor promotes WITH, but the volume it must converge to is only readable
    # AFTER promotion — so the start-list cannot name the services, and briard-services is what
    # renders and starts them once the mount exists.
    #
    # TAKEN FROM lib.nix rather than restated ([B.125]). This used to be a hand-written list
    # annotated "exactly what host.promoterUnits writes in production", which is the kind of claim
    # that stops being true silently: when the front door and the mDNS publishers became chain
    # MEMBERS, this copy still had three units, so the door never started here at all and the
    # service was unreachable by name — the failure this comment is now attached to.
    snippet = """${h.promoterSnippet}"""
    for m in disk_nodes:
        m.succeed(f"install -D /dev/stdin /run/briard/drbd-reactor.d/briard.toml <<'SNIP'\n{snippet}SNIP")
    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")

    # === 5. THE BAR: the node serves the installed service at the VIP ===
    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    node1.wait_until_succeeds("drbdadm role r0 | grep -qE 'Primary|Secondary'", timeout=90)
    primary = next(m for m in disk_nodes if role(m) == "Primary")
    print(f"primary={primary.name}")

    # IT PROMOTED WITH ZERO SERVICES FIRST, which is the shipped state of every node a stranger
    # installs ([V3.15]): briard-services converged to nothing, the VIP came up, and the front
    # door answers. Asserted before the install so the install's effect is a change and not a
    # coincidence.
    primary.wait_until_succeeds("test $(systemctl is-active briard-services.service) = active", timeout=120)
    primary.fail("systemctl cat briard-fixture-app.service")

    # === 5b. the install's Primary-only half, by hand ===
    # The service's data subvolume AND its manifest live on the replicated volume, so only the
    # primary can write them — this is what the install path's Primary-only provision step does.
    # ONE subvolume with plain per-container subdirectories: `btrfs subvolume delete` refuses on a
    # subvolume that contains nested subvolumes, so data.restore would break outright.
    dataroot = node1.succeed("cat /tmp/rendered/dataroot").strip()
    primary.succeed(f"btrfs subvolume show {dataroot} >/dev/null 2>&1 || btrfs subvolume create {dataroot}")
    primary.succeed(f"mkdir -p {dataroot}/app")
    # The MANIFEST is the service's identity, and putting it on the volume is what an install
    # actually does. Everything after this point is the product's own code.
    primary.succeed("mkdir -p /var/lib/briard/.services")
    primary.succeed("cp /tmp/manifest.json /var/lib/briard/.services/fixture.json && sync")
    primary.succeed("briard-agent --converge")

    # CONVERGE rendered the units and quadlet generated them — at RUNTIME, from the volume, with
    # nothing copied in from the side. That is the whole mechanism, asserted after the fact rather
    # than arranged beforehand.
    primary.succeed("systemctl cat briard-fixture-app.service >/dev/null")
    primary.succeed("systemctl cat briard-fixture-pod.service >/dev/null")

    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-pod.service) = active", timeout=120)
    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-app.service) = active", timeout=120)
    print(primary.succeed("podman ps --format '{{.Names}} {{.Pod}} {{.Status}}'"))

    # The service answers on its OWN endpoint (host-networked :8080) — the same thing the health
    # gate probes (agent/host/service.go awaitHealthy, which now asks the guest to resolve it
    # rather than assembling this URL itself), and a REAL 200 from the service, not the front
    # door's own answer.
    primary.wait_until_succeeds("curl -fsS http://127.0.0.1:8080/healthz", timeout=120)
    assert "ok" in primary.succeed("curl -fsS http://127.0.0.1:8080/healthz"), "service /healthz did not report ready"
    print("the installed service answers healthy on its own endpoint — install -> promote -> serve")

    # === 5c. AND THROUGH THE FRONT DOOR, under its own name ([B.48]) ===
    # The install above happened on a node with no flock name, so it was routed and UNNAMED --
    # which is a real shipped state (a node publishes nothing rather than a guess) and the one this
    # asserts first: the table carries the service, with no host to reach it by.
    import json
    table = json.loads(primary.succeed("cat /run/briard/routes.json"))
    assert [s["name"] for s in table["services"]] == ["fixture"], f"routing table = {table}"
    assert not table["services"][0].get("hosts"), f"an unnamed node composed a name: {table}"
    assert table["services"][0]["address"] == "127.0.0.1", f"routing table = {table}"
    assert table["services"][0]["routes"] == [{"listen": "name", "to": "http://:8080"}], f"routing table = {table}"

    # Name the flock the way the agent does, re-converge, and the SAME service acquires a name.
    # This is the sequence a real node runs in the other order; doing it this way is what proves
    # the name is composed from the flock name rather than baked at install.
    primary.succeed("mkdir -p /run/briard && printf 'FLOCK_NAME=brave-elf\\n' >/run/briard/mdns.env")
    primary.succeed("briard-agent --converge")
    host = json.loads(primary.succeed("cat /run/briard/routes.json"))["services"][0]["hosts"][0]
    assert host == "briard-brave-elf-fixture.local", f"the node composed {host}"
    body = primary.succeed(f"curl -fsS -H 'Host: {host}' http://192.168.1.100/healthz")
    assert "ok" in body and "front door" not in body, (
        f"the VIP under the service's name answered {body!r}; want the SERVICE's own answer"
    )
    # The bare address is still the node's, and it names what it routes.
    node_health = primary.succeed("curl -fsS http://192.168.1.100/healthz")
    assert "1 service(s) routed" in node_health and "fixture" in node_health, f"node /healthz = {node_health!r}"
    # A name nobody serves is the node's page, not a service's 404: that is where a household
    # reads which names this node does answer to.
    page = primary.succeed("curl -fsS -H 'Host: briard-brave-elf-nope.local' http://192.168.1.100/")
    assert "fixture" in page, f"the front door's page does not list what it serves: {page!r}"

    # And the service's data really lands on the replicated subvolume.
    primary.wait_until_succeeds(f"test -s {dataroot}/app/state.json", timeout=60)
    print(primary.succeed(f"cat {dataroot}/app/state.json"))
    print("service state is on the DRBD subvolume — install -> promote -> serve, on the shipped node")

    ${pkgs.lib.optionalString (!broken) ''
    # --- THE RENDERED UNITS ARE VOLATILE, AND CONVERGE PUTS THEM BACK BY ITSELF.
    #
    #     Happy-path run only: this reboots the node, so it cannot precede the broken-upgrade half,
    #     which carries on from here against a live service.
    #
    #     /run/containers/systemd is tmpfs, chosen deliberately: the volume holds the MANIFEST and
    #     each node renders its own units from it, so there is one identity and no second durable
    #     copy to drift from it. The consequence is that a reboot erases every rendered unit.
    #
    #     ⚠️ THIS TEST'S CONCLUSION REVERSED WITH [V3b.3](f). It used to assert the premise and
    #     stop there — "after a reboot the service is gone and stays gone until something
    #     re-renders, which is why the HOST must, from its node-local cache at bring-up". Converge
    #     makes that cure unnecessary and the claim false: briard-services is a promoter chain
    #     member, so the node re-renders from the VOLUME the moment it promotes, with no host in
    #     the loop at all. That is the same mechanism that fixes the survivor case, seen from the
    #     reboot side.
    #
    #     Asserted as a sequence, so no step can pass vacuously: the unit exists, the reboot
    #     really erased it, and it comes back on its own.
    primary.succeed("systemctl cat briard-fixture-app.service")
    primary.shutdown()
    primary.start()
    primary.wait_for_unit("multi-user.target")
    left = primary.succeed("ls -A /run/containers/systemd 2>/dev/null || true").strip()
    assert left == "", f"expected the rendered units to be gone after a reboot, found: {left!r}"
    primary.fail("systemctl cat briard-fixture-app.service")
    print("the reboot really erased the rendered units")

    # Re-arm the promoter on the returning node (in the product the agent does this at bring-up;
    # here the harness does, as it did the first time). DRBD does not auto-promote, so the reactor
    # has to be running before any node can take the role — starting it is what re-enters the race.
    primary.succeed("modprobe drbd")
    primary.succeed("systemctl start drbd@r0.target")
    primary.succeed("systemctl start drbd-reactor.service")

    # WHICHEVER node holds the volume must be serving the fixture again, and it does not matter
    # which — that indifference IS the property ([V3b.3](f)). The peer very likely took over while
    # this node was down, in which case it is serving a service it never installed, having read
    # the manifest off the volume and rendered for itself. If the returning node takes its role
    # back instead, it re-rendered from the same volume after a reboot wiped its tmpfs. Both are
    # converge, which is why the assertion refuses to name a node.
    for m in disk_nodes:
        m.wait_until_succeeds("drbdadm role r0 | grep -qE 'Primary|Secondary'", timeout=120)
    now = next(m for m in disk_nodes if role(m) == "Primary")
    print(f"after the reboot the volume is held by {now.name} (it was {primary.name})")
    now.wait_until_succeeds("test $(systemctl is-active briard-fixture-app.service) = active", timeout=180)
    now.wait_until_succeeds("curl -fsS http://127.0.0.1:8080/healthz", timeout=120)
    print("the node holding the volume rendered from it and served — converge, with no host involved")
    ''}
    ${pkgs.lib.optionalString broken ''

    # ============================================================================================
    # THE FAILURE HALF — upgrade to a broken manifest, gate trips, roll back to v0 + data.
    # Everything below drives the guest PRIMITIVES the way agent/host/service.go's applyServiceInstall
    # drives them over the channel (snapshot -> switch -> health-gate -> restore + revert), because
    # the host agent itself cannot run in this harness (the node IS the guest, no host on the wire).
    # ============================================================================================
    import json as _json

    # The good service is serving v0. Capture its tick — the rollback must bring exactly THIS back.
    good_tick = _json.loads(primary.succeed(f"cat {dataroot}/app/state.json"))["ticks"]
    assert good_tick > 0, f"no pre-upgrade data to preserve (tick={good_tick}) — rollback proof would be vacuous"
    print(f"pre-upgrade good tick = {good_tick}")

    # --- Snapshot the rollback point (applyServiceInstall's data.snapshot). The .snapshots dir is
    #     created by briard-data at mount, and the snapshot replicates with the volume. ---
    snap = "/var/lib/briard/.snapshots/fixture-preupgrade"
    primary.succeed("test -d /var/lib/briard/.snapshots")
    primary.succeed(f"btrfs subvolume snapshot -r {dataroot} {snap}")

    # --- The BROKEN manifest: same image digest and same service/container names (so same units,
    #     which is what makes this an UPGRADE and not a rename), the ONLY change being env
    #     BRIARD_BROKEN=1. The dummy then poisons its state and holds /healthz at 503 forever while
    #     staying a live process — the exact shape a runtime service breaks in. ---
    broken_manifest = (
        '{"name":"fixture","version":"v1-broken","network":"host","containers":[{'
        f'"name":"app","image":"${registryHost}/briard-fixture@{digest}",'
        '"mount":"${fixtureMount}","primary":true,"port":8080,"healthPath":"/healthz",'
        '"env":{"BRIARD_BROKEN":"1"}}]}'
    )

    # --- THE SWITCH, and it no longer takes a maintenance bracket ([V3b.3](f)). It used to:
    #     pause both nodes' promoters, quiesce the container, re-render on every node, resume. All
    #     of that existed because the service units WERE promoter chain members, so touching them
    #     under a live reactor meant stopping the reactor first. They are not members now, so an
    #     upgrade is: write the new manifest to the volume, converge.
    #
    #     Converge does the quiesce itself, and only for the service whose rendered bytes actually
    #     changed — systemd does not restart an already-active unit on daemon-reload, so without
    #     that the old container would keep serving while every file on disk described the new one.
    primary.succeed(f"cat > /tmp/broken.json <<'EOF'\n{broken_manifest}\nEOF")
    primary.succeed("cp /tmp/broken.json /var/lib/briard/.services/fixture.json && sync")
    primary.succeed("briard-agent --converge")
    primary.succeed("mountpoint -q /var/lib/briard")  # nothing in the switch unmounted the volume

    # --- THE GATE. The broken container comes up ACTIVE — a live process — so nothing at the DRBD
    #     layer knows anything is wrong, and under the new shape nothing at the promoter layer ever
    #     will: the container is not a chain member, so a crash could not demote the node either.
    #     The signal is the SERVICE's own /healthz, which stays 503 (probed directly, the way the
    #     host health gate does — NOT via the front door, which reports node readiness and would
    #     mask this). Prove it is real, and prove the upgrade actually did damage (poisoned the
    #     data), so a green run can't pass vacuously. ---
    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-app.service) = active", timeout=120)
    primary.wait_until_fails("curl -fsS http://127.0.0.1:8080/healthz", timeout=90)
    poisoned = _json.loads(primary.succeed(f"cat {dataroot}/app/state.json"))["ticks"]
    assert poisoned > good_tick, f"broken upgrade did not poison the data (tick={poisoned}); the rollback proof would be vacuous"
    print(f"health gate would TRIP: container active, its own endpoint 503, data poisoned to tick {poisoned}")

    # --- AND THE NODE IS STILL PROMOTED, which is the failure rule ([V3b.3](f)): converge's own
    #     failure demotes, a SERVICE's failure alerts and promotes. A code fault is deterministic,
    #     so a peer running the identical closure would hit it identically and the failover would
    #     only flap; and one broken service must not take the other N-1 down with it. Asserted
    #     here because this is the one place in the suite where a service is genuinely broken. ---
    primary.succeed("drbdadm role r0 | grep -q Primary")
    primary.succeed("mountpoint -q /var/lib/briard")
    primary.succeed("systemctl is-active briard-vip.service")
    print("the node stayed Primary with the VIP up — a broken service alerts, it does not demote")

    # --- ROLLBACK to the prior service (applyServiceInstall.revert): stop the broken CONTAINER,
    #     restore the data subvolume from the snapshot (delete + rw-snapshot, the guest's
    #     data.restore), put the PRIOR manifest back on the volume, and converge — which re-renders
    #     v0 and starts it fresh on the restored data.
    #
    #     Stop the CONTAINER, NEVER the pod: the container holds the data Volume bind, so stopping
    #     it releases the bind (which the restore needs) and is a clean stop, while stopping the
    #     pod makes podman kill its members out from under their units and each lands in `failed`.
    #
    #     No pause anywhere, and the failed-restore posture improved with it: the revert's refusal
    #     to serve poisoned data is now simply "do not converge", instead of leaving the promoter
    #     stopped and the node unable to fail over at all. ---
    primary.succeed("systemctl stop briard-fixture-app.service")
    primary.succeed("mountpoint -q /var/lib/briard")  # the shared mount survives a CLEAN container stop
    primary.succeed(f"btrfs subvolume delete {dataroot}")
    primary.succeed(f"btrfs subvolume snapshot {snap} {dataroot}")
    primary.succeed("cp /tmp/manifest.json /var/lib/briard/.services/fixture.json && sync")
    primary.succeed("briard-agent --converge")

    # --- RECOVERY. The good (v0) service serves again and the poison is GONE — the tick is back at
    #     the pre-upgrade point and climbing. {code+data} both rolled back: a failed upgrade left a
    #     working node, not a broken one. ---
    primary.wait_until_succeeds("curl -fsS http://127.0.0.1:8080/healthz", timeout=120)
    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-app.service) = active", timeout=120)
    recovered = _json.loads(primary.succeed(f"cat {dataroot}/app/state.json"))["ticks"]
    assert good_tick <= recovered < poisoned, f"data not rolled back to the pre-upgrade point: tick={recovered} (good={good_tick}, poison={poisoned})"
    primary.succeed("sleep 2")
    climbing = _json.loads(primary.succeed(f"cat {dataroot}/app/state.json"))["ticks"]
    assert climbing > recovered, f"restored service is not making progress: {recovered} -> {climbing}"
    print(f"ROLLED BACK: v0 serving, data restored (tick {recovered} < poison {poisoned}) and ticking again")
    ''}
  '';
}
