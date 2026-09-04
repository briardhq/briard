# DOES AN INTERRUPTED `podman image pull` RESUME, OR START OVER? ([B.56] follow-on.)
#
# WHY IT DECIDES A DESIGN. [V3b.3](f) put a fetch on the promotion path, and cold-converge-pull
# measured that nothing bounds it: a slow pull holds the promotion forever, Primary with no VIP.
# The fix under consideration is a `TimeoutStartSec=` on the generated `.image` unit -- but a
# timeout is only half a strategy. What it buys is a RETRY: a fresh connection, possibly to a
# faster CDN mirror, and an escape from a transient network fault. That is worth having only if a
# retry keeps the bytes already on disk. If every attempt starts from zero, a bound set slightly
# too tight converts "slow but eventually serves" into "never converges" -- the household's link
# is re-downloading the same blob until something gives up for good.
#
# So the number that matters is not a duration. It is: after an interrupted pull, how many bytes
# does the SECOND attempt ask the registry for?
#
# THE ARITHMETIC, which is the whole design of this rig. Pull the same image twice and count the
# registry's transmitted bytes each time:
#
#   act 1  uninterrupted                        -> S bytes  (the baseline: one whole image)
#   act 2  interrupted at ~40%, then completed  -> S' bytes
#
#   S' ~= S       => the retry RESUMED: only the remainder crossed the wire
#   S' ~= 1.4 x S => NO RESUME: the first 40% was fetched again
#
# There is no interpretation step and no log to read: the two totals differ by more than a third,
# which no amount of protocol overhead or TLS renegotiation can blur.
#
# SIGTERM IS THE FAITHFUL INTERRUPTION. That is exactly what systemd sends when
# `TimeoutStartSec=` expires, so act 2 interrupts the pull the way the proposed fix would.
#
# ACT 3 IS THE CASE THAT PROMPTED THIS: a TRANSIENT fault, not a slow link. podman's own
# `--retry` (default 3) runs INSIDE one `podman image pull`, so a blip may never reach systemd at
# all. Act 3 blackholes the registry mid-transfer and restores it, and counts the bytes again --
# if podman recovers on its own and does not re-fetch, then transient faults need no timeout and
# the bound is only ever for the persistently-slow case.
#
# A PADDED, INCOMPRESSIBLE LAYER, so the transfer is big enough to interrupt in a controlled place
# and its size is a number this file chose rather than one dockerTools happened to produce.
#
# Rides `debug`: it is a measurement, and its assertions are about the RIG (that the interruption
# landed mid-transfer at all), never about a product behaviour we have decided on.
{ pkgs, ... }:
let
  registryIP = "10.0.0.99";
  registryHost = "${registryIP}:5000";

  padMiB = 24;
  # Fast enough that a whole pull is ~25s, slow enough that the interrupt lands where aimed.
  rate = "8mbit";

  certs = pkgs.runCommand "pull-resume-certs" { nativeBuildInputs = [ pkgs.openssl ]; } ''
    mkdir -p $out
    openssl req -x509 -newkey rsa:2048 -nodes -days 36500 \
      -subj "/CN=briard-test-ca" -keyout $out/ca.key -out $out/ca.crt 2>/dev/null
    openssl req -newkey rsa:2048 -nodes \
      -subj "/CN=${registryIP}" -keyout $out/server.key -out $out/server.csr 2>/dev/null
    printf 'subjectAltName=IP:${registryIP}\n' > $out/ext
    openssl x509 -req -in $out/server.csr -CA $out/ca.crt -CAkey $out/ca.key \
      -CAcreateserial -days 36500 -extfile $out/ext -out $out/server.crt 2>/dev/null
    chmod 0644 $out/server.key
  '';

  # INCOMPRESSIBLE ON PURPOSE: the layer is gzipped in transit, so compressible padding would
  # make the bytes on the wire unrelated to the size chosen here and the interrupt point
  # unpredictable.
  padding = pkgs.runCommand "pad-${toString padMiB}MiB" { } ''
    mkdir -p $out
    head -c ${toString (padMiB * 1024 * 1024)} /dev/urandom > $out/pad.bin
  '';
  fixtureImage = pkgs.dockerTools.buildImage {
    name = "briard-pad";
    tag = "v0";
    copyToRoot = padding;
    config.Cmd = [ "/bin/true" ];
  };

  # THE SAME PAYLOAD, SPLIT INTO LAYERS ([B.56], act 4). One store path per layer is how
  # buildLayeredImage cuts them, so N padding derivations give N distinct blobs of a known size --
  # and a real image is built this way, where acts 1-3's single 24 MiB blob is not.
  layerCount = 6;
  layerMiB = 4;
  layerPads = builtins.genList (
    i:
    pkgs.runCommand "layer-pad-${toString i}" { } ''
      mkdir -p $out/layer${toString i}
      head -c ${toString (layerMiB * 1024 * 1024)} /dev/urandom > $out/layer${toString i}/pad.bin
    ''
  ) layerCount;
  layeredImage = pkgs.dockerTools.buildLayeredImage {
    name = "briard-layered";
    tag = "v0";
    contents = layerPads;
    maxLayers = layerCount + 4;
    config.Cmd = [ "/bin/true" ];
  };
