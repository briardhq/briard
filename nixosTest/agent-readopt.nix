# An agent restart is transparent to the guest, the load-bearing prerequisite for host-agent
# self-update. The guest VM is its own transient service (briard-guest.service), a sibling cgroup
# of briard-agent.service, so `systemctl restart briard-agent` kills only the agent cgroup and the
# restarted agent re-adopts qemu over the persisted control socket instead of booting a second
# VM. Were qemu a child of the agent, a restart would kill the whole cgroup → guest reboot → the
# node stops serving. Proven: the same qemu keeps running, the node stays primary (a lone node:
# no DRBD), and the VIP is served with zero interruption.
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

      # The agent as a real systemd service (the product shape), so `systemctl restart` is
      # well-defined. wantedBy=[]: the testScript starts it once the taps and disks exist.
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
          # Where the host keeps its guest bundle tree: the agent default (config.go), stated.
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
          # The test declares its address (the image bakes none; unset means DHCP, unanswered
          # here). HEALTH_URL stays unset: the probe targets the address the guest holds.
          VIP_DEV = "eth2";
          VIP_ADDR = "192.168.1.100/24";
          NET_MODE = "macvtap";
          NET_WRAP_BIN = "${netWrap}/bin/briard-net-wrap";
          STATUS_EVERY = "2s";
          GUEST_SERIAL = "/tmp/guest-serial.log"; # capture the guest console for post-mortem
          # Act 1b waits out the recovery window on a mute guest; shortened as in agent-recover,
          # under the same guard (guestrecover_test.go: the shipped value outlasts the deadman).
          BRIARD_GUEST_RECOVERY_WINDOW = "60s";
        };
      };
    };

  testScript = ''
    import time

    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # The shipped NIC contract: a carrier-bearing parent, the guest's two LAN NICs as macvtap
    # children in the agent's order (sys0 -> eth1, svc0 -> eth2, the VIP), and the private
    # host<->guest link as a plain tap at 10.11.9.1/24.
    #
    # ⚠️ The VIP poller below is why this rig matters most: it samples the VIP every 0.5s across
    # `systemctl restart briard-agent` and asserts zero dropped ticks, and since the agent owns the
    # host route to the VIP it measures the route too. A reconcile withdrawing the route whenever
    # its context cancelled would blip reachability on every restart (a self-update is one) for a
    # guest that never stopped serving (viproute.go guards it). A macvlan shim would take the
    # route off this path, hiding it from the poller.
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
    host.succeed("systemctl start briard-agent")
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)

    # The guest runs as its own transient service, detached from the agent.
    host.succeed("systemctl is-active briard-guest.service")
    qemu_before = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    print(f"guest qemu pid before restart: {qemu_before}")

    # One PASS/FAIL line per ~0.5 s tick across the restart; a demote, VIP move or guest reboot
    # shows FAILs. A detached background shell, not systemd-run, so it inherits the full PATH
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

    # === The proof: restart the agent; the guest must be untouched. ===
    host.succeed("systemctl restart briard-agent")

    # The new agent re-adopts the active guest (no second VM).
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

    # 3) The guest qemu is still running. (The role was read above: the first status line after
    #    the restart says primary=true quorate=true.)
    host.succeed("test $(pgrep -cf guest.qcow2) -ge 1")

    # 4) Zero VIP interruption: after all 80 ticks, not one FAIL, the strongest form of "no
    #    demote / no failover".
    host.wait_until_succeeds("test $(wc -l < /tmp/vip.log) -ge 80", timeout=90)
    fails = int(host.succeed("grep -c FAIL /tmp/vip.log || true").strip() or "0")
    passes = int(host.succeed("grep -c PASS /tmp/vip.log || true").strip() or "0")
    print(f"VIP poll across restart: {passes} PASS / {fails} FAIL")
    assert passes >= 20, f"poller barely ran ({passes} PASS) — not a real window"
    assert fails == 0, f"VIP dropped {fails} tick(s) across the agent restart — the guest was disturbed"

    print("agent restart was transparent to the guest: same qemu, Primary held, VIP uninterrupted")
    print(host.succeed("journalctl -u briard-agent | tail -30"))

    # === Act 1b: the agent is restarted over a guest that will never answer. ===
    # Re-adoption meeting a guest alive and dead inside (a vCPU WHPX gave up on, a kernel halted in
    # a panic), which the restarted agent adopts and never hears from. An agent that exited cleanly
    # when the bring-up budget ran out ("systemd restarts it") would stay down: under
    # Restart=on-failure a clean exit is a normal stop, leaving a dead qemu holding the sockets
    # and nothing said. agent-recover cannot stand in: it never restarts the agent, so its ladder
    # starts from the observe loop on a converged guest; here it must start from bring-up, with no
    # prior channel, in a real systemd service with a real restart policy, which only this rig has.
    #
    # Only this act proves three things: the agent does not exit (the invocation that adopted the
    # frozen guest relaunches it); the local door answers while the ladder waits, saying what it
    # does; and the restart alert carries the guest last words from the console capture. The
    # relaunch and window timing are agent-recover's, asserted here only as far as needed.
    frozen = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    print(f"freezing the guest QEMU (pid {frozen}) and restarting the agent over it")
    host.succeed(f"kill -STOP {frozen}")
    invocation = host.succeed("systemctl show -p InvocationID --value briard-agent").strip()
    since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()
    host.succeed("systemctl restart briard-agent")
    adopted_at = time.monotonic()
    restarted_invocation = host.succeed("systemctl show -p InvocationID --value briard-agent").strip()
    assert restarted_invocation != invocation, "the restart did not start a new agent"

    # The new agent spends its bring-up budget (the shipped five minutes) on a handshake nothing
    # answers; then the ladder, not an exit.
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since}' | grep -q 're-adopting running guest'", timeout=120
    )
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since}' | grep -q 'the guest never answered during bring-up'",
        timeout=480,
    )
    print(f"the ladder took the mute guest {time.monotonic() - adopted_at:.0f}s after the adopt")

    # The agent did not exit: the adopting invocation still runs, with no restart counted. One that
    # exited would be gone here (exit 0, Restart=on-failure, nothing comes back).
    host.succeed("systemctl is-active briard-agent")
    now_invocation = host.succeed("systemctl show -p InvocationID --value briard-agent").strip()
    assert now_invocation == restarted_invocation, (
        f"the agent was restarted again ({restarted_invocation} -> {now_invocation}) -- it exited on the mute guest"
    )

    # The door answers meanwhile: a status during the wait is refused with what the ladder is
    # doing and the way out, not left hanging until the agent exits, as on a channel with no
    # reader.
    rc, out = host.execute("timeout 20 ${agent}/bin/briard-agent directive status 2>&1")
    print(f"status during the ladder's wait (rc {rc}): {out.strip()}")
    assert "the VM is not answering" in out and "briard stop" in out, (
        f"the door must answer during the ladder with its sentence and the way out; got rc {rc}: {out!r}"
    )

    # The window closes, the forced stop takes the frozen qemu, and a different one comes up.
    host.wait_until_succeeds(
        "${agent}/bin/briard-agent alerts | sed -n '/open now/,/history/p' | grep -q 'the VM has stopped answering'",
        timeout=300,
    )
    # The cause, from the log: the alert body carries the last console lines, words of the guest,
    # not ours (here its boot output, as a frozen VM prints nothing; on a panic, the panic line).
    alert = host.succeed("${agent}/bin/briard-agent alerts | sed -n '/open now/,/history/p' | grep 'the VM has stopped answering'")
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
        "${agent}/bin/briard-agent alerts | sed -n '/history/,$p' | grep -q 'the VM is answering again'",
        timeout=120,
    )
    # ...by the same agent, start to finish.
    assert host.succeed("systemctl show -p InvocationID --value briard-agent").strip() == restarted_invocation
    print(f"a mute guest met at bring-up was relaunched by the ladder, not abandoned: pid {frozen} -> {relaunched}")

    # === Act 2: the other half of the contract. ===
    # Act 1 proved stopping the agent leaves the guest alone; this proves stopping the guest unit
    # powers the machine down rather than pulling its plug: the case of a host reboot, which stops
    # every unit. Without the unit ExecStop, the stop SIGTERMs qemu, and the guest takes its owner
    # installing distro updates as a power cut. The agent is stopped first,
    # deliberately, mirroring a host shutdown: the clean stop must not need the daemon alive. The
    # mechanism is the unit file, which outlives the agent that wrote it.
    host.succeed("systemctl stop briard-agent.service")
    host.succeed("systemctl is-active briard-guest.service")  # still serving: act 1, restated
    serial_before = int(host.succeed("wc -c < /tmp/guest-serial.log").strip())

    stop_start = host.succeed("date +%s").strip()
    host.succeed("systemctl stop briard-guest.service")
    stop_secs = int(host.succeed("date +%s").strip()) - int(stop_start)
    print(f"stopping briard-guest.service took {stop_secs}s")

    # 1) Our side: the ExecStop ran, spoke QMP, and waited for QEMU to be gone.
    host.succeed("journalctl -u briard-guest.service | grep -q 'guest-shutdown: the guest powered off cleanly'")

    # 2) The guest side, which our own logging cannot fake: `reboot: Power down` is printed only at
    #    the end of a full systemd shutdown (targets stopped, filesystems unmounted, page cache
    #    flushed); a SIGTERMed qemu console just stops mid-line. This one line is the durability
    #    claim, made by the machine rather than about it.
    serial = host.succeed("cat /tmp/guest-serial.log")
    tail = serial[serial_before:]
    # Print the non-blank lines, not a byte window: the console tail is a long run of blanks, so
    # `tail[-2000:]` shows a screenful of nothing on the run where someone needs to read it.
    spoken = [ln for ln in tail.splitlines() if ln.strip()]
    print(f"guest console after the stop ({len(tail)} bytes, {len(spoken)} non-blank lines):")
    print("\n".join(spoken[-40:]))
    assert "reboot: Power down" in tail, (
        "the guest never reached a clean power-off — it was killed, not shut down"
    )

    # 3) The ExecStop did it, not TimeoutStopSec expiring into a SIGKILL. The bound excludes the
    #    TimeoutStopSec path (75s) rather than being tight: a 65s clean shutdown is still clean,
    #    and the guest shutdown measured ~25s. What must never pass is a unit that waits out its
    #    timeout and power-cuts the guest anyway, a power cut wearing the clean stop log line.
    assert stop_secs < 70, f"the stop took {stop_secs}s — that is the timeout killing it, not a clean powerdown"

    host.fail("systemctl is-active briard-guest.service")
    print(f"stopping the guest unit powered the guest down cleanly in {stop_secs}s")
  '';
}
