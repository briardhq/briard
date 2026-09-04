# A SPIKE, not a suite member: how long does a COLD converge hold the promotion when the pull it
# has to make is very slow? ([B.56], re-scoped.)
#
# WHY THIS EXISTS. [V3b.3](f) put a fetch on the promotion path. `briard-services` is a chain
# member, its failure fails the promotion, and `warmImage` (agent/guestagent/converge.go) answers a
# missing image with `systemctl start <unit>-image.service` -- a blocking `podman image pull`.
# The comment there argues the case and is right about the design: a node that was down when the
# install ran promotes into a service it was never told about, and fetching what it can name beats
# a cluster-wide negotiation nothing arbitrates. What it does NOT say is how long that fetch may
# take before something gives up, and neither does any test: `chain-member-contract` drives its
# member failures with SIGKILL and `systemctl restart`, which fail INSTANTLY. Every rule we have
# about the chain is a rule about a fast failure.
#
# THE THEORY THIS MEASURES (and it is only a theory until the run prints otherwise): nothing bounds
# it. `briard-services` is `Type=oneshot`, and systemd disables `TimeoutStartSec=` by default for
# oneshot; quadlet's generated `.image` unit is a oneshot too, and neither we nor quadlet set a
# timeout on either. If that holds, a slow pull holds the resource Primary with no VIP and no
# services, indefinitely, and the demote-and-hand-over the eight rules prove ([V3b.5](c)) never
# fires -- because the member never fails, it just never finishes.
#
# WHY IT IS NOT IN THE NIGHTLY. It is a stopwatch, not an assertion: its whole output is a
# timeline, and it deliberately runs its observation window to the end rather than failing early.
# It rides `debug` for the same reason `drbd-link-split` does -- run it by hand, read the log.
#
# ONE NODE, DELIBERATELY. The peer half of [B.56]'s original shape ("withhold the prewarm from one
# anchor") is not what is unmeasured: a cold winner handing over to a warm peer is rules 5/7/8 with
# a different trigger, and peer-image caching ([B.117]) is expected to remove the asymmetry
# entirely. What no topology answers is this node's own behaviour, so this rig has no peer.
#
# SLOW, NOT STALLED, and that distinction is the measurement. A stalled socket would only tell us
# whether some stall detector exists; a transfer that is genuinely progressing the whole time is
# what proves an absent *overall* bound. The rig throttles the registry's link with tbf and prints
# the byte counters at both ends, so a reader can see the pull was moving the whole window.
{ pkgs, guestModule, quadletRender }:
let
  h = import ./lib.nix { inherit pkgs guestModule; };

  registryIP = "10.0.0.99";
  registryHost = "${registryIP}:5000";

  # The throttle. 32 kbit/s is chosen so the LAYER cannot finish inside the window while the
  # handshake and manifest fetch still complete in seconds -- the pull must get properly under way,
  # or this measures a connect timeout rather than a transfer.
  rate = "32kbit";
  # The observation window. Past 300s on purpose: it clears systemd's 90s DefaultTimeoutStartSec,
  # the 180s the deadman uses, and the 300s promotion hold, so a bound at any of the numbers we
  # already use in this tree would be visible rather than argued away.
  windowSecs = 420;

  # Minted at build time, trusted by the node: containers/image refuses plain HTTP for every
  # address, so a throttled registry still has to be a real TLS one (service-install.nix measured
  # this; the same CA shape is reused rather than re-derived).
  certs = pkgs.runCommand "cold-pull-certs" { nativeBuildInputs = [ pkgs.openssl ]; } ''
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

  # The same dummy every other rig runs, so the only novelty here is where it has to come from.
  fixtureImage = pkgs.dockerTools.buildImage {
    name = "briard-fixture";
    tag = "v0";
    config.Cmd = [ "${pkgs.dummy-service}/bin/dummy-service" ];
  };
  fixtureMount = "/var/lib/briard/dummy";

  # Mesh-of-one: no peer to hand over to, which is the point (see the header).
  resource = h.mkResource [ { name = "node1"; id = 0; } ];