in
pkgs.testers.runNixOSTest {
  name = "image-pull-resume";
  skipTypeCheck = true;

  nodes = {
    # The PRODUCT's podman: the guest image takes it from this same nixpkgs module, so the
    # version under test is the version that ships. No DRBD, no promoter, no converge -- none of
    # that is in the question.
    node1 = {
      virtualisation.podman.enable = true;
      security.pki.certificateFiles = [ "${certs}/ca.crt" ];
      networking.interfaces.eth1.ipv4.addresses = [
        { address = "10.0.0.1"; prefixLength = 24; }
      ];
      virtualisation.diskSize = 4096; # the padded image is pulled more than once
    };
    zregistry = {
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
      virtualisation.diskSize = 4096;
    };
  };

  testScript = ''
    import time

    start_all()
    for m in [node1, zregistry]:
        m.wait_for_unit("multi-user.target")
    zregistry.wait_for_open_port(5000)

    zregistry.succeed(
        "skopeo copy docker-archive:${fixtureImage} docker://${registryHost}/briard-pad:v0"
    )
    digest = zregistry.succeed(
        "skopeo inspect docker://${registryHost}/briard-pad:v0 --format '{{.Digest}}'"
    ).strip()
    ref = f"${registryHost}/briard-pad@{digest}"
    print(f"published {ref}")

    zregistry.succeed(
        "skopeo copy docker-archive:${layeredImage} docker://${registryHost}/briard-layered:v0"
    )
    ldigest = zregistry.succeed(
        "skopeo inspect docker://${registryHost}/briard-layered:v0 --format '{{.Digest}}'"
    ).strip()
    print(f"published the layered image: ${registryHost}/briard-layered@{ldigest}")

    def tx():
        return int(zregistry.succeed("cat /sys/class/net/eth1/statistics/tx_bytes").strip())

    def drop_image():
        node1.execute(f"podman rmi -f {ref}")
        node1.fail(f"podman image exists {ref}")

    def throttle(on):
        if on:
            zregistry.succeed("tc qdisc replace dev eth1 root tbf rate ${rate} burst 32kb latency 400ms")
        else:
            zregistry.execute("tc qdisc del dev eth1 root")

    MiB = 1024 * 1024

    # === ACT 1: the baseline -- one whole image, uninterrupted ===
    drop_image()
    a = tx()
    node1.succeed(f"podman image pull {ref}")
    node1.succeed(f"podman image exists {ref}")
    baseline = tx() - a
    print(f"ACT 1 baseline: {baseline / MiB:.1f} MiB on the wire for a ${toString padMiB} MiB payload")

    # === ACT 2: interrupt at ~40%, then complete ===
    # SIGTERM is what `TimeoutStartSec=` sends, so this interrupts the pull exactly the way the
    # proposed bound would.
    drop_image()
    throttle(True)
    b = tx()
    node1.succeed(f"systemd-run --unit=pull-act2 --collect podman image pull {ref}")
    target = int(baseline * 0.4)
    t0 = time.monotonic()
    while tx() - b < target and time.monotonic() - t0 < 180:
        node1.sleep(1)
    partial = tx() - b
    node1.succeed("systemctl kill --signal=SIGTERM pull-act2 || true")
    node1.succeed("systemctl stop pull-act2 || true")
    node1.sleep(2)
    print(f"ACT 2 interrupted after {partial / MiB:.1f} MiB ({100 * partial / baseline:.0f}% of the baseline)")
    # THE RIG'S OWN ASSERTION: an interrupt that landed before the blob started, or after it
    # finished, would make the comparison below meaningless.
    assert partial > baseline * 0.15, f"the interrupt landed too early ({partial} bytes) to prove anything"
    assert partial < baseline * 0.9, f"the interrupt landed too late ({partial} bytes) to prove anything"
    node1.fail(f"podman image exists {ref}")  # an interrupted pull leaves no usable image

    # Complete it, at full speed: the question is how many bytes the SECOND attempt asks for.
    throttle(False)
    node1.succeed(f"podman image pull {ref}")
    node1.succeed(f"podman image exists {ref}")
    total = tx() - b
    second = total - partial
    print(f"ACT 2 second attempt: {second / MiB:.1f} MiB; total across both: {total / MiB:.1f} MiB")

    # === THE ANSWER ===
    ratio = total / baseline
    print("=" * 78)
    print(f"baseline (one whole pull) : {baseline / MiB:8.1f} MiB")
    print(f"interrupted then completed: {total / MiB:8.1f} MiB   ({ratio:.2f}x the baseline)")
    if ratio < 1.15:
        print("VERDICT: the retry RESUMED -- the second attempt fetched only the remainder.")
        print("  A timeout-and-retry bound keeps its progress, so retrying costs little and a")
        print("  slightly-too-tight bound is recoverable.")
    else:
        print("VERDICT: NO RESUME -- the second attempt re-fetched what the first had already got.")
        print(f"  {(total - baseline) / MiB:.1f} MiB crossed the wire twice. Every retry pays the")
        print("  full price again, so a bound below what the household's link needs never")
        print("  converges: it re-downloads until something gives up for good.")
    print("=" * 78)

    # === ACT 3: a TRANSIENT fault inside one pull ===
    # podman's own --retry (default 3) lives inside a single invocation, so a blip may never
    # reach systemd. If podman recovers here without re-fetching, transient faults need no
    # timeout at all and the bound is only ever for the persistently-slow case.
    drop_image()
    throttle(True)
    c = tx()
    node1.succeed(f"systemd-run --unit=pull-act3 --collect podman image pull {ref}")
    t0 = time.monotonic()
    while tx() - c < int(baseline * 0.3) and time.monotonic() - t0 < 180:
        node1.sleep(1)
    at_break = tx() - c
    print(f"ACT 3 breaking the path after {at_break / MiB:.1f} MiB")
    # Blackhole rather than down the link: an interface that disappears is a different fault from
    # a path that stops forwarding, and the latter is what a household actually sees.
    zregistry.succeed("tc qdisc replace dev eth1 root netem loss 100%")
    node1.sleep(20)
    zregistry.succeed("tc qdisc replace dev eth1 root tbf rate ${rate} burst 32kb latency 400ms")
    print("ACT 3 path restored after 20s")

    recovered = False
    t0 = time.monotonic()
    while time.monotonic() - t0 < 300:
        # BOTH "active" AND "activating" MEAN STILL RUNNING, and the first cut of this rig got
        # that wrong. A `systemd-run` transient service is Type=simple, so it reports `active`
        # from the moment it starts -- an `!= "activating"` test therefore fired on the FIRST
        # poll, printed "did NOT recover" 0.2s into a 20s outage, and finished the whole act in
        # under a second. The bytes it reported were the ones already on the wire before the
        # break. Nothing was measured; the number just looked like one.
        st = node1.succeed("systemctl show -p ActiveState --value pull-act3 || echo gone").strip()
        if st not in ("activating", "active"):
            break
        node1.sleep(5)
    recovered = node1.execute(f"podman image exists {ref}")[0] == 0
    act3_total = tx() - c
    print("=" * 78)
    print(f"ACT 3: the pull {'RECOVERED on its own' if recovered else 'did NOT recover'} "
          f"after a 20s outage (final unit state: {st}); {act3_total / MiB:.1f} MiB on the wire "
          f"({act3_total / baseline:.2f}x the baseline)")
    print(node1.succeed("journalctl -u pull-act3 --no-pager | tail -25"))
    print("=" * 78)

    # === ACT 4: MANY LAYERS -- is completed work kept? ===
    # Acts 1-3 used ONE 24 MiB layer, which measures resume WITHIN a blob and says nothing about
    # an image made of several. That is the shape real images have, and the distinction decides
    # how bad a timeout actually is: if finished layers are retained, a long pull makes monotonic
    # progress across attempts even though the layer in flight is always lost, and the "never
    # converges" worry applies only to an image whose SINGLE largest layer cannot fit in the
    # window. If they are not retained, every attempt truly starts from zero.
    lref = f"${registryHost}/briard-layered@{ldigest}"
    node1.execute(f"podman rmi -f {lref}")
    throttle(False)
    d = tx()
    node1.succeed(f"podman image pull {lref}")
    lbaseline = tx() - d
    print(f"ACT 4 layered baseline: {lbaseline / MiB:.1f} MiB across ${toString layerCount} layers")

    node1.succeed(f"podman rmi -f {lref}")
    throttle(True)
    e = tx()
    node1.succeed(f"systemd-run --unit=pull-act4 --collect podman image pull {lref}")
    t0 = time.monotonic()
    while tx() - e < int(lbaseline * 0.5) and time.monotonic() - t0 < 300:
        node1.sleep(1)
    lpartial = tx() - e
    node1.succeed("systemctl kill --signal=SIGTERM pull-act4 || true")
    node1.succeed("systemctl stop pull-act4 || true")
    node1.sleep(2)
    print(f"ACT 4 interrupted after {lpartial / MiB:.1f} MiB ({100 * lpartial / lbaseline:.0f}%)")

    throttle(False)
    node1.succeed(f"podman image pull {lref}")
    node1.succeed(f"podman image exists {lref}")
    ltotal = tx() - e
    lratio = ltotal / lbaseline
    print("=" * 78)
    print(f"layered baseline          : {lbaseline / MiB:8.1f} MiB  (${toString layerCount} layers)")
    print(f"interrupted then completed: {ltotal / MiB:8.1f} MiB   ({lratio:.2f}x)")
    if lratio < 1.25:
        print("VERDICT: COMPLETED LAYERS ARE KEPT -- only the layer in flight was re-fetched.")
        print("  Progress across attempts is monotonic, so a bound is far less dangerous than")
        print("  the single-layer measurement suggested: the risk narrows to an image whose")
        print("  LARGEST SINGLE LAYER cannot cross the link inside the window.")
    else:
        print("VERDICT: NOTHING IS KEPT -- the retry re-fetched layers it had already completed.")
        print("  Every attempt starts from zero regardless of how the image is built.")
    print("=" * 78)

    # === ACT 5: WHERE DO THE BYTES LIVE, AND WHEN ARE THEY SWEPT? ===
    # The guest has ONE filesystem for all of this (16 GiB root; the replicated volume holds only
    # service DATA), so a pull's scratch space competes with the OS closure, the image store and
    # the second system generation an OS upgrade stages. [B.49]'s host ENOSPC and the guest disk
    # sizing note in disk-image.nix are the same question from the other side.
    def usage():
        def kb(p):
            out = node1.execute(f"du -sk {p} 2>/dev/null | cut -f1")[1].strip()
            return int(out) if out.isdigit() else 0
        return {"/var/tmp": kb("/var/tmp"), "storage": kb("/var/lib/containers/storage")}

    node1.succeed(f"podman rmi -f {lref} {ref} || true")
    before = usage()
    throttle(True)
    node1.succeed(f"systemd-run --unit=pull-act5 --collect podman image pull {ref}")
    node1.sleep(12)
    during = usage()
    print(f"ACT 5 mid-pull /var/tmp contents:\n{node1.succeed('ls -la /var/tmp || true')}")
    node1.succeed("systemctl kill --signal=SIGTERM pull-act5 || true")
    node1.succeed("systemctl stop pull-act5 || true")
    node1.sleep(5)
    after = usage()
    print("=" * 78)
    print("                 /var/tmp      storage")
    print(f"before pull   {before['/var/tmp']:9d} KB {before['storage']:9d} KB")
    print(f"mid-pull      {during['/var/tmp']:9d} KB {during['storage']:9d} KB")
    print(f"after SIGTERM {after['/var/tmp']:9d} KB {after['storage']:9d} KB")
    grew = "/var/tmp" if during["/var/tmp"] - before["/var/tmp"] > 1024 else "storage"
    print(f"the in-flight bytes land in: {grew}")
    leaked = after["/var/tmp"] - before["/var/tmp"]
    print(f"left behind in /var/tmp after an interrupted pull: {leaked} KB "
          f"({'SWEPT' if leaked < 1024 else 'NOT SWEPT — this accumulates'})")
    print(node1.succeed("ls -la /var/tmp || true"))
    print("=" * 78)
    throttle(False)

    # === ACT 6: DOES PrivateTmp=true SWEEP THE LEAK? ===
    # Act 5 found the time bomb: every INTERRUPTED pull abandons its scratch directory in
    # /var/tmp, and the 45-minute bound is a machine for producing interrupted pulls. On the
    # 16 GiB root a Home Assistant image leaks ~2.7 GB per expiry, against the ~11 GB free that
    # disk-image.nix sized for one service plus its upgrade.
    #
    # PrivateTmp=true is the candidate because it needs no cleanup code: systemd gives the unit
    # its own /tmp and /var/tmp and removes them when the unit stops, however it stops.
    #
    # TWO THINGS HAVE TO HOLD, and the second is the one that could bite. The sweep must work on
    # the interrupt path (that is the point), AND the success path must be unaffected -- if the
    # private /var/tmp were TMPFS-backed, the copy would run through RAM and a multi-GB image
    # would OOM a guest instead of filling its disk, which trades a slow leak for a fast crash.
    # So this act measures the filesystem behind it rather than trusting the flag's name.
    node1.succeed("rm -rf /var/tmp/container_images_storage* || true")
    print(node1.succeed("findmnt -n -o TARGET,SOURCE,FSTYPE --target /var/tmp || true"))
    node1.succeed("free -m | head -2")

    # -- 6a: the SUCCESS path still works, and does not leak either
    node1.execute(f"podman rmi -f {ref}")
    p_before = usage()
    node1.succeed(
        f"systemd-run --unit=pull-act6a --collect --property=PrivateTmp=true "
        f"--wait podman image pull {ref}"
    )
    node1.succeed(f"podman image exists {ref}")
    p_after_ok = usage()
    print(f"ACT 6a success under PrivateTmp: image present, "
          f"/var/tmp {p_before['/var/tmp']} -> {p_after_ok['/var/tmp']} KB, "
          f"storage {p_before['storage']} -> {p_after_ok['storage']} KB")

    # -- 6b: the INTERRUPT path -- the case act 5 caught leaking
    node1.succeed(f"podman rmi -f {ref}")
    throttle(True)
    q_before = usage()
    node1.succeed(
        "systemd-run --unit=pull-act6b --collect --property=PrivateTmp=true "
        f"podman image pull {ref}"
    )
    node1.sleep(12)
    q_during = usage()
    # Where the private scratch actually lives, and on what. If this is empty while the pull is
    # plainly moving bytes, the copy is NOT on the host's /var/tmp and the RAM question is live.
    print(node1.succeed("ls -la /var/tmp | grep -i private || echo '(no systemd-private dir under /var/tmp)'"))
    print(node1.succeed("df -h /var/tmp | tail -1"))
    node1.succeed("systemctl kill --signal=SIGTERM pull-act6b || true")
    node1.succeed("systemctl stop pull-act6b || true")
    node1.sleep(5)
    q_after = usage()
    throttle(False)

    leak6 = q_after["/var/tmp"] - q_before["/var/tmp"]
    grew6 = q_during["/var/tmp"] - q_before["/var/tmp"]
    print("=" * 78)
    print("                 /var/tmp      storage")
    print(f"before pull   {q_before['/var/tmp']:9d} KB {q_before['storage']:9d} KB")
    print(f"mid-pull      {q_during['/var/tmp']:9d} KB {q_during['storage']:9d} KB")
    print(f"after SIGTERM {q_after['/var/tmp']:9d} KB {q_after['storage']:9d} KB")
    print(f"ACT 6b: in-flight bytes visible under the host /var/tmp: {grew6} KB")
    if leak6 < 1024:
        print(f"VERDICT: PrivateTmp SWEEPS IT -- {leak6} KB left after an interrupted pull.")
        print("  The bound no longer leaks: systemd tears the private scratch down on stop,")
        print("  whatever killed the unit, with no cleanup code of ours to get wrong.")
    else:
        print(f"VERDICT: STILL LEAKS -- {leak6} KB left behind. PrivateTmp is not the fix;")
        print("  the scratch must be swept explicitly (ExecStopPost) or relocated.")
    if grew6 < 1024:
        print("  ⚠️ AND THE BYTES NEVER APPEARED ON THE HOST'S /var/tmp: the private scratch is")
        print("     not disk-backed where act 5's was. Check the df/findmnt lines above before")
        print("     shipping this — a tmpfs here turns a 2.7 GB image into 2.7 GB of RAM.")
    print(node1.succeed("ls -la /var/tmp || true"))
    print("=" * 78)
  '';
}
