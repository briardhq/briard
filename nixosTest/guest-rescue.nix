# The rescue ladder's last rung: restart the guest on its image, and keep the data.
#
# `briard rescue` stops the guest and boots it again on the signed image it was installed from,
# attached read-only. Its claim, and the only reason the verb is safe to offer: the code half
# cannot carry anything forward while the data half is not touched (the image is never written,
# the guest's scratch is emptied at every boot, the replicated volume is a separate disk), so
# what comes back is the same node with a factory guest, not a new node.
#
# Only a real node can show the integration, a fresh guest coming up against a populated data
# disk. This asserts:
#
#   1. a different QEMU is serving afterwards: the VM went down and came back
#   2. the data disk is the same file, same size, same LUKS2 volume UUID
#   3. the restarted guest converges and serves again
#   4. the image is attached read-only, and its bytes are what the install laid down -- nothing
#      any boot did reached it
#   5. the state disk keeps its filesystem (same ext4 UUID)
#   6. the OS moves by image, and a release whose image boots another closure is put back
#
# and, before the rescue, that without -yes the verb refuses and the guest is left running.
#
# (1) stops this passing vacuously: a rescue that quietly did nothing satisfies (2), (3) and (4)
# perfectly, which is exactly what a no-op looks like from outside.
#
# Not proven: that bring-up adopted the existing replica rather than re-seeding it in place. A
# re-seed rewrites metadata in the same file, keeping inode and size, and byte-comparing cannot
# separate the two because DRBD legitimately rewrites its metadata on attach. Proving adoption
# needs real app data in the volume (the catalogued fixture the data tests install): worth
# building, and deliberately not smuggled in here as an assertion that looks stronger than it is.
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
      virtualisation.diskSize = 16384; # the image copy, a staged image and a set-aside one coexist during an upgrade
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ]; # nested KVM
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl pkgs.e2fsprogs ]; # debugfs reads the state disk
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # Same L2 as agent-recover, the shipped NIC contract rather than a rig convenience: veth
    # parent, the guest's two LAN NICs as macvtap children in the agent's order (sys0 -> eth1,
    # svc0 -> eth2), and the private host<->guest link as a plain tap at 10.11.9.1/24. No macvlan
    # shim: the VIP curls pass because the agent routes the VIP over the private link.
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )

    # A copy of the image in a writable directory, because an OS upgrade swaps the file by rename
    # and the store is read-only; briard-agent install lays the image down as a copy too. The file
    # itself is never written: its hash, taken here, is assertion (4).
    host.succeed("cp ${guestDisk}/nixos.qcow2 /tmp/nixos.qcow2 && chmod 0644 /tmp/nixos.qcow2")
    image_sum = host.succeed("sha256sum /tmp/nixos.qcow2").split()[0]
    host.succeed("truncate -s 512M /tmp/data.img")
    # The state disk: empty, sparse; the guest formats it on its first boot and the rescue must
    # not format it again, the whole claim of the disk.
    host.succeed("truncate -s 2G /tmp/state.img")
    # The release keyring the agent verifies guest releases against is read at agent start, so it
    # is minted before the launch and used by the channel section below.
    host.succeed("${stub}/bin/briard-selfupdate-stub keygen /root/release.key /root/keyring.pem")

    # The host holds a guest bundle tree, as briard-agent install lays on every install: the image
    # bakes no reverse proxy, so a guest is dressed by its host or it cannot serve. Copied out of
    # the store because the host writes `guest.good` beside the tree.
    host.succeed("mkdir -p /opt/briard/agent && cp -r ${dressBase}/. /opt/briard/agent/ && chmod -R u+w /opt/briard/agent")

    host.succeed(
        "systemd-run --unit=briard-agent --collect "
        # The PATH the shipped unit carries (scripts/units/briard-agent.service,
        # "Environment=PATH="), pinned so the rig gets what the product gets. The agent shells out
        # by name to systemd-run, systemctl and (since it owns the host route) `ip`, and a
        # transient unit default PATH resolves none of them reliably.
        "--setenv=PATH=/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin "
        "--setenv=QEMU=${pkgs.qemu}/bin/qemu-system-x86_64 --setenv=ACCEL=kvm:tcg "
        # Where the host keeps its guest bundle tree: the agent default (/opt/briard/agent).
        "--setenv=UPDATE_BASE=/opt/briard/agent "
        "--setenv=GUEST_IMAGE=/tmp/nixos.qcow2 --setenv=DATA_DISK=/tmp/data.img --setenv=STATE_DISK=/tmp/state.img "
        # The vm chain: the channel this rig serves, the keyring it mints, the record.
        "--setenv=CHANNEL_URL=http://127.0.0.1:8099 --setenv=UPDATE_KEYRING=/root/keyring.pem --setenv=GUEST_RELEASE_CACHE=/tmp/guest-release.json "
        "--setenv=CONTROL_SOCK=/run/briard-ctl.sock --setenv=ADMIN_SOCK=/run/briard/admin.sock "
        "--setenv=NODE=guest --setenv=SYSTEM_TAP=sys0 --setenv=SYSTEM_DEV=eth1 --setenv=SYSTEM_CIDR=10.0.0.1/24 --setenv=SYSTEM_HOST_CIDR=10.0.0.129/32 --setenv=WITNESS_CIDR=10.11.9.2/24 --setenv=SERVICE_TAP=svc0 --setenv=WITNESS_TAP=briard-priv0 --setenv=STATUS_EVERY=2s "
        "--setenv=VIP_DEV=eth2 --setenv=VIP_ADDR=192.168.1.100/24 "
        "--setenv=NET_MODE=macvtap --setenv=NET_WRAP_BIN=${netWrap}/bin/briard-net-wrap "
        # GUEST_SERIAL is the only window into the guest during a stop: the host watches the VM
        # systemd unit with no console inside it, so 90 seconds of a guest ignoring `os.poweroff`
        # and 90 of one shutting down slowly look identical from out here. The chardev appends
        # across launches (platform serialArgs), so one file holds the stopped and rebuilt guests.
        "--setenv=GUEST_SERIAL=/tmp/guest-console.log "
        "${agent}/bin/briard-agent run"
    )
    try:
        host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    except Exception:
        # A guest that never converged is diagnosable only from inside it: dump its console
        # (the console is the window) and the guest unit's own stderr before failing.
        print("=== guest console (tail) ===")
        print(host.succeed("tr -d '\\r' < /tmp/guest-console.log | tail -200 || true"))
        print(host.succeed("journalctl -u briard-guest.service --no-pager | tail -40 || true"))
        print(host.succeed("pgrep -af qemu-system-x86_64 || true; ls -la /run/briard/ /run/briard/qmp/ || true"))
        raise
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)

    # === What to look at before the rescue. ===
    # The QEMU PID stands in for "the VM really went down and came back", the part of the sequence
    # this harness can see honestly.
    qemu_before = host.succeed("pgrep -f 'qemu-system-x86_64.*nixos.qcow2'").strip().splitlines()[0]

    # The data disk carries the opposite assertion: it must be the same file afterwards, same size,
    # still holding DRBD's metadata.
    data_inode = host.succeed("stat -c %i /tmp/data.img").strip()
    data_size = host.succeed("stat -c %s /tmp/data.img").strip()

    def data_uuid(m):
        """The data volume's identity, read off the backing file: the LUKS2 header's own UUID.

        THE HANDLE USED TO BE THE TAIL of the file, where `meta-disk internal` puts DRBD's
        metadata -- non-zero there meant "something seeded this disk". The storage seam went under
        DRBD (`data.img -> LUKS -> PV -> VG -> LV -> DRBD`) and the tail stopped meaning that:
        LVM rounds the LV to whole extents, so the last megabytes of the PV are unallocated
        remainder that nothing ever writes, and the tail reads as zeroes on a perfectly healthy
        node. The header at offset 0 is the opposite -- written by this node the first time it
        booted, and the ~16 MB whose loss is the loss of the volume.

        It is also a STRONGER claim than the tail was: a UUID that survives says this is the same
        volume, where non-zero bytes said only that some bytes were there."""
        magic = m.succeed(
            "dd if=/tmp/data.img bs=1 count=6 status=none | od -An -tx1 | tr -d ' \\n'"
        ).strip()
        assert magic == "4c554b53babe", \
            f"the data volume is not LUKS2 (read {magic!r}) -- the survival assertions below would be vacuous"
        return m.succeed("dd if=/tmp/data.img bs=1 skip=168 count=40 status=none | tr -d '\\0'").strip()

    data_id = data_uuid(host)
    print(f"before rescue: qemu {qemu_before}, data inode {data_inode}, data volume {data_id}")

    # === The rescue ===
    # `briard-agent <verb>` is the CLI (a bare first argument is a subcommand, main.go); the
    # `briard` name is a symlink briard-agent install makes, which this harness does not have.
    # Without -yes it must refuse and touch nothing: the guard belongs to the verb, not the
    # operator memory, and asserting it means a change that drops it fails a test, not a node.
    host.fail("${agent}/bin/briard-agent rescue -sock /run/briard/admin.sock")
    assert qemu_before == host.succeed("pgrep -f 'qemu-system-x86_64.*nixos.qcow2'").strip().splitlines()[0], \
        "an unconfirmed rescue took the guest down anyway"

    # Where the shutdown starts in the console, marked before it happens. The chardev appends
    # across launches (GUEST_SERIAL above), so by the assertions the file ends with the rebuilt
    # guest boot and a `tail` shows nsncd starting, not the stop that failed; slicing from this
    # mark makes the dump show the shutdown. Counted through the same `tr` the dump uses: collapsing
    # carriage returns changes the line count, so any other mark indexes a different file.
    console_mark = int(host.succeed("tr '\\r' '\\n' < /tmp/guest-console.log | wc -l").strip())
    # The state disk ext4 UUID (superblock at 1024, s_uuid at +0x68), before the rescue, read from
    # the host side with no guest cooperation.
    state_uuid = host.succeed("dd if=/tmp/state.img bs=1 skip=1128 count=16 2>/dev/null | od -An -tx1 | tr -d ' \\n'").strip()
    assert state_uuid and state_uuid != "0" * 32, "the state disk carries no filesystem before the rescue -- the guest never formatted it"

    # The rescue does not hold the loop, and the ladder does not fight it. A rescue runs off the
    # observe loop (agent/host/acts.go): the loop ticks on after the submit (STATUS_EVERY=2s here)
    # until the rescue stops the guest and the channel dies. Then Run, which owns a dying channel,
    # sees a relauncher in flight and waits for it to re-establish the channel instead of climbing
    # the recovery ladder (which would reboot the VM the rescue is halfway through replacing); the
    # loop resumes on the new channel when the rescue ends. While the guest is down the loop is
    # paused (nothing to read; the host-side alerters pause with it, a known, accepted gap).
    # Asserted strictly between the journal marks, each claim on its own line.
    #
    # The wait line of Run is the proof the loop was not held: Run sees the channel die only when
    # the loop read fails, so a loop held by the rescue would reach Run after the rescue ended,
    # with nothing in flight to wait for. A status line between submit and channel death proves
    # nothing either way: a request wakes the loop without running a cycle, and the next tick can
    # land after the guest stopped answering, its reads held until the channel resets.
    cursor = host.succeed("journalctl -u briard-agent -n1 --show-cursor | sed -n 's/^-- cursor: //p'").strip()
    host.succeed("${agent}/bin/briard-agent rescue -yes -sock /run/briard/admin.sock")
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'rescue: the guest was rebuilt and has re-converged'",
        timeout=900,
    )
    lines = host.succeed(f"journalctl -u briard-agent -o cat --after-cursor='{cursor}'").splitlines()
    first = next(i for i, l in enumerate(lines) if "directive kind=rescue submitted locally" in l)
    last = next(i for i, l in enumerate(lines) if "rescue: the guest was rebuilt and has re-converged" in l)
    down = next(i for i, l in enumerate(lines) if i > first and "control channel down" in l)
    assert not any("guest-recovery:" in l for l in lines[first:last]), "the recovery ladder ran during the rescue:\n" + "\n".join(l for l in lines[first:last] if "guest-recovery:" in l)
    assert any("control channel replaced by a rescue in flight; waiting for it" in l for l in lines[down:last]), "Run never waited for the rescue while it was in flight -- the rescue held the loop, or the wait path was not exercised:\n" + "\n".join(lines[first:last + 1])
    seen = sum(1 for l in lines if "status node=" in l)
    host.wait_until_succeeds(f"test $(journalctl -u briard-agent -o cat --after-cursor='{cursor}' | grep -c 'status node=') -gt {seen}", timeout=30)
    print("rescue: Run waited for the rescue in flight, no ladder, ticking again after")

    # === The clean stop must actually be the clean route ===
    # The stop goes through host.stopCleanly, which asks the guest agent first (`os.poweroff` ->
    # `systemctl poweroff --no-block`) and keeps the ACPI power button as the fallback for a guest
    # whose agent is gone. Why a healthy node takes the fallback is invisible from out here (no
    # console inside the guest unit: "the request was ignored" and "the shutdown is stuck" look
    # identical), which is why GUEST_SERIAL is set. The failure guarded against: the shutdown
    # starts a second after the request, then drbd-reactor deadlocks on its own stop for a full
    # 90s TimeoutStopSec and is SIGKILLed, the promote-vs-stop deadlock on the shutdown path
    # (defused on drbd-reactor.service ExecStop, guest-image/configuration.nix).
    #
    # Two assertions, because either alone passes for the wrong reason. The fallback line proves
    # the agent route worked (a guest that still deadlocks reaches the power button; its absence
    # is the whole claim). The console proves why, and guards a future stop hanging on a different
    # unit: a deadlock that moved would still be silent up here. (\r stripped: a serial console.)
    stopleg = host.succeed(
        "journalctl -u briard-agent -o short-precise | grep -aE 'guest-stop|guest-shutdown|rescue:' || true"
    )
    print(stopleg)

    # Keep the duration: it says which bug this is. systemd progress lines carry elapsed time and
    # the job timeout, "... (10s / 1min 30s)"; a match cut at the opening paren throws away what
    # separates a unit stalling a few seconds under a loaded runner (a race whose window load
    # widened) from one sitting there until SIGKILLed (the promote-vs-stop deadlock back on the
    # shutdown path). Both trip the assertion below; they are not the same finding.
    #
    # Matched to the closing paren, not end-of-line: \r is stripped and systemd reprints the line
    # as the job runs, so several prints share one physical line and a greedy `.*` would swallow
    # them into one unreadable match. Per-print matches keep the successive elapsed times, the
    # evidence of how far the job got.
    stuck = host.succeed(
        "tr -d '\\r' < /tmp/guest-console.log | "
        "grep -aoE 'A stop job is running for [^)]*\\)' | sort -u || true"
    )
    # The dump covers both assertions and is taken before either fires: they are two halves of one
    # question (did the clean route work, and if not, what inside the guest stopped it), so
    # whichever trips, the console is its evidence. Hung off `stuck` alone, the ACPI-fallback
    # failure would report a host-side log line and nothing from inside the guest, the side the
    # answer is on. \r -> \n rather than deleted, so the progress console reads as lines instead of
    # one long smear. Sliced from the pre-rescue mark, not tailed: a tail would print the rebuilt
    # guest boot and hide the shutdown entirely. Bounded, because a guest that fails to shut down
    # can emit a lot before anyone gives up.
    if "trying the power button" in stopleg or stuck.strip():
        print(f"=== guest console from the rescue (line {console_mark} on) ===")
        print(host.succeed(
            f"tr '\\r' '\\n' < /tmp/guest-console.log | sed -n '{console_mark},{console_mark + 250}p'"
        ))

    assert "trying the power button" not in stopleg, (
        "the guest agent's os.poweroff did not stop the machine and stopCleanly fell back to ACPI "
        f"-- the fallback stop is back, and the clean route is not the route being taken:\n{stopleg}"
    )
    assert not stuck.strip(), (
        f"the guest's shutdown had to wait on a unit, which is what the fallback stop was:\n{stuck}"
    )
    print("clean stop: the agent route took it, and no unit held the guest's shutdown")

    # (1) The VM really went down and came back: a different QEMU is serving, so the sequence ran
    # rather than short-circuiting.
    qemu_after = host.succeed("pgrep -f 'qemu-system-x86_64.*nixos.qcow2'").strip().splitlines()[0]
    assert qemu_after != qemu_before, \
        f"same QEMU pid {qemu_after} -- the guest was never taken down, so nothing was restarted"
    print(f"guest replaced: pid {qemu_before} -> {qemu_after}")

    # (2) The data disk did not: same file, same size, same LUKS2 volume. The claim the whole verb
    # rests on; the catastrophic failure (a rescue that recreated or wiped the replicated volume)
    # cannot pass it. Not proven (header): adoption rather than an in-place re-seed, which would
    # keep inode, size and the LUKS UUID too, because a re-seed happens inside the volume, above
    # the LUKS header.
    assert data_inode == host.succeed("stat -c %i /tmp/data.img").strip(), \
        "the data disk is a different file -- the rescue recreated the replicated volume"
    assert data_size == host.succeed("stat -c %s /tmp/data.img").strip(), \
        "the data disk changed size -- the rescue resized the replicated volume"
    assert data_uuid(host) == data_id, \
        "the data volume has a different LUKS UUID -- the rescue reformatted the replicated volume"
    print("data disk untouched: same file, same size, same volume")

    # (3) And it is a node again: the restarted guest came up on the existing replica and the
    # reverse proxy answers.
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)

    # (4) The image is read-only, and nothing reached it: two boots and a clean stop later its
    # bytes are the ones the install laid down, and the running QEMU holds it readonly=on. The
    # second can fail on its own (an attach without it), the first on anything that ever wrote it.
    host.succeed("pgrep -af qemu-system-x86_64 | grep -q 'file=/tmp/nixos.qcow2,if=none,readonly=on'")
    assert image_sum == host.succeed("sha256sum /tmp/nixos.qcow2").split()[0], \
        "the guest image changed under a running guest -- something wrote the OS disk"

    print("the guest restarted on its untouched image, kept its data disk, and re-converged")

    # (5) The state disk survived, and the guest is the same machine: its ext4 UUID, read from the
    # host side, is unchanged, so the fresh OS found the disk and kept it. This can fail: a
    # reformat changes the UUID. (The mkfs never reaches the console, systemd-makefs is silent
    # there, so a count of it is no proof.)
    assert state_uuid == host.succeed("dd if=/tmp/state.img bs=1 skip=1128 count=16 2>/dev/null | od -An -tx1 | tr -d ' \\n'").strip(), \
        "the state disk's filesystem UUID changed across the rescue -- it was reformatted"
    NODE, STATE_IMG = "guest", "/tmp/state.img"

    print("the state disk survived the rescue untouched, and the guest kept its machine identity")

    # === (6) The OS moves by image. The vm chain release is a whole image; the agent fetches and
    #        verifies it, stages it beside the one in use, stops the guest, swaps the file, boots
    #        it, proves the booted closure is the one the signed manifest names, health-gates, and
    #        drops the old image. Then the failable control: a release whose manifest names a
    #        closure its image does not boot is put back (the file swapped the other way), and
    #        the node serves what it served.
    V = "${agent.version}"
    GV = "vm." + V.split(".", 1)[1]
    GV2 = "vm.99123000.next0000"
    host.succeed("mkdir -p /srv/vm && cp -r ${channel}/vm/. /srv/vm/ && chmod -R u+w /srv/vm")
    def sign_and_point(ver, pointers):
        d = f"/srv/vm/{ver}/amd64"
        host.succeed(f"${stub}/bin/briard-selfupdate-stub sign /root/release.key {d}/manifest.json | base64 -d > {d}/manifest.json.sig")
        for p in pointers:
            host.succeed(f"mkdir -p /srv/vm/{p}/amd64 && cp {d}/manifest.json {d}/manifest.json.sig /srv/vm/{p}/amd64/")
    sign_and_point(GV, ("stable",))
    sign_and_point(GV2, ("latest",))
    host.succeed("systemd-run --unit=guest-channel --collect ${stub}/bin/briard-selfupdate-stub serve 127.0.0.1:8099 /srv")
    host.wait_until_succeeds("curl -sf http://127.0.0.1:8099/vm/latest/amd64/manifest.json -o /dev/null", timeout=30)
    qemu_before = host.succeed("pgrep -f 'qemu-system-x86_64.*nixos.qcow2'").strip().splitlines()[0]
    state_uuid = host.succeed("dd if=/tmp/state.img bs=1 skip=1128 count=16 2>/dev/null | od -An -tx1 | tr -d ' \\n'").strip()

    out = host.succeed("${agent}/bin/briard-agent update -vm -sock /run/briard/admin.sock -to latest").strip()
    assert f"now running {GV2}" in out, f"briard update -vm said: {out!r}"
    host.succeed(f"journalctl -u briard-agent | grep -q 'image-upgrade: booted {GV2}, health-gating'")
    host.succeed(f"journalctl -u briard-agent | grep -q 'image-upgrade: {GV2} committed'")
    # The guest runs the next image's closure (the manifest named it; the boot proved it), from the
    # swapped file, still read-only, the previous image dropped after the gate.
    host.succeed(f"grep -q '\"version\":\"{GV2}\"' /tmp/guest-release.json")
    host.succeed("grep -q '\"system\":\"${nextSystem}\"' /tmp/guest-release.json")
    host.fail("test -e /tmp/nixos.qcow2.prev"); host.fail("test -e /tmp/nixos.qcow2.next")
    host.succeed("pgrep -af qemu-system-x86_64 | grep -q 'file=/tmp/nixos.qcow2,if=none,readonly=on'")
    assert host.succeed("pgrep -f 'qemu-system-x86_64.*nixos.qcow2'").strip().splitlines()[0] != qemu_before, "the guest was never restarted"
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    assert state_uuid == host.succeed("dd if=/tmp/state.img bs=1 skip=1128 count=16 2>/dev/null | od -An -tx1 | tr -d ' \\n'").strip(), \
        "the state disk was reformatted across the image upgrade"
    print(f"the OS moved from {GV} to {GV2} by swapping the image; state disk kept; guest serving")

    # The failable control: same image bytes as GV2, a signed manifest lying about the system ->
    # the boot does not prove the target -> the swap is undone and the node is back on GV2, serving.
    GV3 = "vm.99123100.liar0000"
    host.succeed(f"mkdir -p /srv/vm/{GV3}/amd64 && cp /srv/vm/{GV2}/amd64/nixos.qcow2.zst /srv/vm/{GV3}/amd64/")
    host.succeed(f"${agent}/bin/briard-agent --stage-manifest /srv/vm/{GV3}/amd64 --chain vm --platform amd64 --release {GV3} --system /nix/store/00000000000000000000000000000000-nixos-system-liar --min-briard {V}")
    sign_and_point(GV3, ("latest",))
    host.fail("${agent}/bin/briard-agent update -vm -sock /run/briard/admin.sock -to latest")
    host.succeed(f"journalctl -u briard-agent | grep -q \"not {GV3}'s system\"")
    host.succeed("journalctl -u briard-agent | grep -q 'OS upgrade rolled back to'")
    host.succeed(f"grep -q '\"version\":\"{GV2}\"' /tmp/guest-release.json")  # the record never moved
    host.fail("test -e /tmp/nixos.qcow2.prev"); host.fail("test -e /tmp/nixos.qcow2.next")
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    host.succeed("journalctl -u briard-agent | grep -q 'guest OS upgrade failed'")  # the owner heard
    print(f"a release that boots something its manifest does not name was put back; the node serves {GV2}")
  '';
}
