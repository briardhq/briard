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
    while time.monotonic() - t0 < 240:
        st = node1.succeed("systemctl show -p ActiveState --value pull-act3").strip()
        if st != "activating":
            recovered = node1.execute(f"podman image exists {ref}")[0] == 0
            break
        node1.sleep(5)
    act3_total = tx() - c
    print("=" * 78)
    print(f"ACT 3: the pull {'RECOVERED on its own' if recovered else 'did NOT recover'} "
          f"after a 20s outage; {act3_total / MiB:.1f} MiB on the wire "
          f"({act3_total / baseline:.2f}x the baseline)")
    print(node1.succeed("journalctl -u pull-act3 --no-pager | tail -25"))
    print("=" * 78)
  '';
}
