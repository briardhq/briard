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

  # UNEVEN LAYERS ([B.56], act 4), and the unevenness is the whole point.
  #
  # The first cut of this act used SIX EQUAL 4 MiB layers and concluded "nothing is kept". That
  # conclusion was not measured. podman copies every layer concurrently, so six equal layers
  # progress in lockstep and all six cross any given percentage together: the registry log showed
  # attempt 1 serving 3407872 / 3211264 / 2392064 / 2228224 / 2883584 / 3276800 bytes against a
  # full layer of 4197350 -- every one partial, not one finished. There were no completed layers
  # to discard, so the rig could not have seen them discarded.
  #
  # Real images are not shaped like that: a handful of small layers and one big one is the normal
  # case, and the small ones finish long before the big one does. That is the only arrangement in
  # which "are finished layers kept?" is a question with an observable answer.
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

    # === ACT 4: ARE FINISHED LAYERS KEPT ACROSS A RESTART? ===
    # This decides how bad the 45-minute bound actually is. If a completed layer survives, a long
    # pull makes MONOTONIC progress across attempts -- only the layer in flight is lost each time
    # -- and the "never converges" risk shrinks to a single question: can the image's LARGEST
    # layer cross the household's link inside the window? If nothing survives, every attempt
    # really does start from zero and the bound has to clear the whole image.
    #
    # AGGREGATE BYTES CANNOT ANSWER THIS, which is what the first cut got wrong. It compared
    # totals and called a complete re-fetch "layers were discarded" -- but with six EQUAL layers
    # copied concurrently, nothing had finished at the interrupt, so there was no retained layer
    # to observe either way. The evidence has to be PER BLOB: which digests does the registry
    # serve on the second attempt, and which does it never hear about again?
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
    # Interrupt once the SMALL layers must be done but the big one cannot be: they share the
    # throttled link, so ~12 MiB total is well past 5 x 1 MiB and nowhere near the 20 MiB layer.
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
    # THE RIG'S OWN PRECONDITION: with nothing finished, this act cannot see retention and would
    # repeat the first cut's mistake.
    assert done1, "no layer completed before the interrupt — this act cannot answer its question"
    assert len(done1) < len(layer_size), "every layer completed — the interrupt was too late"

    throttle(False)
    t_attempt2 = int(zregistry.succeed("date +%s").strip())
    node1.succeed(f"podman image pull {lref}")
    node1.succeed(f"podman image exists {lref}")
    second = blob_gets(t_attempt2)

    refetched = sorted(done1 & set(second))
    kept = sorted(done1 - set(second))
    print("=" * 78)
    print(f"layers COMPLETED in attempt 1 : {len(done1)}")
    print(f"  re-fetched in attempt 2     : {len(refetched)} {[d[:12] for d in refetched]}")
    print(f"  never requested again       : {len(kept)} {[d[:12] for d in kept]}")
    if kept and not refetched:
        saved = sum(layer_size[d] for d in kept)
        print("VERDICT: FINISHED LAYERS ARE KEPT. The retry asked only for what it had not")
        print(f"  completed — {saved / MiB:.1f} MiB of finished work survived the interrupt. Progress")
        print("  across attempts is monotonic, so the bound only has to clear the image's")
        print("  LARGEST SINGLE LAYER, not the whole image.")
    else:
        print("VERDICT: FINISHED LAYERS ARE DISCARDED. A completed layer was fetched again, so")
        print("  every attempt starts from zero however the image is built, and the bound has to")
        print("  clear the entire image on the household's slowest plausible link.")
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
