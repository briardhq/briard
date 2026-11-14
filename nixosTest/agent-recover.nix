# The host restarts a guest that stopped answering (the host-side rung of the ladder). The
# guest-side half is agent-deadman (the guest reboots itself when the host goes silent); this is
# the harder direction: the guest goes silent and the host, which cannot see inside it, must
# decide on its own that waiting is no longer recovery.
#
# The proof is the QEMU PID. agent-bringup freezes and thaws the guest, proving the host re-dials
# the same VM; here the guest is never thawed, so a pass cannot be a reconnect: the old PID is
# dead and a different one serves. Neither CONVERGED nor the VIP answering tells the two apart.
#
# The second assertion is the patience, which would otherwise rot silently. Restarting a wedged
# guest is safe only because the host waits out a window no ordinary channel bounce reaches (the
# in-guest agent exits per connection, an OS upgrade bounces the channel, a slow verb closes it;
# each heals in seconds). Shorten the window and this test would still pass while the product
# power-cycles VMs that were fixing themselves, so the run measures how long the host held off and
# fails if it acted too soon. The wait is the behaviour, hence minutes rather than seconds.
#
# Heavy (a nested VM + the shipped guest disk), so it rides the `integration` tag. Run:
#   nix build .#tests.agent-recover -L
{ pkgs, guestDisk, agent, netWrap, dressBase }:
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

    # The shipped NIC contract, as in agent-bringup: a carrier-bearing veth parent, the guest's two
    # LAN NICs as macvtap children in the agent's order (sys0 -> eth1, svc0 -> eth2), and the
    # private link as a plain tap holding 10.11.9.1/24. All three or none: qemu.go assigns NICs
    # positionally, so omitting sys0 lands the witness NIC on eth2 and the private link silently
    # fails to exist.
    #
    # ⚠️ No host-side macvlan shim: it would hand the rig a reachability the product lacks. The
    # curls pass because the agent routes the VIP over the private link. The macvtap isolation is
    # load-bearing here: it is why the ladder cannot probe the app before deciding.
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )
    host.succeed("ln -s ${guestDisk}/nixos.qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    # The guest bundle tree the install lays on every node: the image bakes no reverse proxy, so
    # an undressed guest cannot serve. Copied out of the store, as the host writes `guest.good`
    # beside it.
    host.succeed("mkdir -p /opt/briard/agent && cp -r ${dressBase}/. /opt/briard/agent/ && chmod -R u+w /opt/briard/agent")

    host.succeed(
        "systemd-run --unit=briard-agent --collect "
        # The shipped unit PATH (scripts/units/briard-agent.service): the agent runs systemd-run,
        # systemctl and `ip` (it owns the host route) by name, which a transient unit default
        # PATH does not reliably resolve. The rig gets what the product gets.
        "--setenv=PATH=/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin "
        "--setenv=QEMU=${pkgs.qemu}/bin/qemu-system-x86_64 --setenv=ACCEL=kvm:tcg "
        # Where the host keeps its guest bundle tree: the agent default (config.go), stated.
        "--setenv=UPDATE_BASE=/opt/briard/agent "
        "--setenv=GUEST_IMAGE=/tmp/guest.qcow2 --setenv=DATA_DISK=/tmp/data.img --setenv=STATE_DISK=/tmp/state.img "
        # The guest console: this failure mode is "the guest never answered", and everything else
        # here observes from outside the VM (the console is the window, as in agent-readopt,
        # deadman and watchdog).
        "--setenv=GUEST_SERIAL=/tmp/guest-serial.log "
        # The recovery window, shortened from the shipped ten minutes, which would spend ~90% of
        # the tier wall clock on one idle wait (and nobody waits out a two-day timer to watch it
        # fire). The shipped binary still runs on a real wedged guest; the assertion below checks
        # the host waited the configured window, the property that would rot. That the shipped
        # value outlasts the guest deadman is asserted in guestrecover_test.go.
        "--setenv=BRIARD_GUEST_RECOVERY_WINDOW=60s "
        "--setenv=CONTROL_SOCK=/run/briard-ctl.sock --setenv=NODE=guest "
        "--setenv=SYSTEM_TAP=sys0 --setenv=SYSTEM_DEV=eth1 --setenv=SYSTEM_CIDR=10.0.0.1/24 --setenv=SYSTEM_HOST_CIDR=10.0.0.129/32 --setenv=WITNESS_CIDR=10.11.9.2/24 --setenv=SERVICE_TAP=svc0 --setenv=WITNESS_TAP=briard-priv0 --setenv=STATUS_EVERY=2s "
        "--setenv=VIP_DEV=eth2 --setenv=VIP_ADDR=192.168.1.100/24 "
        "--setenv=NET_MODE=macvtap --setenv=NET_WRAP_BIN=${netWrap}/bin/briard-net-wrap "
        "${agent}/bin/briard-agent run"
    )

    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)

    # === Phase 1: the VM is gone (not merely quiet) -- relaunched at once, no window. ===
    # A gone VM and a quiet one reach the host as one signal, a dead control channel. With
    # `-no-reboot` a guest that reboots itself exits, so a stopped unit is the ordinary case, and
    # waiting ten minutes for a dead process socket would be pure outage: the agent asks systemd.
    # The timing assertion is the point, since a regression is silent (the node still recovers, a
    # window later). Killing QEMU is what the guest graceful reboot looks like from out here.
    gone = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    print(f"killing the guest QEMU (pid {gone}) -- the shape of a guest that rebooted itself")
    killed_at = time.monotonic()
    host.succeed(f"kill -9 {gone}")

    # The host route follows the guest away. Agent alive and channel dead is exactly when a peer
    # may have taken the VIP over, and a /32 left pointing at our dead guest would replace the
    # working LAN path to that peer with a black hole, so the withdrawal fails open. Asserted here,
    # not in install-macvtap: a withdrawal needs the agent running, and an agent running when its
    # guest goes away relaunches it, which is this test subject and a race anywhere else.
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
        # way in once the control channel is down (the console is the window).
        print("=== guest console ===")
        print(host.execute("tail -120 /tmp/guest-serial.log")[1])
        print("=== agent ===")
        print(host.execute("journalctl -u briard-agent --no-pager | tail -40")[1])
        raise
    restarted = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    assert restarted != gone, f"same qemu pid {restarted} -- nothing was relaunched"
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    # ...and the route comes back. The curl already needs it (no other path to the VIP), but the
    # withdrawn-then-restored pair shows the agent tracking the guest, not lucky once.
    host.succeed("ip route get 192.168.1.100 | grep -q briard-priv0")
    print(f"guest back after a clean exit: pid {gone} -> {restarted}, route restored")

    # === Phase 1b: relaunching into a stop that has not finished. ===
    # `is-active` says "not active" about a unit spending its ExecStop, so the ladder rightly
    # launches -- into a name systemd has not released. systemd-run refuses it ("already loaded or
    # has a fragment file") in milliseconds, so the relaunch burst (three attempts, a 30s floor
    # between failures) is spent on a condition that clears by itself, and the node sits out the
    # two-hour cadence with nothing left to try. The launch therefore waits for the name to come
    # free (platform waitUnitFree).
    #
    # The window is made wide and certain, not raced for: with QEMU SIGSTOPped the channel dies at
    # once and the ExecStop ACPI request has nothing awake to answer it, so the unit sits in
    # `deactivating` for the full TimeoutStopSec (75s) before systemd kills it. That also tests the
    # bound of the wait: one that gave up before systemd does would be no wait at all.
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

    # The assertion that must be able to fail: the node comes back. Without the wait each attempt
    # is refused while the unit is still stopping, and once the burst is spent the guest is not
    # relaunched until the two-hour cadence comes round.
    host.wait_until_succeeds(
        "test \"$(journalctl -u briard-agent | grep -c 'guest relaunched and converged' || true)\" "
        f"-gt {relaunched_before}",
        timeout=900,
    )
    # And it came back by waiting, not by luck: the refusal never happened.
    host.fail("journalctl -u briard-agent | grep -q 'already loaded or has a fragment file'")
    host.wait_until_fails(f"kill -0 {stopping}", timeout=60)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    print("guest relaunched out of a stop that had not finished")

    # === Phase 2: the guest is running but wedged -- the window, then the power cycle. ===
    # A SIGSTOPped QEMU freezes the whole VM: the channel dies and every re-dial finds a socket
    # nothing reads. The case the ladder exists for, indistinguishable from outside from a crash.
    frozen = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    print(f"freezing the guest QEMU (pid {frozen}) and leaving it frozen")
    host.succeed(f"kill -STOP {frozen}")
    froze_at = time.monotonic()
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'control channel down'", timeout=60)

    # The host must not act yet: nothing distinguishes a wedged guest from a channel about to come
    # back, which is the whole reason for the wait.
    host.fail("journalctl -u briard-agent | grep -q 'restarting an unresponsive guest'")

    # After the 60s window it says what it is about to do, in the node alert store (what
    # `briard alerts` reads; on the free tier the only delivery). Asserted through the verb, on the
    # open section: the guest is a condition, open from here until the guest answers.
    host.wait_until_succeeds(
        "${agent}/bin/briard-agent alerts | sed -n '/open now/,/history/p' | grep -q 'the VM has stopped answering'",
        timeout=300, # the 60s window plus the re-dials, with room to spare
    )

    waited = time.monotonic() - froze_at

    # The patience assertion, two-sided on purpose: the host waits the configured window, so the
    # timer is wired to the value the agent was given, not a number typed once. A floor alone
    # passes a host that waited far too long (a hung ladder looks patient); a ceiling alone passes
    # one that never waited. The slack covers noticing the drop before the window opens (the
    # liveness rule needs three missed calls) and the gate read and alert write after it closes.
    window = 60
    assert waited >= window, f"host acted after only {waited:.0f}s, inside its own {window}s window -- the wait is not driving the decision"
    assert waited < window + 90, f"host waited {waited:.0f}s for a {window}s window -- the ladder is not acting when the window closes"
    print(f"host held off for {waited:.0f}s against a {window}s window")

    # A frozen QEMU answers neither its agent nor the ACPI button, so the clean stop must fail and
    # the forced stop take it down. Asserted because which route ran is the single fact that
    # decides whether DRBD recorded its quorum state on the way out.
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'guest-recovery: could not stop the guest cleanly, forcing'",
        timeout=180,
    )

    # The proof: the frozen process is gone and a different QEMU runs, so a restart of the VM
    # recovered the node, not a re-dial. A reconnect-only implementation fails here.
    host.wait_until_fails(f"kill -0 {frozen}", timeout=180)
    # The pattern must match QEMU: a bare `pgrep -f guest.qcow2` also matches the transient
    # `systemd-run` wrapper, which exits moments later (agent-deadman has the measured failure).
    restarted = host.wait_until_succeeds(
        "pgrep -f 'qemu-system-x86_64.*guest.qcow2'", timeout=300
    ).strip().splitlines()[0]
    assert restarted != frozen, f"same QEMU pid {restarted} after recovery -- the guest was never restarted"
    print(f"guest restarted: pid {frozen} -> {restarted}")

    # A node again, not merely a process: bring-up re-drove the sequence on the persisted data disk
    # (idempotent as on the guest-side rung: attach, never re-seed) and the reverse proxy answers.
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'guest-recovery: guest restarted and converged'",
        timeout=600,
    )
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)

    # Announced once: recover() re-evaluates each time the window closes, forever on a guest that
    # never returns, so a re-firing alert would move the flap this rung prevents to the owner phone.
    alerts = int(host.succeed("journalctl -u briard-agent | grep -c 'Briard: the VM has stopped answering' || true").strip())
    assert alerts == 1, f"{alerts} degraded alerts for one incident -- the announce-once latch is broken"

    # The incident closes itself: without the all-clear, a resolved incident would look exactly
    # like an ignored one.
    host.wait_until_succeeds(
        "${agent}/bin/briard-agent alerts | sed -n '/history/,$p' | grep -q 'the VM is answering again'",
        timeout=120,
    )
    # ...and that resolve closed the condition: it is no longer among the open alerts.
    host.fail("${agent}/bin/briard-agent alerts | sed -n '/open now/,/history/p' | grep -q 'the VM has stopped answering'")


    print("host recovered an unresponsive guest by restarting its VM")
    print(host.succeed("journalctl -u briard-agent | tail -40"))
  '';
}
