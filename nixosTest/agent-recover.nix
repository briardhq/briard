# THE HOST RESTARTS A GUEST THAT STOPPED ANSWERING (B.22b, host-side rung).
#
# The guest-side half of this ladder is proven separately (agent-deadman: the guest reboots
# itself when the HOST goes silent). This is the mirror, and the harder direction: the GUEST
# goes silent and the host, which cannot see inside it, has to decide on its own that waiting
# is no longer recovery.
#
# WHAT MAKES IT A REAL PROOF, rather than a slower way of re-testing the reconnect that
# agent-bringup already covers: the QEMU PID. agent-bringup freezes the guest and thaws it, and
# what it proves is that the host RE-DIALS the same VM. Here the guest is frozen and never
# thawed, so a passing run cannot be a reconnect -- the process the host was talking to is gone
# and a different one is serving. The old PID being dead and the new one being different is the
# assertion; CONVERGED alone would not distinguish the two paths, and the VIP answering would
# not either.
#
# The second assertion is the PATIENCE, and it is the one that would otherwise rot silently.
# Restarting a wedged guest is only safe because the host waits out a window that no ordinary
# channel bounce can reach -- the in-guest agent exits per connection, an OS upgrade bounces the
# channel, a slow verb closes it, and every one of those heals in seconds. Shorten the window
# and this test still passes while the product starts power-cycling VMs over events that were
# fixing themselves. So the run measures how long the host held off, and fails if it acted too
# soon. That is why the test takes minutes rather than seconds: the wait IS the behaviour.
#
# Heavy (a nested VM + the shipped guest disk), so it rides the `integration` tag. Run:
#   nix build .#tests.agent-recover -L
{ pkgs, guestDisk, agent, netWrap }:
pkgs.testers.runNixOSTest {
  name = "agent-recover";
  skipTypeCheck = true; # systemd-run + dynamic asserts

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 4096; # room for L1 + the nested 2G guest
      virtualisation.cores = 4;
      virtualisation.diskSize = 10240;
      virtualisation.vlans = [ ]; # we build our own macvtap parent (192.168.1.0/24, the VIP subnet)
      virtualisation.qemu.options = [ "-cpu" "host" ]; # expose vmx -> nested KVM in L1
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl ];
    };

  testScript = ''
    import time

    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # The service L2 on the macvtap substrate (the default), same shape as agent-bringup: a
    # carrier-bearing veth parent, the guest's service NIC as a macvtap child, and a host-side
    # macvlan shim so L1 can curl the VIP at all (macvtap isolates host<->guest by design --
    # which is also precisely why the recovery ladder cannot probe the payload before deciding).
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name shim0 type macvlan mode bridge && ip addr add 192.168.1.1/24 dev shim0 && ip link set shim0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up"
    )
    host.succeed("qemu-img create -f qcow2 -b ${guestDisk}/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    host.succeed(
        "systemd-run --unit=briard-agent --collect "
        "--setenv=QEMU=${pkgs.qemu}/bin/qemu-system-x86_64 --setenv=ACCEL=kvm:tcg "
        "--setenv=GUEST_DISK=/tmp/guest.qcow2 --setenv=DATA_DISK=/tmp/data.img "
        "--setenv=CONTROL_SOCK=/run/briard-ctl.sock --setenv=NODE=guest "
        "--setenv=SERVICE_TAP=svc0 --setenv=STATUS_EVERY=2s "
        "--setenv=VIP_DEV=eth1 --setenv=VIP_ADDR=192.168.1.100/24 "
        "--setenv=NET_MODE=macvtap --setenv=NET_WRAP_BIN=${netWrap}/bin/briard-net-wrap "
        "${agent}/bin/briard-agent"
    )

    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)

    # --- the guest stops answering, and never starts again on its own ---
    # SIGSTOP the nested QEMU: the whole VM freezes, so the control channel dies AND every
    # re-dial the host makes finds a socket nothing is reading. This is the shape of the case
    # the ladder exists for -- indistinguishable, from outside, from a guest that has crashed.
    frozen = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    print(f"freezing the guest QEMU (pid {frozen}) and leaving it frozen")
    host.succeed(f"kill -STOP {frozen}")
    froze_at = time.monotonic()
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'control channel down'", timeout=60)

    # The host must NOT act on this yet. Nothing here distinguishes a wedged guest from a
    # channel that is about to come back, which is the whole reason for the wait.
    host.fail("journalctl -u briard-agent | grep -q 'restarting an unresponsive guest'")

    # It waits out the recovery window, then says what it is about to do -- on the local trail
    # `briard alerts` reads (notify.LogMarker), because on the free tier that trail is the only
    # delivery there is. The timeout is generous against the 3-minute window plus the ~10s each
    # re-dial spends on its handshake deadline.
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'alert \\[warning\\] Briard: restarting an unresponsive guest'",
        timeout=420,
    )
    waited = time.monotonic() - froze_at

    # THE PATIENCE ASSERTION. Anything under two minutes means the window was shortened or
    # bypassed, and the product is now restarting VMs over bounces that heal themselves.
    assert waited >= 120, f"host restarted the guest after only {waited:.0f}s -- the recovery window is not being honoured"
    print(f"host held off for {waited:.0f}s before acting")

    # A frozen QEMU answers neither its agent nor the ACPI button, so the clean stop must fail
    # and the forced stop must be what takes it down. Asserted because which route ran is the
    # single fact that decides whether DRBD recorded its quorum state on the way out.
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'guest-recovery: could not stop the guest cleanly, forcing'",
        timeout=180,
    )

    # THE PROOF. The frozen process is gone and a DIFFERENT QEMU is running -- so what recovered
    # the node was a restart of the VM, not a re-dial to the one that was already there. A
    # reconnect-only implementation fails right here.
    host.wait_until_fails(f"kill -0 {frozen}", timeout=180)
    host.wait_until_succeeds("pgrep -f guest.qcow2", timeout=300)
    restarted = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    assert restarted != frozen, f"same QEMU pid {restarted} after recovery -- the guest was never restarted"
    print(f"guest restarted: pid {frozen} -> {restarted}")

    # And it is a node again, not merely a running process: bring-up re-drove the whole
    # sequence on the persisted data disk (idempotent by B.22b's other half -- attach, never
    # re-seed) and the front door answers.
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'guest-recovery: guest restarted and converged'",
        timeout=600,
    )
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)

    # The ladder stopped at one. It restarts on a schedule of incidents, not on a timer.
    restarts = int(host.succeed("journalctl -u briard-agent | grep -c 'restarting an unresponsive guest' || true").strip())
    assert restarts == 1, f"{restarts} restart alerts for one incident -- the ladder is flapping"

    print("host recovered an unresponsive guest by restarting its VM")
    print(host.succeed("journalctl -u briard-agent | tail -40"))
  '';
}
