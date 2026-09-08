# B.10's last rung: REBUILD THE GUEST FROM THE IMAGE UNDER IT, AND KEEP THE DATA.
#
# `briard rescue` discards the guest's OS-disk overlay and lays down a fresh one on the signed
# backing image it was installed from. The claim it makes -- and the only reason the verb is safe
# to offer -- is that the CODE half is disposable while the DATA half is not: the replicated volume
# is a separate disk, untouched, so what comes back is the same node with a factory guest rather
# than a new node.
#
# THE DIVISION OF LABOUR MATTERS HERE, because the obvious in-VM assertions are all wrong.
# Whether the overlay's CONTENTS were really discarded is proven in platform/overlay_test.go
# against real qemu-img, by planting a snapshot and showing the rebuilt overlay no longer carries
# it. Two proxies for that were tried in this file and both lie: SIZE, because the rebuilt guest
# boots and dirties its overlay before the verb even returns; and INODE, which looks exact and is
# not -- rm + create at the same path reuses the inode number, and an early version of this test
# failed on exactly that, reporting a rescue that had in fact worked.
#
# What only a real node can show is the INTEGRATION: a blank guest coming up against a populated
# data disk. So this asserts:
#
#   1. a DIFFERENT QEMU is serving afterwards -- the VM went down and came back
#   2. the data disk is the SAME file, same size, DRBD metadata intact
#   3. the rebuilt guest converges and serves again
#   4. it is still an overlay on the same image, so it can be rescued a second time
#   5. without -yes the verb refuses and the guest is left running
#
# (1) and (5) are what stop this passing vacuously: a rescue that quietly did nothing satisfies
# (2), (3) and (4) perfectly -- data intact, node serving, disk still an overlay -- which is
# exactly what a no-op looks like from the outside.
#
# WHAT IT DOES NOT PROVE, said here rather than left to be assumed: that bring-up ADOPTED the
# existing replica rather than re-seeding it in place. A re-seed rewrites metadata in the same
# file, keeping both inode and size, and byte-comparing will not separate the two either because
# DRBD legitimately rewrites its metadata on attach. Proving adoption needs real service data in
# the volume (the catalogued fixture the data tests install); that is worth building and is
# deliberately not smuggled in here as an assertion that would look stronger than it is.
#
# Heavy (a nested VM + the shipped guest disk) -> the `integration` tag. Run on the L0:
#   gh workflow run vm-test.yml -f test=guest-rescue
{ pkgs, guestDisk, agent, netWrap, dressBase, stub, channel, nextSystem }:
pkgs.testers.runNixOSTest {
  name = "guest-rescue";
  skipTypeCheck = true; # systemd-run + dynamic asserts

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 4096;
      virtualisation.cores = 4;
      virtualisation.diskSize = 16384; # the image copy, a staged image and a set-aside one coexist during an upgrade ([B.86h])
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ]; # nested KVM
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl pkgs.e2fsprogs ]; # debugfs reads the state disk
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # Same L2 as agent-recover, and it is the SHIPPED NIC contract rather than a rig convenience:
    # veth parent, the guest's two LAN NICs as macvtap children in install.sh's order (sys0 -> eth1,
    # svc0 -> eth2), and the private host<->guest link as a plain tap at 10.11.9.1/24. The macvlan
    # shim this used to build was the rig granting itself reachability the product lacked; the VIP
    # curls below now pass because the agent routes it over the private link ([V3b.19a]).
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )

    # THE OVERLAY IS THE POINT: the guest disk must be a qcow2 overlay on the shipped image, the
    # shape install.sh lays down. A standalone copy would make `rescue` refuse (correctly), so a
    # test built on one would prove nothing about the path users have.
    #
    # --force-share on every `qemu-img info` here for the same reason the product needs it: QEMU
    # holds a write lock on a running guest's disk and qemu-img declines a locked image without it.
    # This test hit that on its own final assertion after the fix had landed in the product, which
    # is a small piece of evidence that the fix was addressing something real rather than a quirk
    # of one environment.
    # A WRITABLE copy of the image, because [B.86h] swaps the file the overlay backs onto and
    # the store is read-only; install.sh lays the image down as a copy too.
    host.succeed("cp ${guestDisk}/nixos.qcow2 /tmp/nixos.qcow2 && chmod 0644 /tmp/nixos.qcow2")
    host.succeed("qemu-img create -f qcow2 -b /tmp/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")
    # The state disk ([B.86g]): empty, sparse; the guest formats it on its first boot and the
    # rescue below must NOT format it again -- that is the whole claim of the disk.
    host.succeed("truncate -s 2G /tmp/state.img")
    # The release keyring the agent verifies guest releases against ([B.86h]) is read at agent
    # START, so it is minted before the launch and used by the channel section below.
    host.succeed("${stub}/bin/briard-selfupdate-stub keygen /root/release.key /root/keyring.pem")
    backing = host.succeed("qemu-img info --output=json --force-share /tmp/guest.qcow2")
    assert "nixos.qcow2" in backing, f"the guest disk is not an overlay on the image; rescue would refuse:\n{backing}"

    # The host holds a guest bundle tree, as install.sh lays on every install ([B.138]): the image
    # bakes no door, so a guest is dressed by its host or it cannot serve. Copied out of the store
    # because the host writes `guest.good` beside the tree.
    host.succeed("mkdir -p /opt/briard/agent && cp -r ${dressBase}/. /opt/briard/agent/ && chmod -R u+w /opt/briard/agent")

    host.succeed(
        "systemd-run --unit=briard-agent --collect "
        # The PATH install.sh gives the shipped unit (scripts/install.sh, "Environment=PATH="). The
        # agent shells out to systemd-run, systemctl and -- since [V3b.19] -- `ip`, all BY NAME, and
        # a transient unit's default PATH resolves none of them reliably. Pinning the shipped value
        # is the point: the rig gets what the product gets ([V3b.19a]).
        "--setenv=PATH=/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin "
        "--setenv=QEMU=${pkgs.qemu}/bin/qemu-system-x86_64 --setenv=ACCEL=kvm:tcg "
        # Where the host keeps its guest bundle tree ([B.138]), the way install.sh sets it.
        "--setenv=UPDATE_BASE=/opt/briard/agent "
        "--setenv=GUEST_DISK=/tmp/guest.qcow2 --setenv=GUEST_IMAGE=/tmp/nixos.qcow2 --setenv=DATA_DISK=/tmp/data.img --setenv=STATE_DISK=/tmp/state.img "
        # The guest chain ([B.86h]): the channel this rig serves, the keyring it mints, the record.
        "--setenv=CHANNEL_URL=http://127.0.0.1:8099 --setenv=UPDATE_KEYRING=/root/keyring.pem --setenv=GUEST_RELEASE_CACHE=/tmp/guest-release.json "
        "--setenv=CONTROL_SOCK=/run/briard-ctl.sock --setenv=ADMIN_SOCK=/run/briard/admin.sock "
        "--setenv=NODE=guest --setenv=SYSTEM_TAP=sys0 --setenv=SYSTEM_DEV=eth1 --setenv=SYSTEM_CIDR=10.0.0.1/24 --setenv=SYSTEM_HOST_CIDR=10.0.0.129/32 --setenv=WITNESS_CIDR=10.11.9.2/24 --setenv=SERVICE_TAP=svc0 --setenv=WITNESS_TAP=briard-priv0 --setenv=STATUS_EVERY=2s "
        "--setenv=VIP_DEV=eth2 --setenv=VIP_ADDR=192.168.1.100/24 "
        "--setenv=NET_MODE=macvtap --setenv=NET_WRAP_BIN=${netWrap}/bin/briard-net-wrap "
        # GUEST_SERIAL is the only window into the guest during a stop, and it is why [B.85] sat
        # unexplained: the host watches the VM's systemd unit and has no console on what is
        # inside it, so 90 seconds of a guest ignoring `os.poweroff` and 90 seconds of a guest
        # shutting down slowly look identical from out here. The chardev APPENDS across launches
        # (platform.qemuArgs), so one file holds the guest that was stopped AND the rebuilt one.
        "--setenv=GUEST_SERIAL=/tmp/guest-console.log "
        "${agent}/bin/briard-agent run"
    )
    try:
        host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    except Exception:
        # A guest that never converged is diagnosable only from inside it: dump its console
        # ([[guest-console-is-the-window]]) and the guest unit's own stderr before failing.
        print("=== guest console (tail) ===")
        print(host.succeed("tr -d '\\r' < /tmp/guest-console.log | tail -200 || true"))
        print(host.succeed("journalctl -u briard-guest.service --no-pager | tail -40 || true"))
        print(host.succeed("pgrep -af qemu-system-x86_64 || true; ls -la /run/briard/ /run/briard/qmp/ || true"))
        raise
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)

    # === What to look at before the rescue. ===
    #
    # THE OS DISK'S REPLACEMENT IS NOT ASSERTED HERE, and that is a deliberate division rather than
    # a gap. It is proven in platform/overlay_test.go against real qemu-img, by planting a snapshot
    # in the overlay and showing the rebuilt one no longer carries it -- a content check, which is
    # what "replaced" actually means. Two in-VM proxies for it were tried and both are wrong:
    # SIZE, because the rebuilt guest boots and dirties its overlay before the verb even returns;
    # and INODE, which looked exact and is not -- rm + create at the same path reuses the inode
    # number on a busy filesystem, and this test failed on precisely that, reporting a rescue that
    # had in fact worked. What this test is FOR is the integration the unit test cannot reach: a
    # blank guest coming up against a populated data disk.
    #
    # The QEMU PID stands in for "the VM really went down and came back", which is the part of the
    # sequence this harness can see honestly.
    qemu_before = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]

    # The DATA disk carries the opposite assertion: it must be the SAME file afterwards, same size,
    # still holding DRBD's metadata.
    data_inode = host.succeed("stat -c %i /tmp/data.img").strip()
    data_size = host.succeed("stat -c %s /tmp/data.img").strip()
    zeros = host.succeed("head -c 4096 /dev/zero | sha256sum").strip().split()[0]
    data_tail = host.succeed("tail -c 4096 /tmp/data.img | sha256sum").strip().split()[0]
    assert data_tail != zeros, "the data disk tail is all zeroes -- DRBD never seeded it, so the survival assertions below would be vacuous"
    print(f"before rescue: qemu {qemu_before}, data inode {data_inode}, data tail {data_tail[:12]}")

    # === THE RESCUE ===
    # `briard-agent <verb>` IS the CLI (a bare first argument is a subcommand, main.go) -- the
    # `briard` name is a symlink install.sh makes, which this harness does not have.
    #
    # Without -yes it must refuse and touch nothing: the guard belongs to the verb, not to the
    # operator's memory, and asserting it here means a change that drops it fails a test rather
    # than a node.
    host.fail("${agent}/bin/briard-agent rescue -sock /run/briard/admin.sock")
    assert qemu_before == host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0], \
        "an unconfirmed rescue took the guest down anyway"

    # WHERE THE SHUTDOWN STARTS IN THE CONSOLE, marked before it happens. The chardev APPENDS
    # across launches (see GUEST_SERIAL above), so by the time the assertions below run the file
    # ends with the REBUILT guest's boot -- a `tail` of it shows nsncd starting, not the stop that
    # failed. Measured, by dumping a tail and getting exactly that. Marking the line count here and
    # slicing from it is what makes the dump show the shutdown.
    #
    # Counted through the same `tr` the dump uses: collapsing carriage returns CHANGES the line
    # count, so a mark taken any other way indexes into a different file.
    console_mark = int(host.succeed("tr '\\r' '\\n' < /tmp/guest-console.log | wc -l").strip())
    # The state disk's ext4 UUID (superblock at 1024, s_uuid at +0x68), before the rescue
    # ([B.86g]): read from the host side, with no guest cooperation.
    state_uuid = host.succeed("dd if=/tmp/state.img bs=1 skip=1128 count=16 2>/dev/null | od -An -tx1 | tr -d ' \\n'").strip()
    assert state_uuid and state_uuid != "0" * 32, "the state disk carries no filesystem before the rescue -- the guest never formatted it"

    host.succeed("${agent}/bin/briard-agent rescue -yes -sock /run/briard/admin.sock")
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'rescue: the guest was rebuilt and has re-converged'",
        timeout=900,
    )

    # === [B.85]: THE CLEAN STOP MUST ACTUALLY BE THE CLEAN ROUTE ===
    # The stop above goes through host.stopCleanly, which asks the guest agent first (`os.poweroff`
    # -> `systemctl poweroff --no-block`) and keeps the ACPI power button as the fallback for a
    # guest whose agent is gone. It was measured taking the fallback EVERY time on a healthy node,
    # and the reason was invisible from out here: the host watches its guest's systemd unit and has
    # no console on what is inside it, so "the request was ignored" and "the shutdown is stuck" look
    # identical. GUEST_SERIAL above is what made the difference legible, and it is why it is set.
    #
    # What it showed: the shutdown STARTED a second after the request, then drbd-reactor deadlocked
    # on its own stop for a full 90s TimeoutStopSec and was SIGKILLed -- the promote-vs-stop
    # deadlock of [B.28], on the shutdown path, where nothing was
    # defusing it. Fixed on drbd-reactor.service's ExecStop (guest-image/configuration.nix).
    #
    # TWO ASSERTIONS, because either alone passes for the wrong reason. The fallback line proves
    # the AGENT route worked -- a guest that still deadlocks reaches the power button, and its
    # absence is the whole claim. The console proves WHY, and guards the case where some future
    # stop hangs on a different unit: a deadlock that moved would still be silent up here.
    # (\r stripped -- it is a serial console.)
    stopleg = host.succeed(
        "journalctl -u briard-agent -o short-precise | grep -aE 'guest-stop|guest-shutdown|rescue:' || true"
    )
    print(stopleg)

    # KEEP THE DURATION -- it is the field that says WHICH bug this is. systemd's progress line
    # carries its own elapsed time and the job's timeout, "... (10s / 1min 30s)", and this capture
    # used to cut the match at the opening paren. That threw away exactly what separates a unit
    # stalling a few seconds under a loaded runner from one sitting there until it was SIGKILLed.
    # Both trip the assertion below and they are not the same finding: the first is a race whose
    # window load widened, the second is [B.28]'s deadlock back on the shutdown path. Every run had
    # already recorded which of the two it was, on the console, and the regex discarded it.
    #
    # Matched up to the CLOSING paren rather than to end-of-line, because \r is stripped above and
    # systemd REPRINTS this line as the job runs -- so several prints land on one physical line and
    # a greedy `.*` would swallow them into one unreadable match. Per-print matches keep the
    # successive elapsed times, and those are the evidence of how far the job actually got.
    stuck = host.succeed(
        "tr -d '\\r' < /tmp/guest-console.log | "
        "grep -aoE 'A stop job is running for [^)]*\\)' | sort -u || true"
    )
    # THE DUMP COVERS BOTH ASSERTIONS, and is taken BEFORE either fires. They are two halves of one
    # question -- did the clean route work, and if not, what inside the guest stopped it -- so
    # whichever trips, the console is the evidence for it. Hanging the dump off the `stuck` half
    # alone left the ACPI-fallback failure reporting a host-side log line and nothing at all from
    # inside the guest, which is the side the answer is on; [B.85] was invisible in precisely that
    # way until GUEST_SERIAL existed. Measured, not reasoned: the fallback half failed on a run
    # while this was still one-sided, and the log could say nothing about why.
    #
    # \r -> \n rather than deleted: this is a progress console, and collapsing the carriage returns
    # is what makes the surrounding lines readable instead of one long smear.
    #
    # Sliced FROM the mark taken before the rescue, not tailed: the console keeps growing through
    # the rebuilt guest's boot, so a tail would print that boot and hide the shutdown entirely.
    # Bounded, because a guest that fails to shut down can emit a lot before anyone gives up.
    if "trying the power button" in stopleg or stuck.strip():
        print(f"=== guest console from the rescue (line {console_mark} on) ===")
        print(host.succeed(
            f"tr '\\r' '\\n' < /tmp/guest-console.log | sed -n '{console_mark},{console_mark + 250}p'"
        ))

    assert "trying the power button" not in stopleg, (
        "the guest agent's os.poweroff did not stop the machine and stopCleanly fell back to ACPI "
        f"-- [B.85] is back, and the clean route is not the route being taken:\n{stopleg}"
    )
    assert not stuck.strip(), (
        f"the guest's shutdown had to wait on a unit, which is what [B.85] was:\n{stuck}"
    )
    print("clean stop: the agent route took it, and no unit held the guest's shutdown")

    # (1) THE VM REALLY WENT DOWN AND CAME BACK. A different QEMU is serving, so the sequence ran
    # rather than short-circuiting -- the honest in-VM half of "it was rebuilt". The other half,
    # that the overlay's CONTENTS were discarded, is proven in platform/overlay_test.go where it
    # can be checked properly (see the note above on why size and inode both lie here).
    qemu_after = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    assert qemu_after != qemu_before, \
        f"same QEMU pid {qemu_after} -- the guest was never taken down, so nothing was rebuilt"
    print(f"guest replaced: pid {qemu_before} -> {qemu_after}")

    # (2) THE DATA DISK WAS NOT. Same file, same size, metadata still there. This is the claim the
    # whole verb rests on, and the catastrophic failure -- a rescue that recreated or wiped the
    # replicated volume -- cannot pass it.
    #
    # WHAT THIS DOES NOT PROVE, stated so nobody reads more into it: that bring-up ADOPTED the
    # replica rather than re-seeding it in place. A re-seed writes fresh metadata to the same file,
    # so it would keep the inode and the size. Byte-comparing the tail cannot separate the two
    # either, because DRBD legitimately rewrites its metadata on attach. Proving adoption needs
    # real service data in the volume (the catalogued fixture the other data tests install) -- worth
    # doing, and deliberately not smuggled in here as an assertion that looks
    # stronger than it is.
    assert data_inode == host.succeed("stat -c %i /tmp/data.img").strip(), \
        "the data disk is a different file -- the rescue recreated the replicated volume"
    assert data_size == host.succeed("stat -c %s /tmp/data.img").strip(), \
        "the data disk changed size -- the rescue resized the replicated volume"
    assert host.succeed("tail -c 4096 /tmp/data.img | sha256sum").strip().split()[0] != zeros, \
        "the data disk's metadata was wiped by the rescue"
    print("data disk untouched: same file, same size, metadata intact")

    # (3) And it is a node again: the rebuilt guest came up on the existing replica and the front
    # door answers.
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)

    # (4) Still an overlay on the same image, so the node can be rescued again -- a rebuild that
    # produced a standalone disk would work once and then refuse forever.
    again = host.succeed("qemu-img info --output=json --force-share /tmp/guest.qcow2")
    assert "nixos.qcow2" in again, f"the rebuilt disk is not an overlay on the image:\n{again}"

    print("the guest was rebuilt from its backing image, kept its data disk, and re-converged")

    # (5) THE STATE DISK SURVIVED ([B.86g]), and the guest is the same machine. The disk carried a
    # filesystem before the rescue (the guest formatted it on its first boot), and its ext4 UUID,
    # read from the host side, is the same after: the rescue's fresh OS found the disk and kept
    # it. [[verification-assertions-must-fail]]: a reformat changes the UUID. (The mkfs itself
    # never reaches the console -- systemd-makefs is silent there -- so a count of it is no proof.)
    assert state_uuid == host.succeed("dd if=/tmp/state.img bs=1 skip=1128 count=16 2>/dev/null | od -An -tx1 | tr -d ' \\n'").strip(), \
        "the state disk's filesystem UUID changed across the rescue -- it was reformatted"
    NODE, STATE_IMG = "guest", "/tmp/state.img"

    print("the state disk survived the rescue untouched, and the guest kept its machine identity")

    # === (6) THE OS MOVES BY IMAGE ([B.86h]). The guest chain's release is a whole image; the
    #        agent fetches and verifies it, stages it beside the one in use, stops the guest,
    #        swaps the file, rebuilds the overlay, boots, proves the booted closure is the one the
    #        signed manifest names, health-gates, and drops the old image. Then the failable
    #        control: a release whose manifest names a closure its image does NOT boot is put
    #        back -- same file swapped the other way -- and the node is serving what it served.
    V = "${agent.version}"
    GV = "guest." + V.split(".", 1)[1]
    GV2 = "guest.20991230.next0000"
    host.succeed("mkdir -p /srv/guest && cp -r ${channel}/guest/. /srv/guest/ && chmod -R u+w /srv/guest")
    def sign_and_point(ver, pointers):
        d = f"/srv/guest/{ver}"
        host.succeed(f"${stub}/bin/briard-selfupdate-stub sign /root/release.key {d}/manifest.json | base64 -d > {d}/manifest.json.sig")
        for p in pointers:
            host.succeed(f"mkdir -p /srv/guest/{p} && cp {d}/manifest.json {d}/manifest.json.sig /srv/guest/{p}/")
    sign_and_point(GV, ("stable",))
    sign_and_point(GV2, ("latest",))
    host.succeed("systemd-run --unit=guest-channel --collect ${stub}/bin/briard-selfupdate-stub serve 127.0.0.1:8099 /srv")
    host.wait_until_succeeds("curl -sf http://127.0.0.1:8099/guest/latest/manifest.json -o /dev/null", timeout=30)
    qemu_before = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    state_uuid = host.succeed("dd if=/tmp/state.img bs=1 skip=1128 count=16 2>/dev/null | od -An -tx1 | tr -d ' \\n'").strip()

    out = host.succeed("${agent}/bin/briard-agent update guest -sock /run/briard/admin.sock -to latest").strip()
    assert f"now running {GV2}" in out, f"briard update guest said: {out!r}"
    host.succeed(f"journalctl -u briard-agent | grep -q 'image-upgrade: booted {GV2}, health-gating'")
    host.succeed(f"journalctl -u briard-agent | grep -q 'image-upgrade: {GV2} committed'")
    # The guest runs the NEXT image's closure (the manifest named it; the boot proved it), on a
    # fresh overlay over the swapped file, the previous image dropped after the gate.
    host.succeed(f"grep -q '\"version\":\"{GV2}\"' /tmp/guest-release.json")
    host.succeed("grep -q '\"system\":\"${nextSystem}\"' /tmp/guest-release.json")
    host.fail("test -e /tmp/nixos.qcow2.prev"); host.fail("test -e /tmp/nixos.qcow2.next")
    backing = host.succeed("qemu-img info --output=json --force-share /tmp/guest.qcow2")
    assert "/tmp/nixos.qcow2" in backing, f"the overlay is not on the swapped image:\n{backing}"
    assert host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0] != qemu_before, "the guest was never restarted"
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    assert state_uuid == host.succeed("dd if=/tmp/state.img bs=1 skip=1128 count=16 2>/dev/null | od -An -tx1 | tr -d ' \\n'").strip(), \
        "the state disk was reformatted across the image upgrade"
    print(f"the OS moved from {GV} to {GV2} by swapping the image; state disk kept; guest serving")

    # THE FAILABLE CONTROL: a release whose signed manifest names a closure its image does not
    # boot. Same image bytes as GV2, manifest lying about the system -> the boot does not prove
    # the target -> the swap is undone and the node is back on GV2, serving.
    GV3 = "guest.20991231.liar0000"
    host.succeed(f"mkdir -p /srv/guest/{GV3} && cp /srv/guest/{GV2}/nixos.qcow2.zst /srv/guest/{GV3}/")
    host.succeed(f"${agent}/bin/briard-agent --stage-manifest /srv/guest/{GV3} --chain guest --release {GV3} --system /nix/store/00000000000000000000000000000000-nixos-system-liar --min-host {V}")
    sign_and_point(GV3, ("latest",))
    host.fail("${agent}/bin/briard-agent update guest -sock /run/briard/admin.sock -to latest")
    host.succeed(f"journalctl -u briard-agent | grep -q \"not {GV3}'s system\"")
    host.succeed("journalctl -u briard-agent | grep -q 'OS upgrade rolled back to'")
    host.succeed(f"grep -q '\"version\":\"{GV2}\"' /tmp/guest-release.json")  # the record never moved
    host.fail("test -e /tmp/nixos.qcow2.prev"); host.fail("test -e /tmp/nixos.qcow2.next")
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    host.succeed("journalctl -u briard-agent | grep -q 'guest OS upgrade failed'")  # the owner heard
    print(f"a release that boots something its manifest does not name was put back; the node serves {GV2}")
  '';
}
