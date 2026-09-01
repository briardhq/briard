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

    # The shipped NIC contract, same shape as agent-bringup: a carrier-bearing veth parent, the
    # guest's two LAN NICs as macvtap children in install.sh's order (sys0 -> eth1, svc0 -> eth2),
    # and the private host<->guest link as a plain tap holding 10.11.9.1/24. All three or none --
    # qemu.go assigns NICs positionally, so omitting sys0 lands the witness NIC on eth2 and the
    # private link silently fails to exist.
    #
    # ⚠️ THIS BUILT A HOST-SIDE MACVLAN SHIM until [V3b.19a], because macvtap isolates host<->guest
    # and L1 could not otherwise curl the VIP -- the rig handing itself a reachability the product
    # did not have. The curls below are unchanged and now pass because the agent routes the VIP over
    # the private link. Note the isolation itself is still real and still load-bearing HERE: it is
    # precisely why the recovery ladder cannot probe the service before deciding.
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )
    host.succeed("qemu-img create -f qcow2 -b ${guestDisk}/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    host.succeed(
        "systemd-run --unit=briard-agent --collect "
        # The PATH install.sh gives the shipped unit (scripts/install.sh, "Environment=PATH="). The
        # agent shells out to systemd-run, systemctl and -- since [V3b.19] -- `ip`, all BY NAME, and
        # a transient unit's default PATH resolves none of them reliably. Pinning the shipped value
        # is the point: the rig gets what the product gets ([V3b.19a]).
        "--setenv=PATH=/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin "
        "--setenv=QEMU=${pkgs.qemu}/bin/qemu-system-x86_64 --setenv=ACCEL=kvm:tcg "
        "--setenv=GUEST_DISK=/tmp/guest.qcow2 --setenv=DATA_DISK=/tmp/data.img "
        # The guest console, because this test's failure mode is "the guest never answered" and
        # everything else here observes from OUTSIDE the VM. agent-readopt/deadman/watchdog all
        # carry it for the same reason ([[guest-console-is-the-window]]).
        "--setenv=GUEST_SERIAL=/tmp/guest-serial.log "
        "--setenv=CONTROL_SOCK=/run/briard-ctl.sock --setenv=NODE=guest "
        "--setenv=SYSTEM_TAP=sys0 --setenv=SYSTEM_DEV=eth1 --setenv=SYSTEM_CIDR=10.0.0.1/24 --setenv=SYSTEM_HOST_CIDR=10.0.0.129/32 --setenv=WITNESS_CIDR=10.11.9.2/24 --setenv=SERVICE_TAP=svc0 --setenv=WITNESS_TAP=briard-priv0 --setenv=STATUS_EVERY=2s "
        "--setenv=VIP_DEV=eth2 --setenv=VIP_ADDR=192.168.1.100/24 "
        "--setenv=NET_MODE=macvtap --setenv=NET_WRAP_BIN=${netWrap}/bin/briard-net-wrap "
        "${agent}/bin/briard-agent run"
    )

    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)

    # === PHASE 1: the VM is GONE (not merely quiet) -- relaunched at once, no window. ===
    #
    # These are two different situations that reach the host as ONE signal, a dead control channel.
    # Because of `-no-reboot` a guest that reboots itself EXITS, so "the unit stopped" is the
    # ordinary case, not the exotic one -- and waiting ten minutes for a socket belonging to a dead
    # process would be pure outage. The agent asks systemd instead of inferring from the channel.
    #
    # The timing assertion is the whole point: a regression here is SILENT, because the node still
    # recovers, just a window later. Killing QEMU is exactly what the guest's own graceful reboot
    # looks like from out here.
    gone = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    print(f"killing the guest QEMU (pid {gone}) -- the shape of a guest that rebooted itself")
    killed_at = time.monotonic()
    host.succeed(f"kill -9 {gone}")

    # [V3b.19] THE HOST'S ROUTE FOLLOWS THE GUEST AWAY, and this is the live proof of the rule that
    # matters most: the agent is ALIVE and the channel is DEAD, which is exactly when a peer may
    # have taken the VIP over. Leaving a /32 pointing at our own dead guest would replace the
    # working LAN path to that peer with a black hole, so the withdrawal fails OPEN.
    #
    # It lives here rather than in install-macvtap because this is where the perturbation already
    # is: a withdrawal needs the agent running, and an agent running when its guest goes away
    # relaunches it -- which is this test's whole subject, and was a race anywhere else.
    host.wait_until_fails("ip route get 192.168.1.100 | grep -q briard-priv0", timeout=180)
    print("the host route was withdrawn while the guest was gone")

    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'the guest unit is stopped; relaunching'", timeout=180
    )
    relaunch_took = time.monotonic() - killed_at
    assert relaunch_took < 300, f"took {relaunch_took:.0f}s to notice a STOPPED unit -- it is waiting out the silence window instead of asking systemd"
    print(f"agent noticed the stopped unit and relaunched after {relaunch_took:.0f}s")

    try:
        host.wait_until_succeeds(
            "journalctl -u briard-agent | grep -q 'guest relaunched and converged'", timeout=900
        )
    except Exception:
        # The guest is the only place that knows why it did not answer, and the console is the only
        # way in once the control channel is down ([[guest-console-is-the-window]]).
        print("=== guest console ===")
        print(host.execute("tail -120 /tmp/guest-serial.log")[1])
        print("=== agent ===")
        print(host.execute("journalctl -u briard-agent --no-pager | tail -40")[1])
        raise
    restarted = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    assert restarted != gone, f"same qemu pid {restarted} -- nothing was relaunched"
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    # ...and comes back with it. The curl above already needs the route (this host has no other
    # path to the VIP), but assert the route itself too: the pair withdrawn-then-restored is what
    # says the agent is TRACKING the guest rather than having got lucky once at bring-up.
    host.succeed("ip route get 192.168.1.100 | grep -q briard-priv0")
    print(f"guest back after a clean exit: pid {gone} -> {restarted}, route restored")

    # === PHASE 1b: relaunching INTO a stop that has not finished. ===
    #
    # `is-active` says "not active" about a unit that is spending its ExecStop, so the ladder
    # correctly decides the VM is not running and launches -- into a name systemd has not released
    # yet. systemd-run refuses it ("already loaded or has a fragment file"), and because a refusal
    # costs milliseconds, all three relaunch attempts are spent inside one logged second, on a
    # condition that clears by itself in a few. The node then sat out the two-hour cadence with
    # nothing left to try: [V3b.18], measured on a stranger's machine.
    #
    # The window is made WIDE and certain rather than raced for. SIGSTOP the guest QEMU first: the
    # control channel dies at once, and the graceful stop that follows cannot complete, because the
    # ExecStop's ACPI request has nothing awake to answer it. So the unit sits in `deactivating`
    # for the full TimeoutStopSec (75s) before systemd kills it -- which is also the honest test of
    # the wait's bound, since a wait that gave up earlier than systemd does would be no wait at all.
    print("freezing the guest QEMU and asking systemd to stop the unit -- the stop cannot finish")
    stopping = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    host.succeed(f"kill -STOP {stopping}")
    host.succeed("systemctl stop --no-block briard-guest.service")
    host.wait_until_succeeds(
        "test \"$(systemctl show -p ActiveState --value briard-guest.service)\" = deactivating", timeout=60
    )

    relaunched_before = int(host.succeed(
        "journalctl -u briard-agent | grep -c 'guest relaunched and converged' || true"
    ).strip())

    # THE ASSERTION THAT MUST BE ABLE TO FAIL: the node comes back. Without the wait the three
    # attempts are gone in a second and this times out -- the guest is never relaunched at all
    # until the two-hour cadence comes round.
    host.wait_until_succeeds(
        "test \"$(journalctl -u briard-agent | grep -c 'guest relaunched and converged' || true)\" "
        f"-gt {relaunched_before}",
        timeout=900,
    )
    # And it came back by WAITING, not by getting lucky: the refusal never happened.
    host.fail("journalctl -u briard-agent | grep -q 'already loaded or has a fragment file'")
    host.wait_until_fails(f"kill -0 {stopping}", timeout=60)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    print("guest relaunched out of a stop that had not finished")

    # === PHASE 2: the guest is RUNNING but wedged -- the window, then the power cycle. ===
    # --- the guest stops answering, and never starts again on its own ---
    # SIGSTOP the nested QEMU: the whole VM freezes, so the control channel dies AND every
    # re-dial the host makes finds a socket nothing is reading. This is the shape of the case
    # the ladder exists for -- indistinguishable, from outside, from a guest that has crashed.
    frozen = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    print(f"freezing the guest QEMU (pid {frozen}) and leaving it frozen")
    host.succeed(f"kill -STOP {frozen}")
    froze_at = time.monotonic()
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'control channel down'", timeout=60)

    # The host must NOT act on this yet. Nothing here distinguishes a wedged guest from a
    # channel that is about to come back, which is the whole reason for the wait.
    host.fail("journalctl -u briard-agent | grep -q 'restarting an unresponsive guest'")

    # It waits out the recovery window, then says what it is about to do -- on the local trail
    # `briard alerts` reads (notify.LogMarker), because on the free tier that trail is the only
    # delivery there is. The timeout is generous against the 10-minute window plus the ~10s each
    # re-dial spends on its handshake deadline.
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'alert \\[warning\\] Briard: the guest has stopped answering'",
        timeout=1200,
    )
    waited = time.monotonic() - froze_at

    # THE PATIENCE ASSERTION, and it is the reason this test costs a quarter of an hour.
    #
    # The bound is not arbitrary: the host's window sits ABOVE the guest deadman's own threshold
    # (6m + up to 60s of jitter) so that a guest able to reboot itself gracefully gets to do so,
    # and the host power-cycle is the backstop for when that failed. A shortened window inverts
    # that -- the host would win every race and the clean demote would never happen -- while
    # leaving every other assertion in this file passing. So the floor is checked against the
    # guest's LATEST possible fire, not against a round number.
    assert waited >= 420, f"host acted after only {waited:.0f}s -- inside the guest deadman's own window (6m+jitter), so the host would pre-empt the graceful reboot it is supposed to back up"
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
    # One query, and the pattern must match QEMU rather than anything mentioning the disk -- a bare
    # `pgrep -f guest.qcow2` also matches the transient `systemd-run` wrapper the agent launches
    # through, which exits moments later (see agent-deadman for the measured failure).
    restarted = host.wait_until_succeeds(
        "pgrep -f 'qemu-system-x86_64.*guest.qcow2'", timeout=300
    ).strip().splitlines()[0]
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

    # ANNOUNCED ONCE. recover() re-evaluates every time the window closes, which on a guest that
    # never came back is forever -- so an alert that re-fired would move the flap this rung exists
    # to prevent from the VM to the owner's phone.
    alerts = int(host.succeed("journalctl -u briard-agent | grep -c 'Briard: the guest has stopped answering' || true").strip())
    assert alerts == 1, f"{alerts} degraded alerts for one incident -- the announce-once latch is broken"

    # AND THE INCIDENT CLOSES ITSELF. The guest came back, so the owner is told it is over. This
    # is the half that used to be missing: the ladder announced trouble and then never mentioned
    # it again either way, so a resolved incident looked exactly like an ignored one.
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'alert \\[recovered\\] Briard: the guest is answering again'",
        timeout=120,
    )

    print("host recovered an unresponsive guest by restarting its VM")
    print(host.succeed("journalctl -u briard-agent | tail -40"))
  '';
}
