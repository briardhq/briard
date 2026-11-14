# Does an interrupted `podman image pull` resume, or start over? (A follow-on to the
# cold-converge-pull stopwatch.)
#
# Converge-at-promotion puts a fetch on the promotion path, and cold-converge-pull shows that,
# unbounded, a slow pull holds the promotion forever, Primary with no VIP. The bound is a
# `TimeoutStartSec=` on the generated `.image` unit (agent/quadlet/render.go, PullTimeout), but a
# timeout only buys a retry (a fresh connection, maybe a faster CDN mirror, an escape from a
# transient fault), worth having only if the retry keeps the bytes already on disk. If every
# attempt starts from zero, a bound set slightly too tight turns "slow but eventually serves"
# into "never converges": the household link re-downloads the same blob until something gives up
# for good. So the number that matters is not a duration but how many bytes the second attempt
# asks the registry for.
#
# The arithmetic is the whole design of this rig. Pull the same image twice and count the
# registry transmitted bytes each time:
#
#   act 1  uninterrupted                        -> S bytes  (the baseline: one whole image)
#   act 2  interrupted at ~40%, then completed  -> S' bytes
#
#   S' ~= S       => the retry RESUMED: only the remainder crossed the wire
#   S' ~= 1.4 x S => NO RESUME: the first 40% was fetched again
#
# No interpretation step: the totals differ by more than a third, which no protocol overhead or
# TLS renegotiation can blur. SIGTERM is the faithful interruption: systemd sends it when
# `TimeoutStartSec=` expires, so act 2 interrupts the pull the way the shipped bound does.
#
# Act 3 is the case that prompted this, a transient fault rather than a slow link: podman
# `--retry` (default 3) runs inside one `podman image pull`, so a blip may never reach systemd.
# Act 3 blackholes the registry mid-transfer, restores it and counts again; if podman recovers
# without re-fetching, transient faults need no timeout and the bound is only for the
# persistently slow case.
#
# A padded, incompressible layer makes the transfer big enough to interrupt in a controlled place,
# at a size this file chose rather than one dockerTools produced. Rides `debug`: a measurement,
# whose assertions are about the rig (that the interruption landed mid-transfer), never about a
# product behaviour we have decided on.
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

  # Incompressible on purpose: the layer is gzipped in transit, so compressible padding would
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

  # Uneven layers, and the unevenness is the point. podman copies every layer concurrently, so
  # equal layers progress in lockstep and are all partial at the interrupt: with no completed
  # layer to discard the rig cannot see one discarded, and "nothing is kept" would read as a
  # result. A few small layers and one big one, the small ones finishing long before, is the
  # normal shape of a real image, and only that shape gives "are finished layers kept?" an
  # observable answer.
  smallLayers = 5;
  smallMiB = 1;
  bigMiB = 20;
  unevenPads = builtins.genList (
    i:
    pkgs.runCommand "small-pad-${toString i}" { } ''
      mkdir -p $out/small${toString i}
      head -c ${toString (smallMiB * 1024 * 1024)} /dev/urandom > $out/small${toString i}/pad.bin
    ''
  ) smallLayers
  ++ [
    (pkgs.runCommand "big-pad" { } ''
      mkdir -p $out/big
      head -c ${toString (bigMiB * 1024 * 1024)} /dev/urandom > $out/big/pad.bin
    '')
  ];
  layeredImage = pkgs.dockerTools.buildLayeredImage {
    name = "briard-uneven";
    tag = "v0";
    contents = unevenPads;
    maxLayers = smallLayers + 5;
    config.Cmd = [ "/bin/true" ];
  };
