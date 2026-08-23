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
# DRBD legitimately rewrites its metadata on attach. Proving adoption needs real payload data in
# the volume (the dummy-payload fixture the data tests use); that is worth building and is
# deliberately not smuggled in here as an assertion that would look stronger than it is.
#
# Heavy (a nested VM + the shipped guest disk) -> the `integration` tag. Run on the L0:
#   gh workflow run vm-test.yml -f test=guest-rescue
{ pkgs, guestDisk, agent, netWrap }:
pkgs.testers.runNixOSTest {
  name = "guest-rescue";
  skipTypeCheck = true; # systemd-run + dynamic asserts

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 4096;
      virtualisation.cores = 4;
      virtualisation.diskSize = 10240;
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ]; # nested KVM
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl ];
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # Same L2 as agent-recover, and it is the SHIPPED NIC contract rather than a rig convenience:
    # veth parent, the guest's two LAN NICs as macvtap children in install.sh's order (sys0 -> eth1,
    # svc0 -> eth2), and the private host<->guest link as a plain tap at 10.9.9.1/24. The macvlan
    # shim this used to build was the rig granting itself reachability the product lacked; the VIP
    # curls below now pass because the agent routes it over the private link ([V3b.19a]).
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.9.9.1/24 dev briard-priv0 && ip link set briard-priv0 up"
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
    host.succeed("qemu-img create -f qcow2 -b ${guestDisk}/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")
    backing = host.succeed("qemu-img info --output=json --force-share /tmp/guest.qcow2")
    assert "nixos.qcow2" in backing, f"the guest disk is not an overlay on the image; rescue would refuse:\n{backing}"

    host.succeed(
        "systemd-run --unit=briard-agent --collect "
        # The PATH install.sh gives the shipped unit (scripts/install.sh, "Environment=PATH="). The
        # agent shells out to systemd-run, systemctl and -- since [V3b.19] -- `ip`, all BY NAME, and
        # a transient unit's default PATH resolves none of them reliably. Pinning the shipped value
        # is the point: the rig gets what the product gets ([V3b.19a]).
        "--setenv=PATH=/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin "
        "--setenv=QEMU=${pkgs.qemu}/bin/qemu-system-x86_64 --setenv=ACCEL=kvm:tcg "
        "--setenv=GUEST_DISK=/tmp/guest.qcow2 --setenv=DATA_DISK=/tmp/data.img "
        "--setenv=CONTROL_SOCK=/run/briard-ctl.sock --setenv=ADMIN_SOCK=/run/briard/admin.sock "
        "--setenv=NODE=guest --setenv=SYSTEM_TAP=sys0 --setenv=SERVICE_TAP=svc0 --setenv=WITNESS_TAP=briard-priv0 --setenv=STATUS_EVERY=2s "
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
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
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
    # deadlock of nixosTest/reactor-pause-deadlock.nix, on the shutdown path, where nothing was
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
    assert "trying the power button" not in stopleg, (
        "the guest agent's os.poweroff did not stop the machine and stopCleanly fell back to ACPI "
        f"-- [B.85] is back, and the clean route is not the route being taken:\n{stopleg}"
    )

    stuck = host.succeed(
        "tr -d '\\r' < /tmp/guest-console.log | "
        "grep -aoE 'A stop job is running for [^(]*' | sort -u || true"
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
    # real payload data in the volume (nixosTest/dummy-payload.nix, the fixture the other data
    # tests use) -- worth doing, and deliberately not smuggled in here as an assertion that looks
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
  '';
}
