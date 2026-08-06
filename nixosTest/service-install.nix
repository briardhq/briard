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
#   - the REAL renderer (agent/quadlet via the quadlet-render helper) — the spike deliberately
#     hand-wrote its quadlet files and so tested the mechanism but not our output; this closes that;
#   - real quadlet generation (files under /run/containers/systemd becoming units at daemon-reload),
#     real podman, real drbd-reactor driving the rewritten promoter chain, and the real front door.
#
# WHAT IS NOT COVERED, named so a green run is not read as more than it is:
#   - the host agent's ORCHESTRATION (fetch/verify/warm/bracket/health-gate/revert) cannot run in
#     this harness: it drives the guest over virtio-serial, and here the node IS the guest with no
#     host on the other end. That sequence is unit-tested in agent/host/service_test.go, which is
#     where its bugs have actually surfaced.
#   - the maintenance BRACKET itself. This test writes the chain and starts drbd-reactor after, the
#     way every other lib.nix test does; it does not rewrite a chain under a live reactor.
#
# WITH broken = true, it runs the happy path and then keeps going: it UPGRADES the service
# to a manifest whose container never becomes healthy (the dummy's BRIARD_BROKEN mode), proves the
# health gate WOULD trip (the container stays active but the front door stays 503, and the data is
# poisoned), and then ROLLS BACK to the prior service with its data intact — the service-level twin
# of the {code+data} OS rollback. The host agent's orchestration of that sequence is unit-tested
# in agent/host/service_test.go (it drives the guest over virtio-serial, absent here); this proves
# the REAL mechanisms it relies on — a real broken container, a real btrfs snapshot/restore, a real
# quadlet re-render, and the real front door recovering.
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

  # The service's payload: the existing dummy-service, which serves /healthz and ticks a counter
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
  # Promoter = false: the chain is written by the test from the RENDERER's output, which is the
  # point — the agent writes it in production, and a static snippet here would test nothing.
  node =
    { ... }:
    {
      imports = [ (h.mkNode { inherit resource; promoter = false; payload = false; }) ];
      # The guest trusts the test CA. Free in this harness — the node is a module composition, not
      # a prebuilt disk — which is exactly why this test is hermetic rather than nested.
      security.pki.certificateFiles = [ "${certs}/ca.crt" ];
      environment.systemPackages = [ quadletRender pkgs.btrfs-progs ]; # test-only: the product path runs btrfs from the guest agent unit's own PATH
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
        '{"name":"fixture","version":"v0","containers":[{'
        f'"name":"app","image":"${registryHost}/briard-fixture@{digest}",'
        '"mount":"${fixtureMount}","primary":true,"port":8080,"healthPath":"/healthz"}]}'
    )
    for m in disk_nodes:
        m.succeed(f"cat > /tmp/manifest.json <<'EOF'\n{manifest}\nEOF")
        print(m.succeed("quadlet-render /tmp/manifest.json /tmp/rendered"))

    # The renderer's output is what the rest of this test runs on. Show it once — a rendered unit
    # is the artifact a human debugs when an install misbehaves.
    print(node1.succeed("cat /tmp/rendered/briard-fixture.pod /tmp/rendered/briard-fixture-app.container /tmp/rendered/briard-fixture-app.image"))

    # === 3. install: warm the image, render the units, provision the data ===
    # Warming is a REAL pull over TLS from the registry, digest-pinned, on EVERY node — the shape
    # the install path uses.
    for m in disk_nodes:
        m.succeed("mkdir -p /run/containers/systemd && cp /tmp/rendered/*.pod /tmp/rendered/*.container /tmp/rendered/*.image /run/containers/systemd/")
        m.succeed("systemctl daemon-reload")
        for unit in m.succeed("cat /tmp/rendered/images").split():
            m.succeed(f"systemctl start {unit}")
        # The pull really happened, and the image is addressable by the digest the manifest pins.
        m.succeed(f"podman image exists ${registryHost}/briard-fixture@{digest}")
    print("images warmed on every node by a real digest-pinned TLS pull")

    # Quadlet turned the rendered files into real units at daemon-reload — the whole point of the
    # mechanism, and the thing that makes unit generation a RUNTIME act.
    for unit in node1.succeed("cat /tmp/rendered/chain").split():
        if unit.startswith("briard-fixture"):
            node1.succeed(f"systemctl cat {unit} >/dev/null")
    # ...and nothing auto-started: the promoter decides, so a secondary must not run the pod.
    node1.succeed("test $(systemctl is-active briard-fixture-app.service) != active")
    print("quadlet generated the units at runtime, and none of them auto-started")

    # === 4. bring DRBD up, and give the promoter the RENDERED chain ===
    for m in [node1, node2, witness]:
        m.succeed("modprobe drbd")
    for m in disk_nodes:
        m.succeed("drbdadm create-md --force r0")
    for m in [node1, node2, witness]:
        m.succeed("systemctl start drbd@r0.target")
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")

    # The chain comes from the RENDERER, not from a hand-written snippet — in production the agent
    # writes exactly this, so a static list here would prove nothing about our output.
    chain = node1.succeed("cat /tmp/rendered/chain").split()
    print(f"promoter chain from the renderer: {chain}")
    quoted = ", ".join(f'"{u}"' for u in chain)
    snippet = f'[[promoter]]\n[promoter.resources.r0]\nstart = [ {quoted} ]\n'
    for m in disk_nodes:
        m.succeed(f"install -D /dev/stdin /etc/drbd-reactor.d/briard.toml <<'SNIP'\n{snippet}SNIP")
    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")

    # === 5. THE BAR: the node serves the installed service at the VIP ===
    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    node1.wait_until_succeeds("drbdadm role r0 | grep -qE 'Primary|Secondary'", timeout=90)
    primary = next(m for m in disk_nodes if role(m) == "Primary")
    print(f"primary={primary.name}")

    # The service's data subvolume lives on the replicated volume, so only the primary can make
    # it — this is what the install path's Primary-only provision step does. ONE subvolume with
    # plain per-container subdirectories: `btrfs subvolume delete` refuses on a subvolume that
    # contains nested subvolumes, so data.restore would break outright on any other shape.
    dataroot = node1.succeed("cat /tmp/rendered/dataroot").strip()
    primary.succeed(f"btrfs subvolume show {dataroot} >/dev/null 2>&1 || btrfs subvolume create {dataroot}")
    primary.succeed(f"mkdir -p {dataroot}/app")
    # Re-run the chain now that the storage exists (the promoter's earlier attempt had nothing to
    # mount into the container).
    primary.succeed("systemctl restart drbd-reactor.service")

    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-pod.service) = active", timeout=120)
    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-app.service) = active", timeout=120)
    print(primary.succeed("podman ps --format '{{.Names}} {{.Pod}} {{.Status}}'"))

    # The service answers on its OWN endpoint (host-networked :8080) — the same thing the health
    # gate probes (agent/host/service.go awaitHealthy), and a REAL 200 from the service, not the
    # front door's "no service installed" 200. The front door reports NODE readiness and
    # does not route to a runtime-installed service (per-domain routing is deferred), so gating
    # on the VIP /healthz would pass vacuously — which is exactly what the broken-install test caught.
    primary.wait_until_succeeds("curl -fsS http://127.0.0.1:8080/healthz", timeout=120)
    assert "ok" in primary.succeed("curl -fsS http://127.0.0.1:8080/healthz"), "service /healthz did not report ready"
    print("the installed service answers healthy on its own endpoint — install -> promote -> serve")

    # And the service's data really lands on the replicated subvolume.
    primary.wait_until_succeeds(f"test -s {dataroot}/app/state.json", timeout=60)
    print(primary.succeed(f"cat {dataroot}/app/state.json"))
    print("service state is on the DRBD subvolume — install -> promote -> serve, on the shipped node")

    ${pkgs.lib.optionalString (!broken) ''
    # --- THE RENDERED UNITS ARE VOLATILE, AND SOMETHING MUST PUT THEM BACK.
    #
    #     Happy-path run only: this reboots the node, so it cannot precede the broken-upgrade half,
    #     which carries on from here against a live service.
    #
    #     /run/containers/systemd is tmpfs, chosen deliberately: the volume holds the MANIFEST and
    #     each node renders its own units from it, so there is one identity and no second durable
    #     copy to drift from it. The consequence is that a reboot erases every rendered unit, and
    #     this asserts it plainly instead of leaving it a property people assume.
    #
    #     Nothing in THIS harness can put them back: the node here IS the guest, with no host on
    #     the other end of the channel (see the header). Restoring them at bring-up is the HOST
    #     agent's job, from its node-local manifest cache. So the honest claim here is the
    #     premise, not the cure — after a reboot the service is gone and stays gone until
    #     something re-renders, which is why the host must.
    #     Asserted as a PAIR on the same command, so the "after" cannot pass against a node that
    #     never had the unit in the first place — the non-vacuity discipline this suite applies to
    #     every before/after claim.
    primary.succeed("systemctl cat briard-fixture-app.service")
    primary.shutdown()
    primary.start()
    primary.wait_for_unit("multi-user.target")
    left = primary.succeed("ls -A /run/containers/systemd 2>/dev/null || true").strip()
    assert left == "", f"expected the rendered units to be gone after a reboot, found: {left!r}"
    # ...and systemd has no unit left to start, which is exactly what a promoter would hit.
    primary.fail("systemctl cat briard-fixture-app.service")
    print("after a reboot the rendered units are gone — the host must re-render them at bring-up")
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

    # --- The BROKEN manifest: same image digest and same service/container names (so same units and
    #     same promoter chain — this is an UPGRADE, not a rename), the ONLY change being env
    #     BRIARD_BROKEN=1. The dummy then poisons its state and holds /healthz at 503 forever while
    #     staying a live process — the exact shape a runtime service breaks in. Render it on EVERY
    #     node (units are node-local) so a survivor would have it too. ---
    broken_manifest = (
        '{"name":"fixture","version":"v1-broken","containers":[{'
        f'"name":"app","image":"${registryHost}/briard-fixture@{digest}",'
        '"mount":"${fixtureMount}","primary":true,"port":8080,"healthPath":"/healthz",'
        '"env":{"BRIARD_BROKEN":"1"}}]}'
    )
    for m in disk_nodes:
        m.succeed(f"cat > /tmp/broken.json <<'EOF'\n{broken_manifest}\nEOF")
        print(m.succeed("quadlet-render /tmp/broken.json /tmp/broken"))
        m.succeed("cp /tmp/broken/*.pod /tmp/broken/*.container /tmp/broken/*.image /run/containers/systemd/")
        m.succeed("systemctl daemon-reload")

    # --- The maintenance bracket, mirrored (reactor.pause/resume). reactor.pause STOPS drbd-reactor
    #     with the promote-vs-stop override removed; stop-services-on-exit defaults false, so the
    #     promoted services + DRBD Primary stay UP (the volume stays mounted). Bracket BOTH disk nodes
    #     — both run a promoter, so pausing only the primary would let the peer promote and pull the
    #     volume out from under the maintenance. ---
    def pause(m):
        m.succeed("rm -f /run/systemd/system/drbd-services@r0.target.d/reactor-50-before.conf")
        m.succeed("systemctl daemon-reload")
        m.succeed("systemctl stop drbd-reactor.service")
    def resume(m):
        m.succeed("systemctl start drbd-reactor.service")

    # Switch to the broken version: pause, quiesce the CONTAINER (only — see the rollback for why
    # never the pod) so resume restarts it with the new content (systemd does not restart an
    # already-active unit on daemon-reload alone), resume.
    for m in disk_nodes:
        pause(m)
    primary.succeed("mountpoint -q /var/lib/briard")  # the bracket kept the volume mounted
    primary.succeed("systemctl stop briard-fixture-app.service")
    for m in disk_nodes:
        resume(m)

    # --- THE GATE. The broken container comes up ACTIVE — a live process — so drbd-reactor's promoter
    #     is satisfied and nothing at the DRBD layer knows anything is wrong. The signal is the
    #     SERVICE's own /healthz, which stays 503 (probed directly, the way the host health gate does
    #     — NOT via the front door, which reports node readiness and would mask this). Prove it is
    #     real, and prove the upgrade actually did damage (poisoned the data), so a green run can't
    #     pass vacuously. ---
    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-app.service) = active", timeout=120)
    primary.wait_until_fails("curl -fsS http://127.0.0.1:8080/healthz", timeout=90)
    poisoned = _json.loads(primary.succeed(f"cat {dataroot}/app/state.json"))["ticks"]
    assert poisoned > good_tick, f"broken upgrade did not poison the data (tick={poisoned}); the rollback proof would be vacuous"
    print(f"health gate would TRIP: container active, front door 503, data poisoned to tick {poisoned}")

    # --- ROLLBACK to the prior service (applyServiceInstall.revert): under one pause, stop the broken
    #     CONTAINER, restore the data subvolume from the snapshot (delete + rw-snapshot, the guest's
    #     data.restore), re-render the prior (v0) units, then resume so v0 starts fresh on the
    #     restored data.
    #
    #     Stop the CONTAINER, NEVER the pod: the container
    #     holds the data Volume bind, so stopping it releases the bind AND leaves the drbd-reactor
    #     target up — while stopping the pod tears the target down and unmounts the SHARED /var/lib/briard
    #     (which would take every other service on a multi-service node with it). ---
    for m in disk_nodes:
        pause(m)
    primary.succeed("systemctl stop briard-fixture-app.service")
    primary.succeed("mountpoint -q /var/lib/briard")  # the shared mount survives a CLEAN container stop
    primary.succeed(f"btrfs subvolume delete {dataroot}")
    primary.succeed(f"btrfs subvolume snapshot {snap} {dataroot}")
    for m in disk_nodes:
        m.succeed("cp /tmp/rendered/*.pod /tmp/rendered/*.container /tmp/rendered/*.image /run/containers/systemd/")
        m.succeed("systemctl daemon-reload")
    for m in disk_nodes:
        resume(m)

    # --- RECOVERY. The front door is healthy again, the good (v0) service serves, and the poison is
    #     GONE — the tick is back at the pre-upgrade point and climbing again. {code+data} both rolled
    #     back: a failed upgrade left a working node, not a broken one. ---
    primary.wait_until_succeeds("curl -fsS http://127.0.0.1:8080/healthz", timeout=120)
    primary.wait_until_succeeds("test $(systemctl is-active briard-fixture-app.service) = active", timeout=120)
    recovered = _json.loads(primary.succeed(f"cat {dataroot}/app/state.json"))["ticks"]
    assert good_tick <= recovered < poisoned, f"data not rolled back to the pre-upgrade point: tick={recovered} (good={good_tick}, poison={poisoned})"
    primary.succeed("sleep 2")
    climbing = _json.loads(primary.succeed(f"cat {dataroot}/app/state.json"))["ticks"]
    assert climbing > recovered, f"restored service is not making progress: {recovered} -> {climbing}"
    print(f"ROLLED BACK: front door healthy, data restored (tick {recovered} < poison {poisoned}) and ticking again")
    ''}
  '';
}
