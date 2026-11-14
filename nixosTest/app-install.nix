# Installing an app at runtime, on the shape a stranger actually installs. A shipped node boots
# with no app (it mounts, promotes, and serves a landing page at the VIP); this test puts an app
# on it the way `briard app install` does and asserts install -> promote -> serve, the
# integration bar for the free tier (failover of a runtime-installed app is a flock concern: on
# briard free each node is an isolated island).
#
# Real here, because the value of this test is entirely in what it does not stub:
#   - an OCI registry over TLS and a digest-pinned `podman pull` from it (containers/image refuses
#     plain HTTP for every address including localhost, so the test runs a real CA);
#   - the renderer, run where the product runs it, inside `briard-guest-agent --converge`, off the
#     manifest this test puts on the replicated volume. Only the .image unit is copied in from the
#     side, to warm the pull; converge writes the app units;
#   - quadlet generation (files under /run/containers/systemd becoming units at daemon-reload),
#     podman, drbd-reactor driving the static promoter chain, and the reverse proxy;
#   - the node promoting with zero apps first, then with one, so the install is a change and not
#     a coincidence.
#
# Not covered, so a green run is not read as more: the host agent's orchestration
# (fetch/verify/warm/health-gate/revert) drives the guest over virtio-serial, and here the node is
# the guest with no host on the other end; it is unit-tested in agent/host/app_test.go. The test
# does by hand the Primary-only half an install performs on the volume, then hands over to converge.
#
# With broken = true it continues past the happy path: it upgrades the app to a manifest whose
# container never becomes healthy (the dummy's BRIARD_BROKEN mode), proves the health gate would
# trip (container active, its own endpoint 503, data poisoned), proves the node stays promoted
# (an app failure alerts, it does not demote: converge-at-promotion's failure rule), then rolls
# back to the prior app with its data intact, the app-level twin of the {code+data} OS rollback.
# The orchestration of that sequence is likewise unit-tested; this proves the real mechanisms it
# relies on: a broken container, btrfs snapshot/restore, a converge re-render, and a recovery.
{ pkgs, guestModule, quadletRender, broken ? false }:
let
  h = import ./lib.nix { inherit pkgs guestModule; };

  registryIP = "10.0.0.99";
  registryPort = "5000";
  registryHost = "${registryIP}:${registryPort}";

  # A CA + server cert for the registry, minted at build time. The guest nodes trust the CA, so
  # the pull is a real TLS pull: `--tls-verify=false` anywhere in this test would quietly delete
  # the thing it is meant to prove.
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

  # The app: the existing dummy-app, which serves /healthz and ticks a counter into its data dir.
  # Reused because every other test already trusts it, and its tick proves the mounted subvolume
  # is really the app's.
  fixtureImage = pkgs.dockerTools.buildImage {
    name = "briard-fixture";
    tag = "v0";
    config.Cmd = [ "${pkgs.dummy-app}/bin/dummy-app" ];
  };
  # The fixture's data path is a const in its source, so the container's mount point is the
  # fixture's while the host-side directory is the renderer's: the normal case for a catalogued
  # app (upstream decides where its data lives inside the container), so the right shape to test.
  fixtureMount = "/var/lib/briard/dummy";

  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  # Promoter = false: the test writes the snippet itself, when it chooses, so the node is seen
  # promoting with zero apps before the install and with one after. The chain it writes is the
  # static production one (lib.nix promoterSnippet), not derived from a rendering.
  node =
    { config, ... }:
    {
      imports = [ (h.mkNode { inherit resource; promoter = false; }) ];
      # The guest trusts the test CA, free here because the node is a module composition, not a
      # prebuilt disk: exactly why this test is hermetic rather than nested.
      security.pki.certificateFiles = [ "${certs}/ca.crt" ];
      # Test-only: in the product these run from unit PATHs (btrfs from the guest agent unit,
      # converge from briard-apps). This test drives the install by hand, so all three must be
      # where a test command can reach them.
      environment.systemPackages = [ quadletRender pkgs.btrfs-progs config.briard.agentPackage ];
    };
  witnessNode = h.mkNode {
    inherit resource;
    diskless = true;
    promoter = false;
  };
