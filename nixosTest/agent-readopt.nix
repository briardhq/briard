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
{ pkgs, guestDisk, agent, netWrap }:
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
      # has created the bridge/tap/overlay; the testScript starts it explicitly.
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
          GUEST_DISK = "/tmp/guest.qcow2";
          DATA_DISK = "/tmp/data.img";
          CONTROL_SOCK = "/run/briard-ctl.sock";
          NODE = "guest";
          SYSTEM_TAP = "sys0";
          SYSTEM_DEV = "eth1";
          SYSTEM_CIDR = "10.0.0.1/24";
          SYSTEM_HOST_CIDR = "10.0.0.129/32";
          WITNESS_CIDR = "10.11.9.2/24";
          SERVICE_TAP = "svc0";
          WITNESS_TAP = "briard-priv0";
          # The test declares its service address: the image bakes none (V3.19c step 3) and unset
          # means DHCP, which nothing answers here. HEALTH_URL stays unset so the agent resolves the
          # probe target from the address the guest actually holds.
          VIP_DEV = "eth2";
          VIP_ADDR = "192.168.1.100/24";
          NET_MODE = "macvtap";
          NET_WRAP_BIN = "${netWrap}/bin/briard-net-wrap";
          STATUS_EVERY = "2s";
          GUEST_SERIAL = "/tmp/guest-serial.log"; # capture the guest console for post-mortem
        };
      };
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # The shipped NIC contract: a carrier-bearing parent, the guest's two LAN NICs as macvtap
    # children in install.sh's order (sys0 -> eth1, svc0 -> eth2, the VIP), and the private
    # host<->guest link as a plain tap at 10.11.9.1/24.
    #
    # ⚠️ THE VIP POLLER BELOW IS WHY THIS RIG MATTERS MOST ([V3b.19a]). It samples the VIP every
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
    host.succeed("qemu-img create -f qcow2 -b ${guestDisk}/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    # Boot: the agent launches the guest as a transient service and drives bring-up.
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

    # === ACT 2 [V3.26a]: the other half of the same contract. ===
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
