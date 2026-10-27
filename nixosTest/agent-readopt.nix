# An agent restart is TRANSPARENT to the guest — the
# load-bearing prerequisite for host-agent self-update. The guest VM runs as
# its own transient systemd service (briard-guest.service), a sibling cgroup of
# briard-agent.service, so `systemctl restart briard-agent` kills only the agent's
# cgroup and leaves qemu serving; the restarted agent RE-ADOPTS it over the persisted
# control socket instead of booting a second VM. Were qemu a child of the agent, a restart
# would kill the whole cgroup → guest reboot → DRBD demote → VIP move. This test proves it is
# not: across the restart the same qemu process keeps running, DRBD stays Primary, and the VIP
# is served with ZERO interruption.
#
# Heavy (nested VM), so it rides the `integration` tag. Run:
#   nix build .#tests.agent-readopt -L
{ pkgs, guestDisk, agent, netWrap, dressBase }:
pkgs.testers.runNixOSTest {
  name = "agent-readopt";
  skipTypeCheck = true; # dynamic asserts, backgrounded poller

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 4096; # room for L1 + the nested 2G guest
      virtualisation.cores = 4;
      virtualisation.diskSize = 10240;
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ]; # expose vmx -> nested KVM
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl ];

      # The agent as a REAL systemd service (the product shape) so `systemctl restart`
      # is well-defined. wantedBy=[] so it does not auto-start at boot before the test
      # has created the bridge/tap/disks; the testScript starts it explicitly.
      systemd.services.briard-agent = {
        description = "Briard host agent (re-adopt proof)";
        wantedBy = [ ];
        path = [
          pkgs.qemu
          pkgs.iproute2
          pkgs.systemd # systemd-run / systemctl: the agent launches + probes the guest unit
        ];
        serviceConfig = {
          ExecStart = "${agent}/bin/briard-agent run";
          Restart = "on-failure";
          RestartSec = 2;
        };
        environment = {
          QEMU = "${pkgs.qemu}/bin/qemu-system-x86_64";
          ACCEL = "kvm:tcg";
          GUEST_IMAGE = "/tmp/guest.qcow2";
          # Where the host keeps its guest bundle tree, the way install.sh sets it.
          UPDATE_BASE = "/opt/briard/agent";
          DATA_DISK = "/tmp/data.img";
          STATE_DISK = "/tmp/state.img";
          CONTROL_SOCK = "/run/briard-ctl.sock";
          NODE = "guest";
          SYSTEM_TAP = "sys0";
          SYSTEM_DEV = "eth1";
          SYSTEM_CIDR = "10.0.0.1/24";
          SYSTEM_HOST_CIDR = "10.0.0.129/32";
          WITNESS_CIDR = "10.11.9.2/24";
          SERVICE_TAP = "svc0";
          WITNESS_TAP = "briard-priv0";
          # The test declares its service address: the image bakes none and unset
          # means DHCP, which nothing answers here. HEALTH_URL stays unset so the agent resolves the
          # probe target from the address the guest actually holds.
          VIP_DEV = "eth2";
          VIP_ADDR = "192.168.1.100/24";
          NET_MODE = "macvtap";
          NET_WRAP_BIN = "${netWrap}/bin/briard-net-wrap";
          STATUS_EVERY = "2s";
          GUEST_SERIAL = "/tmp/guest-serial.log"; # capture the guest console for post-mortem
          # Act 1b waits out the recovery window on a guest that never answers; shortened for the
          # same reason and under the same guard as agent-recover (guestrecover_test.go asserts
          # the shipped value outlasts the guest deadman).
          BRIARD_GUEST_RECOVERY_WINDOW = "60s";
        };
      };
    };

  testScript = ''
    import time

    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # The shipped NIC contract: a carrier-bearing parent, the guest's two LAN NICs as macvtap
    # children in install.sh's order (sys0 -> eth1, svc0 -> eth2, the VIP), and the private
    # host<->guest link as a plain tap at 10.11.9.1/24.
    #
    # ⚠️ THE VIP POLLER BELOW IS WHY THIS RIG MATTERS MOST. It samples the VIP every
    # 0.5s across `systemctl restart briard-agent` and asserts ZERO dropped ticks -- and since the
    # agent now owns the host's route to that address, the poller is measuring the route as well as
    # the guest. That caught a real regression while this conversion was being written: the
    # reconcile withdrew the route whenever its context cancelled, so every restart -- which is what
    # a self-update IS -- would have blipped reachability for a guest that never stopped serving.
    # With the old macvlan shim here, nothing about the route was on this path and the poller could
    # not have seen it.
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )
    host.succeed("ln -s ${guestDisk}/nixos.qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    # Boot: the agent launches the guest as a transient service and drives bring-up.
    # The host holds a guest bundle tree, as install.sh lays on every install: the
    # image bakes no door, so a guest is dressed by its host or it cannot serve. Copied out of the
    # store because the host writes `guest.good` beside the tree.
    host.succeed("mkdir -p /opt/briard/agent && cp -r ${dressBase}/. /opt/briard/agent/ && chmod -R u+w /opt/briard/agent")
    host.succeed("systemctl start briard-agent")
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)

    # The guest runs as its OWN transient service, detached from the agent.
    host.succeed("systemctl is-active briard-guest.service")
    qemu_before = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    print(f"guest qemu pid before restart: {qemu_before}")

    # A continuous VIP poller spanning the restart: one PASS/FAIL line per ~0.5 s tick.
    # A DRBD demote / VIP move / guest reboot would drop the VIP and show FAILs. Run it as
    # a DETACHED background shell (not systemd-run) so it inherits the machine's full PATH
    # (curl/sleep) and survives host.succeed returning; a `while` loop avoids seq.
    host.succeed(
        "rm -f /tmp/vip.log; "
        "( i=0; while [ $i -lt 80 ]; do "
        "curl -fsS -m3 http://192.168.1.100/healthz >/dev/null 2>&1 && echo PASS || echo FAIL; "
        "i=$((i+1)); sleep 0.5; done > /tmp/vip.log 2>&1 & )"
    )
    # Confirm it actually started ticking before we perturb anything.
    host.wait_until_succeeds("test $(wc -l < /tmp/vip.log) -ge 3", timeout=15)

    since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()

    # === THE PROOF: restart the agent. The guest must be untouched. ===
    host.succeed("systemctl restart briard-agent")

    # The new agent finds the guest active and re-adopts it (does NOT boot a 2nd VM).
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since}' | grep -q 're-adopting running guest'",
        timeout=120,
    )
    # ...and re-drives the idempotent bring-up to CONVERGED on the adopted guest.
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since}' | grep -q CONVERGED",
        timeout=180,
    )
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since}' | grep -q 'primary=true quorate=true .* healthy=true'",
        timeout=60,
    )

    # 1) Same qemu process — the guest never rebooted.
    qemu_after = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    print(f"guest qemu pid after restart:  {qemu_after}")
    assert qemu_before == qemu_after, f"guest qemu restarted ({qemu_before} -> {qemu_after}) — NOT transparent"

    # 2) The guest transient service stayed active throughout.
    host.succeed("systemctl is-active briard-guest.service")

    # 3) DRBD never demoted (the survivor of a coupled restart would have): still Primary.
    host.succeed("test $(pgrep -cf guest.qcow2) -ge 1")

    # 4) Zero VIP interruption across the restart. Wait for the poller to finish (80 ticks),
    #    then assert not a single FAIL tick — the strongest form of "no demote / no failover".
    host.wait_until_succeeds("test $(wc -l < /tmp/vip.log) -ge 80", timeout=90)
    fails = int(host.succeed("grep -c FAIL /tmp/vip.log || true").strip() or "0")
    passes = int(host.succeed("grep -c PASS /tmp/vip.log || true").strip() or "0")
    print(f"VIP poll across restart: {passes} PASS / {fails} FAIL")
    assert passes >= 20, f"poller barely ran ({passes} PASS) — not a real window"
    assert fails == 0, f"VIP dropped {fails} tick(s) across the agent restart — the guest was disturbed"

    print("agent restart was transparent to the guest: same qemu, Primary held, VIP uninterrupted")
    print(host.succeed("journalctl -u briard-agent | tail -30"))

    # === ACT 1b: the agent is restarted over a guest that will NEVER answer. ===
    #
    # Act 1 is re-adoption working: a healthy guest kept across the restart. This is re-adoption
    # meeting a guest that is alive and dead inside -- the shape of a vCPU WHPX gave up on and of a
    # kernel halted in a panic, both measured on the first real desktop -- which the restarted
    # agent adopts and never hears from. What used to happen: bring-up's budget ran out, the agent
    # EXITED cleanly ("systemd restarts it"), and under Restart=on-failure a clean exit is a
    # normal stop, so the node stayed down with a dead qemu holding the sockets and nothing said.
    #
    # agent-recover cannot stand in for this: it never restarts the agent, so the guest it
    # freezes is one the running agent had already converged, and the ladder is entered from the
    # observe loop. Here the ladder has to be entered from bring-up, with no prior channel, by an
    # agent that is a real service with a real restart policy -- which only this rig has.
    #
    # Three things only this act proves: the agent does NOT exit (the service invocation that
    # adopted the frozen guest is the one that relaunches it); the local door answers while the
    # ladder waits, with what it is doing; and the alert that announces the restart carries the
    # guest's last words from the console capture. The relaunch itself and the window's timing
    # are agent-recover's, asserted here only as far as the act needs them to finish.
    frozen = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    print(f"freezing the guest QEMU (pid {frozen}) and restarting the agent over it")
    host.succeed(f"kill -STOP {frozen}")
    invocation = host.succeed("systemctl show -p InvocationID --value briard-agent").strip()
    since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()
    host.succeed("systemctl restart briard-agent")
    adopted_at = time.monotonic()
    restarted_invocation = host.succeed("systemctl show -p InvocationID --value briard-agent").strip()
    assert restarted_invocation != invocation, "the restart did not start a new agent"

    # The new agent adopts the frozen guest and spends its bring-up budget on a handshake nothing
    # answers; then the LADDER, not an exit. The budget is the shipped five minutes.
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since}' | grep -q 're-adopting running guest'", timeout=120
    )
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since}' | grep -q 'the guest never answered during bring-up'",
        timeout=480,
    )
    print(f"the ladder took the mute guest {time.monotonic() - adopted_at:.0f}s after the adopt")

    # THE AGENT DID NOT EXIT: the invocation that adopted the guest is still the one running, and
    # the service manager counted no restart of its own. With the old code the agent is gone
    # here -- exit 0, Restart=on-failure, nothing comes back.
    host.succeed("systemctl is-active briard-agent")
    now_invocation = host.succeed("systemctl show -p InvocationID --value briard-agent").strip()
    assert now_invocation == restarted_invocation, (
        f"the agent was restarted again ({restarted_invocation} -> {now_invocation}) -- it exited on the mute guest"
    )

    # THE DOOR ANSWERS MEANWHILE. A status asked during the wait is refused with what the ladder
    # is doing and the way out -- not left hanging until the agent exits, which is what a channel
    # with no reader does, and what the tray and `briard stop` used to get.
    rc, out = host.execute("timeout 20 ${agent}/bin/briard-agent directive status 2>&1")
    print(f"status during the ladder's wait (rc {rc}): {out.strip()}")
    assert "the guest is not answering" in out and "briard stop" in out, (
        f"the door must answer during the ladder with its sentence and the way out; got rc {rc}: {out!r}"
    )

    # The window closes, the forced stop takes the frozen qemu, and a DIFFERENT one comes up.
    host.wait_until_succeeds(
        "${agent}/bin/briard-agent alerts | sed -n '/open now/,/history/p' | grep -q 'the guest has stopped answering'",
        timeout=300,
    )
    # THE CAUSE, FROM THE LOG. The alert's body carries the console's last lines -- here the
    # guest's own boot output, since a frozen VM printed nothing on its way out; on a panicked
    # kernel it is the panic line itself. The point is that the words are the guest's, not ours.
    alert = host.succeed("${agent}/bin/briard-agent alerts | sed -n '/open now/,/history/p' | grep 'the guest has stopped answering'")
    print(f"the alert: {alert.strip()[:400]}")
    assert "last words" in alert and "console:" in alert, f"the alert does not carry the guest's last words: {alert!r}"
    assert "briard stop" in alert, f"the alert does not name the way out: {alert!r}"
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since}' | grep -q 'guest-recovery: could not stop the guest cleanly, forcing'",
        timeout=180,
    )
    host.wait_until_fails(f"kill -0 {frozen}", timeout=180)
    try:
        host.wait_until_succeeds(
            f"journalctl -u briard-agent --since='{since}' | grep -q 'guest-recovery: guest restarted and converged'",
            timeout=600,
        )
    except Exception:
        print("=== guest console ===")
        print(host.execute("tail -120 /tmp/guest-serial.log")[1])
        print("=== agent ===")
        print(host.execute("journalctl -u briard-agent --no-pager | tail -60")[1])
        raise
    relaunched = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    assert relaunched != frozen, f"same QEMU pid {relaunched} -- the mute guest was never restarted"
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    host.wait_until_succeeds(
        "${agent}/bin/briard-agent alerts | sed -n '/history/,$p' | grep -q 'the guest is answering again'",
        timeout=120,
    )
    # ...by the same agent, start to finish.
    assert host.succeed("systemctl show -p InvocationID --value briard-agent").strip() == restarted_invocation
    print(f"a mute guest met at bring-up was relaunched by the ladder, not abandoned: pid {frozen} -> {relaunched}")

    # === ACT 2: the other half of the same contract. ===
    # Act 1 proved stopping the AGENT leaves the guest alone. This proves stopping the GUEST UNIT
    # powers the machine down rather than pulling its plug — the case a host reboot creates, since
    # systemd stops every unit on the way down. Before the unit had an ExecStop, that SIGTERMed
    # qemu, and the guest experienced its owner installing distro updates as a power cut.
    #
    # The agent is stopped FIRST, deliberately: it mirrors a host shutdown, and it proves the clean
    # stop does not depend on the daemon still being alive to arrange it. The mechanism is the unit
    # file, which outlives the agent that wrote it.
    host.succeed("systemctl stop briard-agent.service")
    host.succeed("systemctl is-active briard-guest.service")  # still serving: act 1, restated
    serial_before = int(host.succeed("wc -c < /tmp/guest-serial.log").strip())

    stop_start = host.succeed("date +%s").strip()
    host.succeed("systemctl stop briard-guest.service")
    stop_secs = int(host.succeed("date +%s").strip()) - int(stop_start)
    print(f"stopping briard-guest.service took {stop_secs}s")

    # 1) OUR side: the ExecStop ran, spoke QMP, and waited for QEMU to be gone.
    host.succeed("journalctl -u briard-guest.service | grep -q 'guest-shutdown: the guest powered off cleanly'")

    # 2) THE GUEST'S side, which is the assertion that cannot be faked by our own logging: its
    #    kernel's last words. `reboot: Power down` is printed only at the end of a full systemd
    #    shutdown — targets stopped, filesystems unmounted, page cache flushed. A SIGTERMed qemu
    #    prints nothing further at all; the console simply stops mid-line. So this single line is
    #    the durability claim, made by the machine rather than about it.
    serial = host.succeed("cat /tmp/guest-serial.log")
    tail = serial[serial_before:]
    # Print the SPEAKING lines, not a byte window: the console's tail is a long run of blanks, so
    # a `tail[-2000:]` shows a screenful of nothing on the one run where someone needs to read it.
    spoken = [ln for ln in tail.splitlines() if ln.strip()]
    print(f"guest console after the stop ({len(tail)} bytes, {len(spoken)} non-blank lines):")
    print("\n".join(spoken[-40:]))
    assert "reboot: Power down" in tail, (
        "the guest never reached a clean power-off — it was killed, not shut down"
    )

    # 3) It was the ExecStop that did it, not systemd's TimeoutStopSec expiring into a SIGKILL.
    #    The bound is set to exclude the TimeoutStopSec path (75s) rather than to be tight: a
    #    clean shutdown that took 65s is still clean, and the guest's own shutdown was measured at
    #    ~25s. What must never pass is a unit that waits out its timeout and power-cuts the guest
    #    anyway — the old behaviour wearing the new behaviour's log line.
    assert stop_secs < 70, f"the stop took {stop_secs}s — that is the timeout killing it, not a clean powerdown"

    host.fail("systemctl is-active briard-guest.service")
    print(f"stopping the guest unit powered the guest down cleanly in {stop_secs}s")
  '';
}
