# Which generation a launch boots is decided OUTSIDE the guest's disk.
#
# The reboot half of an OS upgrade needs a way to say "come up on the new generation THIS
# time, and if it goes badly, come up on the old one next time" — without writing that
# choice anywhere the rollback would restore. This test proves the mechanism that does it,
# and nothing else: no staging over a cache, no health gate, no snapshot, no maintenance
# bracket, no upgrade orchestration of any kind. Those come after, and they come after
# BECAUSE this is the piece that can brick a box if it is wrong.
#
# The mechanism, end to end: `os.stageboot` registers a closure as a generation of a
# `staging` system profile and reinstalls the bootloader FROM THE RUNNING SYSTEM, so grub
# gains a staging submenu while its default entry does not move. The host then passes
# `-smbios type=11,value=briard_boot=staging` on the one launch that should come up on it,
# and the guest's grub reads that OEM string back and overrides `default` for that boot only.
#
# WHAT MAKES IT A REAL PROOF — three launches of ONE disk, and it is the comparison across
# them that carries it:
#   1. stage:   boots the shipped default, arms `staging`, and must still report the
#               unchanged system (so staging did not quietly become the default).
#   2. select:  same disk, same state, selector passed -> must report the STAGING closure.
#               If the selector did nothing, this launch reports the default and fails.
#   3. release: same disk, same state, selector withheld -> must report the ORIGINAL
#               closure again. This is the rollback, and the reason the arming lives off
#               the disk: nothing persisted, so not passing the flag is all it takes.
# Launch 3 is what a `grub-reboot`-style arming could not satisfy without also having
# written into the guest's own grubenv — i.e. into any snapshot taken around the upgrade.
#
# The guest here is the SHIPPED disk plus a second baked generation, and nothing else: the delta
# between them is a single /etc file, so both entries boot the identical kernel and the ONLY
# thing that can distinguish them is which menu entry grub chose. No binary cache is involved —
# delivery is os-stage's proof, not this one.
#
# It needs no payload: `BOOT_SELECT=1` returns before bring-up ever runs and the guest is given
# no NICs at all, so a payload would never be observed.
#
# Heavy (nested VM), so it rides the `integration` tag; `nix flake check` skips it. Run:
#   nix build .#tests.boot-select -L
{
  pkgs,
  guestDisk,
  stagingSystem,
  driver,
}:
pkgs.testers.runNixOSTest {
  name = "boot-select";
  skipTypeCheck = true;

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 4096;
      virtualisation.cores = 4;
      virtualisation.diskSize = 12288;
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ]; # expose vmx -> nested KVM in L1
      environment.systemPackages = [
        pkgs.qemu
        driver
      ];
      # The nested guest gets NO NICs at all: this proof runs entirely over the virtio-serial
      # control channel, and a boot mechanism has nothing to do with the network. One less
      # substrate between the assertion and what it is asserting.
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # ONE overlay for all three launches. The staging registration and the rewritten
    # grub.cfg have to survive across boots -- a fresh disk each time would test nothing.
    host.succeed("qemu-img create -f qcow2 -b ${guestDisk}/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")

    def boot(unit, extra=""):
        """Run the guest once and return the closure it came up on."""
        host.succeed(
            f"systemd-run --unit={unit} --collect "
            "--setenv=QEMU=${pkgs.qemu}/bin/qemu-system-x86_64 --setenv=ACCEL=kvm:tcg "
            "--setenv=GUEST_DISK=/tmp/guest.qcow2 --setenv=BOOT_SELECT=1 "
            f"--setenv=CONTROL_SOCK=/run/{unit}.sock --setenv=QMP_SOCK=/run/briard-qmp/{unit}.sock "
            f"--setenv=GUEST_SERIAL=/tmp/{unit}-console.log "
            f"{extra} ${driver}/bin/driver"
        )
        try:
            host.wait_until_succeeds(f"journalctl -u {unit} | grep -q 'BOOTED system='", timeout=300)
            # Each launch ends ITSELF -- it asks the guest OS to power off and waits for the
            # VM to go -- rather than being stopped from out here. That ordering is what keeps
            # a power cut away from the launch that has just rewritten the guest's bootloader,
            # which is where killing QEMU hung the NEXT boot about half the time.
            host.wait_until_fails(f"systemctl is-active --quiet {unit}", timeout=180)
            # And the stop must have been CLEAN, not the driver's power-cut fallback. This
            # assertion is the point of the whole ordering above: without it the test passes
            # just as happily on a killed QEMU, which is the state that made it flaky. The
            # driver prints this marker only after the guest agent acknowledged `os.poweroff`
            # AND the QEMU monitor socket disappeared, i.e. the VM is actually gone -- so it
            # is the guest's exit being asserted, not our intent to ask for it.
            #
            # It was long believed unassertable here: a nested L2 guest was thought to complete
            # no shutdown by any trigger. That was wrong (and the second symptom, an empty
            # console, was `-serial file:` truncating on open). Measured before this landed:
            # 4 consecutive runs, 3 clean shutdowns each, 12/12, no fallbacks.
            host.succeed(f"journalctl -u {unit} | grep -q CLEAN_SHUTDOWN")
        finally:
            # Always dump the guest console -- grub speaks on ttyS0 too, so a launch that
            # never reaches the agent still leaves its evidence here.
            print(f"--- {unit} guest console ---")
            print(host.succeed(f"cat /tmp/{unit}-console.log || true"))
        line = host.succeed(f"journalctl -u {unit} | grep 'BOOTED system=' | tail -1")
        return line.split("BOOTED system=")[1].strip()

    # 1. Arm staging. The marker only appears if os.stageboot succeeded AND the running
    #    system was unchanged afterwards (the driver re-reads it).
    shipped = boot("stage", "--setenv=STAGE_BOOT=${stagingSystem}")
    assert shipped != "${stagingSystem}", f"the disk already boots the staging target ({shipped}) -- nothing to select"
    host.wait_until_succeeds("journalctl -u stage | grep -q 'STAGED_BOOT system=${stagingSystem}'", timeout=300)
    # The containment, asserted where it is actually created: anyone who can reach a QMP
    # socket owns that VM outright, and QEMU makes the socket itself world-readable.
    host.succeed("test $(stat -c %a /run/briard-qmp) = 700")

    # 2. Same disk, selector passed. This is the whole claim.
    selected = boot("select", "--setenv=BOOT_STAGING=1")
    assert selected == "${stagingSystem}", (
        f"selector ignored: booted {selected}, want the staging generation ${stagingSystem}"
    )

    # 3. Same disk, selector withheld -> back to the shipped default. Nothing was armed on
    #    disk, so the rollback is just this: launch without the flag.
    released = boot("release")
    assert released == shipped, f"selector persisted: booted {released}, want the original {shipped}"

    print("V3.17c2: the host picked the generation, per launch, from outside the disk —")
    print(f"  default={shipped}\n  staging={selected}\n  released={released}")
  '';
}