in
pkgs.testers.runNixOSTest {
  name = "app-install${pkgs.lib.optionalString broken "-broken"}";
  skipTypeCheck = true;

  nodes = {
    node1 = node;
    node2 = node;
    witness = witnessNode;
    # Named to sort after "witness": the framework numbers nodes alphabetically and lib.nix pins
    # each DRBD peer at 10.0.0.<id+1>, so a node sorting before the witness silently shifts its
    # address and DRBD never connects.
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
        # The pusher trusts the same CA, so nothing in this test ever passes --tls-verify=false.
        security.pki.certificateFiles = [ "${certs}/ca.crt" ];
        # Brings /etc/containers/policy.json, which skopeo requires before it copies anything.
        # Preferred over `skopeo --insecure-policy`, whose name reads like a TLS bypass and would
        # make a reader think this test skips verification; it does not.
        virtualisation.containers.enable = true;
        environment.systemPackages = [ pkgs.skopeo ];
      };
  };

  testScript = ''
    disk_nodes = [node1, node2]
    start_all()
    for m in [node1, node2, witness, zregistry]:
        m.wait_for_unit("multi-user.target")

    # === 1. publish the fixture into the registry, and learn its digest ===
    # The manifest schema accepts only digest-pinned images (a tag is mutable, making the app
    # identity a promise someone else could change), so the digest is the reference the whole
    # install hangs off.
    zregistry.wait_for_open_port(5000)
    zregistry.succeed(
        "skopeo copy docker-archive:${fixtureImage} docker://${registryHost}/briard-fixture:v0"
    )
    digest = zregistry.succeed(
        "skopeo inspect docker://${registryHost}/briard-fixture:v0 --format '{{.Digest}}'"
    ).strip()
    print(f"fixture published at ${registryHost}/briard-fixture@{digest}")

    # === 2. the app manifest, and the real renderer ===
    manifest = (
        '{"name":"fixture","version":"v0","network":"host","containers":[{'
        f'"name":"app","image":"${registryHost}/briard-fixture@{digest}",'
        '"mounts":[{"tier":"data","dir":"app","path":"${fixtureMount}"}],"primary":true,"port":8080,"healthPath":"/healthz"}]}'
    )
    for m in disk_nodes:
        m.succeed(f"cat > /tmp/manifest.json <<'EOF'\n{manifest}\nEOF")
        print(m.succeed("quadlet-render /tmp/manifest.json /tmp/rendered"))

    # Shown once: a rendered unit is what a human debugs when an install misbehaves.
    print(node1.succeed("cat /tmp/rendered/briard-fixture.pod /tmp/rendered/briard-fixture-app.container /tmp/rendered/briard-fixture-app.image"))

    # === 3. warm the image on every node ===
    # A digest-pinned TLS pull, the shape the install path uses and the half of an install that
    # legitimately needs the network; running it on every node lets the failover path promise it
    # never will. Only the .image unit is copied in, to start the pull: converge writes the pod
    # and container units itself, from the volume, on whichever node promotes, and copying them in
    # would make this harness stand in for the product.
    for m in disk_nodes:
        m.succeed("mkdir -p /run/containers/systemd")
        m.succeed("cp /tmp/rendered/briard-fixture-app.image /run/containers/systemd/ && systemctl daemon-reload")
        for unit in m.succeed("cat /tmp/rendered/images").split():
            m.succeed(f"systemctl start {unit}")
        # The pull really happened, and the image is addressable by the digest the manifest pins.
        m.succeed(f"podman image exists ${registryHost}/briard-fixture@{digest}")
    print("images warmed on every node by a real digest-pinned TLS pull")

    # Nothing is running yet, and nothing generated itself: only the .image units exist (started by
    # hand above), the pod and container units do not, and no node has been given the manifest.
    node1.fail("systemctl cat briard-fixture-app.service")
    print("the image is resident and the app does not exist — the state before an install")

    # === 4. bring DRBD up, and give the promoter the static chain ===
    for m in [node1, node2, witness]:
        m.succeed("modprobe drbd")
    for m in disk_nodes:
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")
    # The witness has no tier to build and still needs its `.res` and its attach;
    # briard-node-storage runs on every node, which is why one call covers both.
    witness.succeed("briard-test-storage")
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")

    # The chain is static: drbd-reactor promotes with it, but the volume to converge to is
    # readable only after promotion, so the start-list cannot name the apps; briard-apps renders
    # and starts them once the mount exists. Taken from lib.nix rather than restated: a
    # hand-written copy goes stale silently, and one missing the reverse proxy never starts it,
    # leaving the app unreachable by name.
    snippet = """${h.promoterSnippet}"""
    for m in disk_nodes:
        m.succeed(f"install -D /dev/stdin /run/briard/drbd-reactor.d/briard.toml <<'SNIP'\n{snippet}SNIP")
    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")

    # === 5. the bar: the node serves the installed app at the VIP ===
    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    node1.wait_until_succeeds("drbdadm role r0 | grep -qE 'Primary|Secondary'", timeout=90)
    primary = next(m for m in disk_nodes if role(m) == "Primary")
    print(f"primary={primary.name}")

    # It promoted with zero apps first, the shipped state of every node a stranger installs:
    # briard-apps converged to nothing (active, no app unit). Asserted before the install so the
    # install is a change and not a coincidence.
    primary.wait_until_succeeds("test $(systemctl is-active briard-apps.service) = active", timeout=120)
    primary.fail("systemctl cat briard-fixture-app.service")

    # === 5b. the install's Primary-only half, by hand ===
    # The data subvolume and the manifest live on the replicated volume, so only the primary can
    # write them (the install path's Primary-only provision step). One subvolume with plain
    # per-container subdirectories: `btrfs subvolume delete` refuses a subvolume containing nested
    # subvolumes, so data.restore would break outright.
    dataroot = node1.succeed("cat /tmp/rendered/dataroot").strip()
    primary.succeed(f"btrfs subvolume show {dataroot} >/dev/null 2>&1 || (mkdir -p $(dirname {dataroot}) && btrfs subvolume create {dataroot})")
    primary.succeed(f"mkdir -p {dataroot}/app")
    # The manifest is the app's identity, and putting it on the volume is what an install does.
    # Everything after this point is the product's own code.
    primary.succeed("mkdir -p /var/lib/briard/.apps")
    primary.succeed("cp /tmp/manifest.json /var/lib/briard/.apps/fixture.json && sync")
    primary.succeed("briard-guest-agent --converge")

    # Converge rendered the units and quadlet generated them, at runtime, from the volume, with
    # nothing copied in from the side: the whole mechanism, asserted after the fact rather than
    # arranged beforehand.
    primary.succeed("systemctl cat briard-fixture-app.service >/dev/null")
    primary.succeed("systemctl cat briard-fixture-pod.service >/dev/null")

    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-pod.service) = active", timeout=120)
    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-app.service) = active", timeout=120)
    print(primary.succeed("podman ps --format '{{.Names}} {{.Pod}} {{.Status}}'"))

    # The app answers on its own endpoint (host-networked :8080), what the health gate probes
    # (agent/host/app.go awaitHealthy, which asks the guest to resolve it rather than assembling
    # this URL): a real 200 from the app, not the reverse proxy.
    primary.wait_until_succeeds("curl -fsS http://127.0.0.1:8080/healthz", timeout=120)
    assert "ok" in primary.succeed("curl -fsS http://127.0.0.1:8080/healthz"), "app /healthz did not report ready"
    print("the installed app answers healthy on its own endpoint — install -> promote -> serve")

    # === 5c. and through the reverse proxy, under its own name ===
    # The install happened on a node with no flock name, so the app is routed and unnamed, a real
    # shipped state (a node publishes nothing rather than a guess): the table carries the app with
    # no host to reach it by.
    import json
    table = json.loads(primary.succeed("cat /run/briard/routes.json"))
    assert [s["name"] for s in table["apps"]] == ["fixture"], f"routing table = {table}"
    assert not table["apps"][0].get("hosts"), f"an unnamed node composed a name: {table}"
    assert table["apps"][0]["address"] == "127.0.0.1", f"routing table = {table}"
    assert table["apps"][0]["routes"] == [{"listen": "name", "to": "http://:8080"}], f"routing table = {table}"

    # Name the flock the way the agent does, re-converge, and the same app acquires a name. A real
    # node runs this in the other order; this order proves the name is composed from the flock
    # name rather than baked at install.
    primary.succeed("mkdir -p /run/briard && printf 'FLOCK_NAME=brave-elf\\n' >/run/briard/mdns.env")
    primary.succeed("briard-guest-agent --converge")
    hosts = json.loads(primary.succeed("cat /run/briard/routes.json"))["apps"][0]["hosts"]
    host = hosts[0]
    assert host == "briard-brave-elf-fixture.local", f"the node composed {host}"
    assert "briard-fixture.local" in hosts, f"the bare alias is not in the table: {hosts}"
    # The reverse proxy routes the flock-scoped name and the bare alias alike to the app.
    for name in (host, "briard-fixture.local"):
        body = primary.succeed(f"curl -fsS -H 'Host: {name}' http://192.168.1.100/healthz")
        assert "ok" in body and "reverse proxy" not in body, (
            f"the VIP under {name} answered {body!r}; want the APP's own answer"
        )
    # The bare address is still the node's, and it names what it routes.
    node_health = primary.succeed("curl -fsS http://192.168.1.100/healthz")
    assert "1 app(s) routed" in node_health and "fixture" in node_health, f"node /healthz = {node_health!r}"
    # A name nobody serves still lands on the node's reverse proxy rather than some app's 404: the
    # fall-through is the property. The proxy forwards it to the household dashboard, which
    # refuses a browser with no session (401, one instruction, no inventory), because reaching
    # the VIP is not authentication. Which names the node answers to needs a session to read;
    # the unauthenticated version is /healthz, asserted just above.
    #
    # ⚠️ This is the non-vacuous place for the no-inventory assertion, which is why it is here and
    # not only in install-macvtap.nix: that test refuses on a node with nothing installed, so
    # "the refusal names no app" passes there even if the refusal rendered whatever it knew. Here
    # an app is installed and routed under a name composed above, so a refusal that leaked the
    # inventory would print it and fail.
    refused = primary.succeed(
        "curl -sS -o /tmp/vip.html -w '%{http_code}' "
        "-H 'Host: briard-brave-elf-nope.local' http://192.168.1.100/"
    ).strip()
    page = primary.succeed("cat /tmp/vip.html")
    assert refused == "401" and "not yet trusted" in page, f"the unrouted name served {refused}: {page!r}"
    assert "fixture" not in page and host not in page, (
        f"the refusal named the inventory to a browser with no session: {page!r}"
    )

    # And the app's data really lands on the replicated subvolume.
    primary.wait_until_succeeds(f"test -s {dataroot}/app/state.json", timeout=60)
    print(primary.succeed(f"cat {dataroot}/app/state.json"))
    print("app state is on the DRBD subvolume — install -> promote -> serve, on the shipped node")

    ${pkgs.lib.optionalString (!broken) ''
    # --- An image removed under an active pull unit is pulled again: the shape an uninstall leaves
    #     for the next install of the same app, and an upgrade for the one after. The image is
    #     removed while its `.image` unit (a oneshot with RemainAfterExit) still reads active from
    #     the pull in step 3, so a `start` of it succeeds and runs nothing and the container then
    #     fails on Pull=never; only a real systemd can say so. The premise is asserted, so this
    #     cannot pass on a node whose unit happened to be inactive.
    primary.succeed("systemctl stop briard-fixture-app.service")
    primary.succeed(f"podman rmi ${registryHost}/briard-fixture@{digest}")
    for unit in node1.succeed("cat /tmp/rendered/images").split():
        primary.succeed(f"test $(systemctl is-active {unit}) = active")
    primary.succeed("briard-guest-agent --converge")
    primary.succeed(f"podman image exists ${registryHost}/briard-fixture@{digest}")
    primary.wait_until_succeeds("curl -fsS http://127.0.0.1:8080/healthz", timeout=90)
    print("an image removed under its still-active pull unit was pulled again, and the app serves")

    # --- The rendered units are volatile, and converge puts them back by itself. Happy-path run
    #     only: this reboots the node, so it cannot precede the broken-upgrade half, which carries
    #     on against a live app.
    #
    #     /run/containers/systemd is tmpfs by choice: the volume holds the manifest and each node
    #     renders its own units from it, so there is one identity and no second durable copy to
    #     drift, and a reboot erases every rendered unit. No host re-renders them: briard-apps is a
    #     promoter chain member, so the node re-renders from the volume the moment it promotes, the
    #     same mechanism that fixes the survivor case, seen from the reboot side.
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

    # Re-arm the promoter on the returning node (the agent does this at bring-up in the product;
    # here the harness does, as the first time). DRBD does not auto-promote, so the reactor must
    # run before any node can take the role; starting it re-enters the race.
    primary.succeed("modprobe drbd")
    primary.succeed("briard-test-storage") # re-attach the surviving replica (never re-seeds it)
    primary.succeed("systemctl start drbd-reactor.service")

    # Whichever node holds the volume must serve the fixture again; that indifference is the
    # property. The peer very likely took over while this node was down, and then serves an app it
    # never installed, read off the volume and rendered for itself. If the returning node takes
    # its role back instead, it re-rendered from the same volume after a reboot wiped its tmpfs.
    # Both are converge, which is why the assertion names no node.
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
    # The failure half: upgrade to a broken manifest, gate trips, roll back to v0 + data. This
    # drives the guest primitives the way agent/host/app.go applyAppInstall does over the channel
    # (snapshot -> switch -> health-gate -> restore + revert), because the host agent cannot run in
    # this harness (the node is the guest, no host on the wire).
    # ============================================================================================
    import json as _json

    # The good app is serving v0. Capture its tick: the rollback must bring exactly this back.
    good_tick = _json.loads(primary.succeed(f"cat {dataroot}/app/state.json"))["ticks"]
    assert good_tick > 0, f"no pre-upgrade data to preserve (tick={good_tick}) — rollback proof would be vacuous"
    print(f"pre-upgrade good tick = {good_tick}")

    # --- Stop, then snapshot the rollback point (applyAppInstall quiesce + data.member, in that
    #     order). A live snapshot is only crash-consistent, and the catalog cannot promise every
    #     app survives one: apps-pair.nix shows mosquitto losing exactly the retained message a
    #     rollback would be for. The container unit, never the pod, which would take the volume
    #     down under every other app. The baseline tick is read before this stop, the ordering the
    #     product keeps for the S1 sample, which is why the recovery assertion is a range (good <=
    #     recovered < poisoned). briard-primary-storage creates .snapshots at mount, and the
    #     snapshot replicates with the volume. No restart: the converge below starts the units. ---
    snap = "/var/lib/briard/data/.snapshots/fixture-preupgrade"
    primary.succeed("test -d /var/lib/briard/data/.snapshots")
    primary.succeed("systemctl stop briard-fixture-app.service")
    primary.succeed(f"btrfs subvolume snapshot -r {dataroot} {snap}")

    # --- The broken manifest: same image digest and app/container names (so same units: an
    #     upgrade, not a rename), the only change env BRIARD_BROKEN=1. The dummy then poisons its
    #     state and holds /healthz at 503 forever while staying a live process, the exact shape a
    #     runtime app breaks in. ---
    broken_manifest = (
        '{"name":"fixture","version":"v1-broken","network":"host","containers":[{'
        f'"name":"app","image":"${registryHost}/briard-fixture@{digest}",'
        '"mounts":[{"tier":"data","dir":"app","path":"${fixtureMount}"}],"primary":true,"port":8080,"healthPath":"/healthz",'
        '"env":{"BRIARD_BROKEN":"1"}}]}'
    )

    # --- The switch, with no maintenance bracket (no promoter pause): the app units are not
    #     promoter chain members, so an upgrade is: write the new manifest to the volume, converge.
    #     Converge quiesces only the app whose rendered bytes changed: systemd does not restart an
    #     already-active unit on daemon-reload, so otherwise the old container would keep serving
    #     while every file on disk described the new one.
    primary.succeed(f"cat > /tmp/broken.json <<'EOF'\n{broken_manifest}\nEOF")
    primary.succeed("cp /tmp/broken.json /var/lib/briard/.apps/fixture.json && sync")
    primary.succeed("briard-guest-agent --converge")
    primary.succeed("mountpoint -q /var/lib/briard")  # nothing in the switch unmounted the volume

    # --- The gate. The broken container comes up active (a live process), so nothing at the DRBD
    #     layer knows anything is wrong, and nothing at the promoter layer ever will: the container
    #     is not a chain member, so even a crash could not demote the node. The signal is the
    #     /healthz of the app, staying 503, probed directly as the host health gate does, not via
    #     the reverse proxy, which reports node readiness and would mask this. Prove it, and prove
    #     the upgrade did damage (poisoned the data), so a green run cannot pass vacuously. ---
    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-app.service) = active", timeout=120)
    primary.wait_until_fails("curl -fsS http://127.0.0.1:8080/healthz", timeout=90)
    poisoned = _json.loads(primary.succeed(f"cat {dataroot}/app/state.json"))["ticks"]
    assert poisoned > good_tick, f"broken upgrade did not poison the data (tick={poisoned}); the rollback proof would be vacuous"
    print(f"health gate would TRIP: container active, its own endpoint 503, data poisoned to tick {poisoned}")

    # --- And the node is still promoted, the failure rule: converge failing demotes, an app
    #     failing alerts and promotes. A code fault is deterministic, so a peer running the
    #     identical closure would hit it identically and failover would only flap; and one broken
    #     app must not take the other N-1 down. Asserted here, the one place in the suite where an
    #     app is genuinely broken. ---
    primary.succeed("drbdadm role r0 | grep -q Primary")
    primary.succeed("mountpoint -q /var/lib/briard")
    primary.succeed("systemctl is-active briard-vip.service")
    print("the node stayed Primary with the VIP up — a broken app alerts, it does not demote")

    # --- Rollback to the prior app (applyAppInstall.revert): stop the broken container, restore
    #     the data subvolume from the snapshot (delete + rw-snapshot, the guest data.restore), put
    #     the prior manifest back, and converge, which re-renders v0 and starts it fresh on the
    #     restored data. The container, never the pod: the container holds the data Volume bind,
    #     so stopping it releases the bind the restore needs and is a clean stop, while stopping
    #     the pod makes podman kill its members out from under their units, each landing in
    #     `failed`. No pause anywhere: the revert refuses to serve poisoned data simply by not
    #     converging, leaving the one app stopped while the node keeps its ability to fail over. ---
    primary.succeed("systemctl stop briard-fixture-app.service")
    primary.succeed("mountpoint -q /var/lib/briard")  # the shared mount survives a CLEAN container stop
    primary.succeed(f"btrfs subvolume delete {dataroot}")
    primary.succeed(f"btrfs subvolume snapshot {snap} {dataroot}")
    primary.succeed("cp /tmp/manifest.json /var/lib/briard/.apps/fixture.json && sync")
    primary.succeed("briard-guest-agent --converge")

    # --- Recovery: the good (v0) app serves again and the poison is gone, the tick back at the
    #     pre-upgrade point and climbing. {code+data} both rolled back: a failed upgrade left a
    #     working node, not a broken one. ---
    primary.wait_until_succeeds("curl -fsS http://127.0.0.1:8080/healthz", timeout=120)
    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-app.service) = active", timeout=120)
    recovered = _json.loads(primary.succeed(f"cat {dataroot}/app/state.json"))["ticks"]
    assert good_tick <= recovered < poisoned, f"data not rolled back to the pre-upgrade point: tick={recovered} (good={good_tick}, poison={poisoned})"
    primary.succeed("sleep 2")
    climbing = _json.loads(primary.succeed(f"cat {dataroot}/app/state.json"))["ticks"]
    assert climbing > recovered, f"restored app is not making progress: {recovered} -> {climbing}"
    print(f"ROLLED BACK: v0 serving, data restored (tick {recovered} < poison {poisoned}) and ticking again")
    ''}
  '';
}