in
pkgs.testers.runNixOSTest {
  name = "image-pull-resume";
  skipTypeCheck = true;

  nodes = {
    # The product podman: the guest image takes it from this same nixpkgs module, so the version
    # under test is the one that ships. No DRBD, promoter or converge: none is in the question.
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
    import json
    import re
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
        "skopeo copy docker-archive:${layeredImage} docker://${registryHost}/briard-uneven:v0"
    )
    ldigest = zregistry.succeed(
        "skopeo inspect docker://${registryHost}/briard-uneven:v0 --format '{{.Digest}}'"
    ).strip()
    print(f"published the uneven-layer image: ${registryHost}/briard-uneven@{ldigest}")

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

    # === Act 1: the baseline -- one whole image, uninterrupted ===
    drop_image()
    a = tx()
    node1.succeed(f"podman image pull {ref}")
    node1.succeed(f"podman image exists {ref}")
    baseline = tx() - a
    print(f"ACT 1 baseline: {baseline / MiB:.1f} MiB on the wire for a ${toString padMiB} MiB payload")

    # === Act 2: interrupt at ~40% with SIGTERM (what the bound sends), then complete ===
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
    # The rig assertion: an interrupt before the blob started or after it finished would make the
    # comparison meaningless.
    assert partial > baseline * 0.15, f"the interrupt landed too early ({partial} bytes) to prove anything"
    assert partial < baseline * 0.9, f"the interrupt landed too late ({partial} bytes) to prove anything"
    node1.fail(f"podman image exists {ref}")  # an interrupted pull leaves no usable image

    # Complete it at full speed: the question is how many bytes the second attempt asks for.
    throttle(False)
    node1.succeed(f"podman image pull {ref}")
    node1.succeed(f"podman image exists {ref}")
    total = tx() - b
    second = total - partial
    print(f"ACT 2 second attempt: {second / MiB:.1f} MiB; total across both: {total / MiB:.1f} MiB")

    # === The answer ===
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

    # === Act 3: a transient fault inside one pull (see the header) ===
    drop_image()
    throttle(True)
    c = tx()
    node1.succeed(f"systemd-run --unit=pull-act3 --collect podman image pull {ref}")
    t0 = time.monotonic()
    while tx() - c < int(baseline * 0.3) and time.monotonic() - t0 < 180:
        node1.sleep(1)
    at_break = tx() - c
    print(f"ACT 3 breaking the path after {at_break / MiB:.1f} MiB")
    # Blackhole rather than down the link: a vanished interface is a different fault from a path
    # that stops forwarding, which is what a household actually sees.
    zregistry.succeed("tc qdisc replace dev eth1 root netem loss 100%")
    node1.sleep(20)
    zregistry.succeed("tc qdisc replace dev eth1 root tbf rate ${rate} burst 32kb latency 400ms")
    print("ACT 3 path restored after 20s")

    recovered = False
    t0 = time.monotonic()
    while time.monotonic() - t0 < 300:
        # Both "active" and "activating" mean still running: a `systemd-run` transient service is
        # Type=simple, `active` from the start, so an `!= "activating"` test would fire on the
        # first poll, print "did NOT recover" a moment into the outage and report the bytes on the
        # wire before the break: a number that only looks like a measurement.
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

    # === Act 4: are finished layers kept across a restart? ===
    # This decides how bad the pull bound (45 minutes for an unsized entry) is. If a completed
    # layer survives, progress across attempts is monotonic (only the layer in flight is lost) and
    # the "never converges" risk reduces to whether the largest layer crosses the household link
    # inside the window; if nothing survives, the bound has to clear the whole image. Aggregate
    # bytes cannot answer this (a total reads a complete re-fetch as "discarded" even when nothing
    # had finished), so the evidence is per blob: which digests the registry serves on the second
    # attempt, and which it never hears about again.
    lref = f"${registryHost}/briard-uneven@{ldigest}"
    raw = zregistry.succeed(
        "skopeo inspect --raw docker://${registryHost}/briard-uneven:v0"
    )
    layer_size = {
        l["digest"].split(":")[1]: l["size"] for l in json.loads(raw)["layers"]
    }
    print(f"ACT 4 layer sizes: {sorted((s // 1024, d[:12]) for d, s in layer_size.items())}")

    def blob_gets(since):
        """Which layer blobs the registry served since `since`, and how many bytes of each.
        Read from the registry's OWN access log: a layer that is not re-requested does not appear,
        which is direct evidence rather than an inference from a total."""
        log = zregistry.succeed(
            f"journalctl -u docker-registry --since @{since} --no-pager -o cat || true"
        )
        served = {}
        for m in re.finditer(
            r'GET /v2/briard-uneven/blobs/sha256:([0-9a-f]+) HTTP/1\.1" 200 ([0-9]+)', log
        ):
            d, n = m.group(1), int(m.group(2))
            if d in layer_size:
                served[d] = max(served.get(d, 0), n)
        return served

    node1.execute(f"podman rmi -f {lref}")
    throttle(True)
    t_attempt1 = int(zregistry.succeed("date +%s").strip())
    e = tx()
    node1.succeed(f"systemd-run --unit=pull-act4 --collect podman image pull {lref}")
    # Interrupt once the small layers must be done and the big one cannot be: on the shared
    # throttled link ~12 MiB total is well past 5 x 1 MiB and nowhere near the 20 MiB layer.
    t0 = time.monotonic()
    while tx() - e < 12 * MiB and time.monotonic() - t0 < 300:
        node1.sleep(1)
    node1.succeed("systemctl kill --signal=SIGTERM pull-act4 || true")
    node1.succeed("systemctl stop pull-act4 || true")
    node1.sleep(3)
    first = blob_gets(t_attempt1)
    done1 = {d for d, n in first.items() if n >= layer_size[d]}
    print(f"ACT 4 attempt 1 interrupted after {(tx() - e) / MiB:.1f} MiB; "
          f"{len(done1)} of {len(layer_size)} layers had COMPLETED")
    for d, n in sorted(first.items(), key=lambda kv: -layer_size[kv[0]]):
        print(f"    {d[:12]}  {n:9d} / {layer_size[d]:9d}  {'complete' if d in done1 else 'partial'}")
    # The rig precondition: with nothing finished this act cannot see retention and would report
    # a result it never measured.
    assert done1, "no layer completed before the interrupt — this act cannot answer its question"
    assert len(done1) < len(layer_size), "every layer completed — the interrupt was too late"

    throttle(False)
    t_attempt2 = int(zregistry.succeed("date +%s").strip())
    node1.succeed(f"podman image pull {lref}")
    node1.succeed(f"podman image exists {lref}")
    second = blob_gets(t_attempt2)

    refetched = sorted(done1 & set(second))
    kept = sorted(done1 - set(second))
    saved = sum(layer_size[d] for d in kept)
    paid = sum(layer_size[d] for d in refetched)
    print("=" * 78)
    print(f"layers COMPLETED in attempt 1 : {len(done1)}")
    print(f"  re-fetched in attempt 2     : {len(refetched)} {[d[:12] for d in refetched]} = {paid} B")
    print(f"  never requested again       : {len(kept)} {[d[:12] for d in kept]} = {saved} B")
    # Weighed in bytes, not counted: a count calls one re-fetched blob "discarded" and prints the
    # wrong verdict over the right data (five 1 MiB layers kept, the re-fetched one 450 bytes).
    # Convergence is decided by megabytes; a rule letting 450 bytes outvote 5 MiB of retained
    # work measures its own arithmetic, not podman behaviour.
    if saved > 0 and paid < saved // 20:
        print("VERDICT: FINISHED LAYERS ARE KEPT. The retry asked only for what it had not")
        print(f"  finished — {saved / MiB:.1f} MiB of completed work survived, against {paid} B re-fetched.")
        print("  Progress across attempts is MONOTONIC, so the bound only has to clear the image's")
        print("  LARGEST SINGLE LAYER, not the whole image.")
    else:
        print("VERDICT: FINISHED LAYERS ARE DISCARDED. Completed layers were fetched again, so")
        print("  every attempt starts from zero however the image is built, and the bound has to")
        print("  clear the entire image on the household's slowest plausible link.")
    print("=" * 78)

    # === Act 5: where do the bytes live, and when are they swept? ===
    # In the guest one filesystem, the state disk, holds both /var (scratch, emptied at boot) and,
    # in its persistent half, the image store (the replicated volume holds only app data), so pull
    # scratch competes with the image store. The state disk in disk-image.nix is the same
    # question from the other side.
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

    # === Act 6: does PrivateTmp=true sweep the leak? ===
    # Act 5 measures the time bomb: an interrupted pull abandons its scratch directory in
    # /var/tmp, and the pull bound is a machine for interrupted pulls; on the guest a Home
    # Assistant pull leaks up to its whole compressed download (622 MB) per expiry until the next
    # boot empties the scratch, and podman never comes back for it. The .image unit ships
    # PrivateTmp=true (agent/quadlet/render.go) because it needs no cleanup code: systemd gives
    # the unit its own /tmp and /var/tmp and removes them when it stops, however it stops.
    #
    # Two things must hold, and the second could bite: the sweep works on the interrupt path, and
    # the success path is unaffected. A tmpfs-backed private /var/tmp would run the copy through
    # RAM and OOM the guest on a multi-GB image, trading a slow leak for a fast crash, so this act
    # measures the filesystem behind it rather than trusting the flag name.
    node1.succeed("rm -rf /var/tmp/container_images_storage* || true")
    print(node1.succeed("findmnt -n -o TARGET,SOURCE,FSTYPE --target /var/tmp || true"))
    node1.succeed("free -m | head -2")

    # -- 6a: the success path still works, and does not leak either
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

    # -- 6b: the interrupt path -- the case act 5 caught leaking
    node1.succeed(f"podman rmi -f {ref}")
    throttle(True)
    q_before = usage()
    node1.succeed(
        "systemd-run --unit=pull-act6b --collect --property=PrivateTmp=true "
        f"podman image pull {ref}"
    )
    node1.sleep(12)
    q_during = usage()
    # Where the private scratch lives, and on what. Empty while the pull is plainly moving bytes
    # means the copy is not on the host /var/tmp, and the RAM question is live.
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
        print("     shipping this — a tmpfs here turns a 622 MB download into 622 MB of RAM.")
    print(node1.succeed("ls -la /var/tmp || true"))
    print("=" * 78)
  '';
}