in
pkgs.testers.runNixOSTest {
  name = "cold-converge-pull";
  skipTypeCheck = true;

  nodes = {
    node1 =
      { config, ... }:
      {
        # NO `fixture`: the image must be ABSENT, which is the whole premise. Every other rig
        # prewarms it precisely so converge never reaches the pull.
        imports = [ (h.mkNode { inherit resource; }) ];
        security.pki.certificateFiles = [ "${certs}/ca.crt" ];
        environment.systemPackages = [ quadletRender pkgs.btrfs-progs config.briard.agentPackage ];
      };
    # Sorts after node1, as service-install.nix's does: the framework numbers nodes
    # alphabetically and lib.nix pins DRBD peers by node id, so an earlier name shifts addresses.
    zregistry =
      { ... }:
      {
        networking.interfaces.eth1.ipv4.addresses = [
          { address = registryIP; prefixLength = 24; }
        ];
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
        security.pki.certificateFiles = [ "${certs}/ca.crt" ];
        virtualisation.containers.enable = true;
        environment.systemPackages = [ pkgs.skopeo pkgs.iproute2 ];
      };
  };

  testScript = ''
    import time

    start_all()
    for m in [node1, zregistry]:
        m.wait_for_unit("multi-user.target")

    # === 1. publish the image, AT FULL SPEED ===
    # The throttle goes on later: staging the registry over a 32kbit link would cost the same
    # hours the pull is about to, and the push is not the subject.
    zregistry.wait_for_open_port(5000)
    zregistry.succeed(
        "skopeo copy docker-archive:${fixtureImage} docker://${registryHost}/briard-fixture:v0"
    )
    digest = zregistry.succeed(
        "skopeo inspect docker://${registryHost}/briard-fixture:v0 --format '{{.Digest}}'"
    ).strip()
    ref = f"${registryHost}/briard-fixture@{digest}"
    print(f"published {ref}")

    # === 2. a normal, zero-service promotion ===
    # The node has to be Primary with the volume mounted before anything can be written to it --
    # the same constraint the product's install path lives under, and the reason converge exists.
    node1.succeed("modprobe drbd")
    node1.succeed("drbdadm create-md --force r0")
    node1.succeed("systemctl start drbd@r0.target")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    node1.succeed("mkdir -p /run/briard && touch /run/briard/data.format")
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    print("promoted with zero services -- the shipped state, before anything is installed")

    # === 3. install a service this node has no image for ===
    # The manifest goes on the VOLUME, which is what converge reads. The data subvolume and its
    # subdirectories come from the RENDERER's own sidecars rather than being restated here, so the
    # only thing missing when converge runs is the image itself.
    manifest = (
        '{"name":"fixture","version":"v0","network":"host","containers":[{'
        f'"name":"app","image":"{ref}",'
        '"mount":"${fixtureMount}","primary":true,"port":8080,"healthPath":"/healthz"}]}'
    )
    node1.succeed(f"cat > /tmp/manifest.json <<'EOF'\n{manifest}\nEOF")
    node1.succeed("quadlet-render /tmp/manifest.json /tmp/rendered")
    dataroot = node1.succeed("cat /tmp/rendered/dataroot").strip()
    node1.succeed(f"btrfs subvolume show {dataroot} >/dev/null 2>&1 || btrfs subvolume create {dataroot}")
    for sub in node1.succeed("cat /tmp/rendered/subdirs").split():
        node1.succeed(f"mkdir -p {dataroot}/{sub}")
    # The .image unit's NAME comes from the renderer too -- a name restated here would assert our
    # own arithmetic about unit naming rather than watching the unit converge actually starts.
    image_unit = node1.succeed("cat /tmp/rendered/images").split()[0]
    print(f"the pre-warm unit converge will start: {image_unit}")
    node1.succeed("mkdir -p /var/lib/briard/.services")
    node1.succeed("cp /tmp/manifest.json /var/lib/briard/.services/fixture.json")
    node1.succeed("sync")
    # THE PREMISE, asserted rather than assumed: this node cannot serve without a pull.
    node1.fail(f"podman image exists {ref}")
    print("manifest installed on the volume; the image is absent -- converge must fetch it")

    # === 4. throttle the registry's link ===
    # tbf on the registry's egress, so the bytes the node is waiting for arrive at ${rate}. The
    # node's own link is untouched: a throttle on both ends would slow the test's own commands.
    zregistry.succeed(
        "tc qdisc replace dev eth1 root tbf rate ${rate} burst 4kb latency 400ms"
    )
    print(zregistry.succeed("tc -s qdisc show dev eth1"))

    def counters():
        return int(zregistry.succeed("cat /sys/class/net/eth1/statistics/tx_bytes").strip())

    tx0 = counters()

    # === 5. THE MEASUREMENT ===
    # --no-block, because the whole question is whether this ever returns. A blocking restart would
    # hang the test driver instead of measuring the hang.
    node1.succeed("systemctl reset-failed briard-services.service || true")
    t0 = time.monotonic()
    node1.succeed("systemctl --no-block restart briard-services.service")

    def probe():
        def show(unit, prop):
            return node1.succeed(f"systemctl show -p {prop} --value {unit} 2>/dev/null || true").strip()
        return {
            "t": round(time.monotonic() - t0, 1),
            "services": show("briard-services.service", "ActiveState") + "/" + show("briard-services.service", "SubState"),
            "result": show("briard-services.service", "Result"),
            "restarts": show("briard-services.service", "NRestarts"),
            "image_unit": node1.execute(f"systemctl show -p ActiveState --value {image_unit}")[1].strip(),
            "role": node1.execute("drbdadm role r0")[1].strip(),
            "mounted": "yes" if node1.execute("mountpoint -q /var/lib/briard")[0] == 0 else "no",
            "vip": "yes" if node1.execute("ip -4 addr show to 192.168.1.100")[1].strip() else "no",
            "masked": "yes" if node1.execute("systemctl is-enabled drbd-services@r0.target")[1].strip() == "masked" else "no",
            "tx_kb": (counters() - tx0) // 1024,
        }

    print("t     services              result    restarts image           role       mnt vip mask   txKB")
    settled = None
    while time.monotonic() - t0 < ${toString windowSecs}:
        p = probe()
        print(
            f"{p['t']:<5} {p['services']:<21} {p['result']:<9} {p['restarts']:<8} "
            f"{p['image_unit']:<15} {p['role']:<10} {p['mounted']:<3} {p['vip']:<3} {p['masked']:<6} {p['tx_kb']}"
        )
        # "Settled" = the start job is over, one way or the other. Recorded, not asserted: the
        # interesting outcome is the one where this never happens.
        if settled is None and p["services"].split("/")[1] not in ("start", "start-pre", "start-post", "auto-restart"):
            settled = p
        node1.sleep(15)

    final = probe()
    tx_total = final["tx_kb"]

    print("=" * 78)
    print("WINDOW: ${toString windowSecs}s at ${rate}")
    print(f"BYTES PULLED: {tx_total} KB -- the transfer was {'PROGRESSING' if tx_total > 64 else 'NOT PROGRESSING'}")
    if settled is None:
        print("VERDICT: the converge start job NEVER completed inside the window.")
        print(f"  briard-services: {final['services']} after {final['t']}s, restarts={final['restarts']}")
        print(f"  the node held the promotion throughout: role={final['role']} mounted={final['mounted']} vip={final['vip']}")
        print("  nothing bounded the pull: no timeout fired, the member never failed, so the")
        print("  demote-and-hand-over of [V3b.5](c) rules 5/7/8 was never reached.")
    else:
        print(f"VERDICT: the start job settled at t={settled['t']}s -> {settled['services']} result={settled['result']}")
        print(f"  something DOES bound it. Final: role={final['role']} vip={final['vip']} masked={final['masked']}")
    print("=" * 78)

    # The journal is the artifact a reader checks the timeline against.
    print(node1.succeed("journalctl -u briard-services.service --no-pager | tail -40"))
    print(node1.succeed(f"journalctl -u {image_unit} --no-pager | tail -20 || true"))
    print(node1.succeed("systemctl list-units --failed --no-pager || true"))

    # THE ONE ASSERTION, and it is about the RIG rather than the product: a window in which
    # nothing was transferred would have measured a stalled connection, not a slow one, and every
    # conclusion above would be about the wrong fault.
    assert tx_total > 64, f"the throttled pull moved only {tx_total} KB -- this measured a stall, not a slow transfer"
  '';
}
