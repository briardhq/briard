# systemd's watchdog restarts a host agent that is ALIVE BUT WEDGED (V3.32) — the one rung of the
# unresponsive-guest ladder that neither the guest deadman nor the host's own recovery can reach.
# A crashed agent self-heals through Restart=; a HUNG one does not, because systemd sees a healthy
# process and nothing fires.
#
# HOW THIS WEDGES THE AGENT, AND WHY IT IS NOT SIGSTOP.
#
# SIGSTOP is the obvious way to freeze an agent and it is the WRONG one: it stops every goroutine,
# including whatever is sending the pings, so the watchdog trips and the test passes *for a naive
# timer-goroutine implementation too*. It would prove the unit config and the restart, and nothing
# about the part that is hard.
#
# So the wedge here leaves the PROCESS entirely alive and blocks only the observe goroutine, which
# is the real failure shape. It needs no fault-injection hook, because the product already has the
# shape: writeTelemetry does an os.WriteFile that takes no context, in the observe loop, every
# cycle. Point TELEMETRY_PATH at a path whose ".tmp" sibling is a FIFO with no reader and open(2)
# blocks forever — an uninterruptible syscall, no deadline, no ctx to honour. Go hands the P off to
# another thread, so the runtime, the timers and every other goroutine keep running perfectly. A
# bare `time.Ticker` pinger would go on reporting liveness through the whole thing.
#
# THAT IS THE MUTATION CHECK, and it is the point of the test: replace beat/lease with a timer
# goroutine and this test must FAIL (no restart, the wait_until_succeeds times out). A test that
# cannot fail against the design it rejects proves only that systemd works.
#
# It also asserts the two things that make the feature worth having rather than merely working:
#   * the GOROUTINE DUMP. WatchdogSignal defaults to SIGABRT and Go answers it with every
#     goroutine's stack, so the trip names the wedge instead of just clearing it. That needs
#     GOTRACEBACK=all — the default dumps one arbitrary goroutine and is useless here.
#   * RE-ADOPT. The guest must be untouched across the trip. A watchdog that costs a guest reboot
#     on every fire is a cure worse than the disease (agent-readopt.nix proves the same property
#     for a deliberate restart; this proves it for an involuntary one).
#
# And, before any of that, it proves READY=1 means THE AGENT rather than THE NODE: under
# Type=notify `systemctl start` blocks until READY, so the start returning in seconds — long
# before the guest has converged — is the assertion. With READY gated on first healthy
# convergence, as it was before V3.32, this unit would sit in `activating` past TimeoutStartSec
# and be killed.
#
# Heavy (nested VM), so it rides the `integration` tag. Run:
#   nix build .#tests.agent-watchdog -L
{ pkgs, guestDisk, agent, netWrap }:
pkgs.testers.runNixOSTest {
  name = "agent-watchdog";
  skipTypeCheck = true; # dynamic asserts

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 4096; # room for L1 + the nested 2G guest
      virtualisation.cores = 4;
      virtualisation.diskSize = 10240;
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ]; # expose vmx -> nested KVM
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl ];

      # THE SHIPPED WATCHDOG SHAPE. These four lines are the ones install.sh writes; the rest of
      # the unit mirrors agent-readopt's harness. WatchdogSec is what makes WATCHDOG_USEC appear
      # in the agent's environment, which is the agent's ONLY source for its ping interval — so
      # this number has one definition and the Go side cannot drift from it.
      systemd.services.briard-agent = {
        description = "Briard host agent (watchdog proof)";
        wantedBy = [ ];
        path = [
          pkgs.qemu
          pkgs.iproute2
          pkgs.systemd
        ];
        serviceConfig = {
          Type = "notify";
          NotifyAccess = "main";
          TimeoutStartSec = 30; # bounds a config read, NOT a bring-up: READY precedes bringUp
          WatchdogSec = 20;
          ExecStart = "${agent}/bin/briard-agent";
          Restart = "on-failure"; # covers a watchdog timeout, not only a non-zero exit
          RestartSec = 2;
        };
        environment = {
          QEMU = "${pkgs.qemu}/bin/qemu-system-x86_64";
          ACCEL = "kvm:tcg";
          GUEST_DISK = "/tmp/guest.qcow2";
          DATA_DISK = "/tmp/data.img";
          CONTROL_SOCK = "/run/briard-ctl.sock";
          NODE = "guest";
          SERVICE_TAP = "svc0";
          VIP_DEV = "eth1";
          VIP_ADDR = "192.168.1.100/24";
          NET_MODE = "macvtap";
          NET_WRAP_BIN = "${netWrap}/bin/briard-net-wrap";
          STATUS_EVERY = "2s";
          GUEST_SERIAL = "/tmp/guest-serial.log";
          # The wedge point. Off on a shipped install; the soak sets it, and so do we — the
          # blocking write is the product's, not the test's.
          TELEMETRY_PATH = "/tmp/telemetry.json";
          # Without this the SIGABRT dump names one arbitrary goroutine instead of the wedged one.
          GOTRACEBACK = "all";
        };
      };
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # Service L2 on the macvtap substrate (as agent-readopt): carrier-bearing parent, the guest's
    # service NIC as a macvtap child, and a host-side macvlan shim so L1 can reach the VIP.
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name shim0 type macvlan mode bridge && ip addr add 192.168.1.1/24 dev shim0 && ip link set shim0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up"
    )
    host.succeed("qemu-img create -f qcow2 -b ${guestDisk}/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    # === 1) READY MEANS THE AGENT, NOT THE NODE ===
    # Under Type=notify this call blocks until READY=1. It has to return in seconds, well before
    # the guest converges (that takes minutes). Gated on first healthy convergence, as it was
    # before V3.32, systemd would kill this unit at TimeoutStartSec=30 instead.
    t0 = int(host.succeed("date +%s").strip())
    host.succeed("systemctl start briard-agent")
    t1 = int(host.succeed("date +%s").strip())
    started = t1 - t0
    print(f"systemctl start returned after {started}s (TimeoutStartSec=30)")
    assert started < 25, (
        f"start took {started}s — READY is arriving too late to be 'the agent started'; "
        "if it is gated on node health again, this unit dies at TimeoutStartSec"
    )
    # ...and it is genuinely ready BEFORE the node is healthy, which is the whole point.
    host.succeed("systemctl is-active briard-agent")
    host.fail("journalctl -u briard-agent | grep -q CONVERGED")
    # The watchdog is armed only once start-up completes, so this ordering is also what puts the
    # ladder under a watchdog at all. The agent says so on the way past.
    host.succeed("journalctl -u briard-agent | grep -q 'systemd watchdog armed'")

    # Now let it finish the job it was ready to do.
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
    host.succeed("systemctl is-active briard-guest.service")

    qemu_before = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    restarts_before = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    print(f"guest qemu pid {qemu_before}, agent restarts so far {restarts_before}")

    # Prove telemetry is actually being written before we sabotage it, or the wedge below would be
    # a no-op that this test could not distinguish from a working watchdog.
    host.wait_until_succeeds("test -s /tmp/telemetry.json", timeout=30)

    since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()

    # === 2) THE WEDGE: alive, but the observe goroutine is gone ===
    # os.WriteFile opens "<TELEMETRY_PATH>.tmp" O_WRONLY|O_CREATE|O_TRUNC. On a FIFO with no
    # reader that open(2) blocks forever. No ctx, no deadline, nothing to cancel.
    host.succeed("rm -f /tmp/telemetry.json.tmp && mkfifo /tmp/telemetry.json.tmp")

    # The process must still be ALIVE and running other goroutines — that is what separates this
    # from SIGSTOP, and what a bare-timer pinger would sail straight through.
    host.wait_until_succeeds("test $(pgrep -cf 'bin/briard-agent') -ge 1", timeout=10)

    # === 3) THE WATCHDOG FIRES ===
    # WatchdogSec=20, so the miss lands within ~20s of the last beat; allow generous slack for a
    # loaded nested VM. NRestarts growing is the assertion systemd itself makes.
    host.wait_until_succeeds(
        f"test $(systemctl show -p NRestarts --value briard-agent) -gt {restarts_before}",
        timeout=120,
    )
    host.succeed(f"journalctl -u briard-agent --since='{since}' | grep -qi 'watchdog'")

    # === 4) AND IT LEFT THE DIAGNOSIS BEHIND ===
    # SIGABRT + GOTRACEBACK=all: every goroutine's stack at the moment it wedged. This is the half
    # of the feature that closes the bug rather than clearing it.
    host.succeed(f"journalctl -u briard-agent --since='{since}' | grep -q 'SIGABRT'")
    # NOT anchored with ^: journalctl prefixes every line with its own timestamp/unit
    # ("... briard-agent[841]: goroutine 1 [syscall]:"), so a start-of-line anchor here can never
    # match and the assertion would be vacuous in the direction that looks like a real failure.
    host.succeed(f"journalctl -u briard-agent --since='{since}' | grep -qE 'goroutine [0-9]+ \\['")
    # The dump must actually NAME the wedge — the whole reason for dumping every goroutine rather
    # than the one that happened to take the signal. Either frame proves it located the blocking
    # call; which of the two survives depends on inlining, which is not a property worth asserting.
    host.succeed(
        f"journalctl -u briard-agent --since='{since}' | grep -qE 'writeTelemetry|os\\.WriteFile'"
    )

    # === 5) THE GUEST WAS NEVER TOUCHED ===
    # An involuntary restart must re-adopt exactly as a deliberate one does.
    host.succeed("systemctl is-active briard-guest.service")
    qemu_mid = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    assert qemu_before == qemu_mid, (
        f"the watchdog cost the guest a reboot ({qemu_before} -> {qemu_mid}) — "
        "a cure worse than the disease"
    )

    # === 6) CLEAR THE FAULT AND CONVERGE AGAIN ===
    # Until the FIFO goes, every restarted agent wedges again at the same place and trips the same
    # watchdog — which is the correct behaviour, and incidentally re-proves re-adopt each round.
    since2 = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()
    host.succeed("rm -f /tmp/telemetry.json.tmp")
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since2}' | grep -q 're-adopting running guest'",
        timeout=180,
    )
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since2}' | grep -q CONVERGED",
        timeout=300,
    )
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)

    qemu_after = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    assert qemu_before == qemu_after, (
        f"guest qemu changed across the whole episode ({qemu_before} -> {qemu_after})"
    )
    # And the agent settles: no further watchdog trips once the fault is gone.
    settled = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    host.succeed("sleep 60")
    now = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    assert now == settled, (
        f"the agent kept tripping the watchdog after the fault cleared ({settled} -> {now}) — "
        "the beat is not covering steady-state operation"
    )
    print("watchdog: wedged observe loop caught, traceback captured, guest untouched")
  '';
}
