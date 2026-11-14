# systemd's watchdog restarts a host agent that is alive but wedged: the rung of the
# unresponsive-guest ladder that neither the guest deadman nor the host's own recovery can reach.
# A crashed agent self-heals through Restart=; a hung one does not, because systemd sees a healthy
# process and nothing fires.
#
# The wedge is deliberately not SIGSTOP: that stops every goroutine, including whatever sends the
# pings, so a naive timer-goroutine implementation would pass too and the test would prove only
# the unit config and the restart. Here the process stays entirely alive and only the observe
# goroutine blocks, the real failure shape: open(2) on a FIFO with no reader blocks forever (an
# uninterruptible syscall, no deadline, no ctx to honour) while Go hands the P to another thread,
# so the runtime, the timers and every other goroutine keep running. A bare `time.Ticker` pinger
# would report liveness through the whole thing.
#
# The product has no such wedge of its own, by design: an un-ctx-bounded os.WriteFile of telemetry
# in the observe loop would be one (a reader-less FIFO at TELEMETRY_PATH's ".tmp" sibling, no
# fault-injection hook needed; telemetry, the least important thing in the loop, taking the
# node's supervisor down), so the write runs on its own goroutine (telemetryWriter). That design
# makes the shape unreachable, so the lever is explicit: `WEDGE_FIFO`, opened in the observe loop
# every cycle (wedgeForTest), a fixture named like one, set by nothing else and never by
# install.sh. The telemetry FIFO is an assertion instead (step 2): it must not take the agent
# down, and with the write back on the observe loop that step fails by tripping the watchdog it
# exists to prove is not needed there.
#
# That is the mutation check, and the point of the test: replace beat/lease with a timer goroutine
# and this test must fail (no restart, the wait_until_succeeds times out). A test that cannot fail
# against the design it rejects proves only that systemd works.
#
# It also proves the other direction, since a watchdog is only as good as its false-positive rate:
# work that legitimately takes minutes must not be killed. Step 3 runs an `app install` that
# outlasts WatchdogSec and asserts an outcome, not a SIGABRT; the install is an act, run off the
# observe loop (acts.go) under a budget lease (beat.go), so the loop keeps beating through it.
#
# And it asserts the two things that make the feature worth having rather than merely working:
#   * the goroutine dump. WatchdogSignal defaults to SIGABRT and Go answers it with every
#     goroutine's stack, so the trip names the wedge instead of just clearing it. That needs
#     GOTRACEBACK=all; the default dumps one arbitrary goroutine and is useless here.
#   * re-adopt. The guest must be untouched across the trip: a watchdog that costs a guest reboot
#     on every fire is a cure worse than the disease (agent-readopt.nix proves the same for a
#     deliberate restart; this proves it for an involuntary one).
#
# Before any of that, READY=1 means the agent, not the node: under Type=notify `systemctl start`
# blocks until READY, so a start returning in seconds, long before the guest converges, is the
# assertion; gated on first healthy convergence, the unit would sit in `activating` past
# TimeoutStartSec and be killed. The same holds for the first start's disks: the data disk is
# made on ext2 (no fallocate), so the agent writes every zero, behind a write throttle that
# makes the write outlast TimeoutStartSec. Made before READY, the start dies at the timeout and
# the unit restarts into the same wait forever; made after READY, off the loop while the loop
# keeps beating (joinFill), the start returns in seconds and the write finishes under the
# watchdog untripped.
#
# Heavy (nested VM), so it rides the `integration` tag. Run:
#   nix build .#tests.agent-watchdog -L
{ pkgs, guestDisk, agent, netWrap, dressBase }:
pkgs.testers.runNixOSTest {
  name = "agent-watchdog";
  skipTypeCheck = true; # dynamic asserts

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 4096; # room for L1 + the nested 2G guest
      virtualisation.cores = 4;
      virtualisation.diskSize = 10240;
      virtualisation.emptyDiskImages = [ 3072 ]; # /dev/vdb: the slow disk step 1 provisions on
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ]; # expose vmx -> nested KVM
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl pkgs.iptables ];

      # The shipped watchdog shape: Type, NotifyAccess and WatchdogSec are the shipped unit
      # (scripts/units/briard-agent.service); TimeoutStartSec is 30 here against its 60, the bound
      # step 1 measures against; the rest mirrors agent-readopt's harness. WatchdogSec makes
      # WATCHDOG_USEC appear in the agent's environment, its only source for the ping interval, so
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
          TimeoutStartSec = 30; # bounds a config read, not a bring-up: READY precedes bringUp
          WatchdogSec = 20;
          ExecStart = "${agent}/bin/briard-agent run";
          Restart = "on-failure"; # covers a watchdog timeout, not only a non-zero exit
          RestartSec = 2;
          # Step 1's slow disk: the agent's own zero-write, held to a rate that takes a 2 GiB
          # volume well past TimeoutStartSec. The guest's qemu runs in its own unit, unthrottled.
          IOWriteBandwidthMax = "/dev/vdb 20M";
        };
        environment = {
          QEMU = "${pkgs.qemu}/bin/qemu-system-x86_64";
          ACCEL = "kvm:tcg";
          GUEST_IMAGE = "/tmp/guest.qcow2";
          # Where the host keeps its guest bundle tree, the way install.sh sets it.
          UPDATE_BASE = "/opt/briard/agent";
          DATA_DISK = "/slow/data.img"; # made by the agent at its first start (step 1)
          DATA_SIZE = "2G";
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
          VIP_DEV = "eth2";
          VIP_ADDR = "192.168.1.100/24";
          NET_MODE = "macvtap";
          NET_WRAP_BIN = "${netWrap}/bin/briard-net-wrap";
          STATUS_EVERY = "2s";
          GUEST_SERIAL = "/tmp/guest-serial.log";
          # Off on a shipped install; the soak sets it, and so do we — step 2 needs a real
          # telemetry path to hang, to prove that hanging it costs nothing.
          TELEMETRY_PATH = "/tmp/telemetry.json";
          # The wedge point (step 4): a fixture, set only here. The path does not exist yet, the
          # disarmed state: the agent opens it every cycle and gets ENOENT until step 4 mkfifos it.
          WEDGE_FIFO = "/tmp/wedge.fifo";
          # Step 3's catalog. This harness is hermetic (a shipped node reads the real one over the
          # WAN), so the fetch needs somewhere local, which step 3 makes unreachable on purpose.
          # Set here, not in the step, because the agent reads its configuration once, at start.
          CATALOG_URL = "http://127.0.0.1:8098/catalog/latest.json";
          # Without this the SIGABRT dump names one arbitrary goroutine instead of the wedged one.
          GOTRACEBACK = "all";
        };
      };
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # The shipped NIC contract (as agent-readopt): carrier-bearing parent, the guest's two LAN NICs
    # as macvtap children in install.sh's order (sys0 -> eth1, svc0 -> eth2), and the private
    # host<->guest link as a plain tap at 10.11.9.1/24. No macvlan shim, which would be the rig
    # granting itself reachability the product lacks: the VIP answers here because the agent routes
    # it over that link. A wedged agent cannot reconcile, so the route it installed stays across the
    # wedge steps, and a regression making the VIP assertions depend on a live reconcile would FAIL.
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )
    host.succeed("ln -s ${guestDisk}/nixos.qcow2 /tmp/guest.qcow2")
    # ext2 has no fallocate, so the agent's allocation is the zero-write fallback (platform.allocate).
    host.succeed("${pkgs.e2fsprogs}/bin/mkfs.ext2 -q /dev/vdb && mkdir -p /slow && mount -t ext2 /dev/vdb /slow")

    # === 1) READY means the agent, not the node ===
    # Under Type=notify this call blocks until READY=1, and must return in seconds, well before the
    # guest converges (minutes). Were READY gated on first healthy convergence, systemd would kill
    # this unit at TimeoutStartSec=30 instead.
    t0 = int(host.succeed("date +%s").strip())
    # The host holds a guest bundle tree, as install.sh lays on every install: the
    # image bakes no reverse proxy, so a guest is dressed by its host or it cannot serve. Copied out
    # of the store because the host writes `guest.good` beside the tree.
    host.succeed("mkdir -p /opt/briard/agent && cp -r ${dressBase}/. /opt/briard/agent/ && chmod -R u+w /opt/briard/agent")
    host.succeed("systemctl start briard-agent")
    t1 = int(host.succeed("date +%s").strip())
    started = t1 - t0
    print(f"systemctl start returned after {started}s (TimeoutStartSec=30)")
    assert started < 25, (
        f"start took {started}s — READY is arriving too late to be 'the agent started'; "
        "if it is gated on node health again, this unit dies at TimeoutStartSec"
    )
    # ...and it is genuinely ready before the node is healthy, which is the whole point.
    host.succeed("systemctl is-active briard-agent")
    host.fail("journalctl -u briard-agent | grep -q CONVERGED")
    # The watchdog is armed only once start-up completes, so this ordering is also what puts the
    # ladder under a watchdog at all; the agent logs it on the way past.
    host.succeed("journalctl -u briard-agent | grep -q 'systemd watchdog armed'")

    # ...and the first start's disk is made after READY, off the loop. data.img appears, whole, only
    # once the last zero is written and flushed, so that moment is the end of provisioning.
    host.wait_until_succeeds("test $(stat -c %s /slow/data.img) -eq 2147483648", timeout=600)
    provisioned = int(host.succeed("date +%s").strip()) - t0
    print(f"data disk provisioned {provisioned}s after the start began")
    # Non-vacuous first: the write has to outlast TimeoutStartSec, or a start that returned in
    # seconds would say nothing about where the write sits.
    assert provisioned > 35, (
        f"provisioning took {provisioned}s -- the throttle or the zero-write fallback is gone, so this "
        "step no longer outlasts TimeoutStartSec=30 and proves nothing about the order"
    )
    assert int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip()) == 0, (
        "the agent restarted during its first start's provisioning"
    )
    host.fail("journalctl -u briard-agent | grep -q 'Watchdog timeout'")

    # Now let it finish the job it was ready to do.
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
    host.succeed("systemctl is-active briard-guest.service")

    qemu_before = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    restarts_before = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    print(f"guest qemu pid {qemu_before}, agent restarts so far {restarts_before}")

    # Telemetry must be written before step 2 sabotages it, or step 2 would be a no-op this test
    # could not distinguish from a working fix.
    host.wait_until_succeeds("test -s /tmp/telemetry.json", timeout=30)

    # === 2) A hung telemetry path is not the agent's problem ===
    # os.WriteFile opens "<TELEMETRY_PATH>.tmp" O_WRONLY|O_CREATE|O_TRUNC; on a FIFO with no
    # reader that open(2) blocks forever, and it stands in for the realistic trigger — an NFS or
    # FUSE mount under TELEMETRY_PATH gone unresponsive. It must cost samples and nothing else.
    b87_since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()
    b87_restarts = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    host.succeed("rm -f /tmp/telemetry.json.tmp && mkfifo /tmp/telemetry.json.tmp")

    # Non-vacuous first: the writer really is stuck, not merely slow. Without this the step
    # would pass just as well against a FIFO nothing ever tried to open.
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{b87_since}' | grep -q 'telemetry write is not keeping up'",
        timeout=60,
    )
    # Well past WatchdogSec=20 with the write hung, the agent must not have restarted and must
    # still serve; with the write on the observe loop, the watchdog would fire here.
    host.succeed("sleep 45")
    b87_now = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    assert b87_now == b87_restarts, (
        f"a hung TELEMETRY_PATH took the agent down ({b87_restarts} -> {b87_now} restarts) — "
        "the telemetry write is back on the observe goroutine"
    )
    host.succeed("systemctl is-active briard-agent")
    host.succeed("curl -fsS http://192.168.1.100/healthz")
    # The writer goroutine stays parked in that open(2) for the life of this process (nothing short
    # of a reader frees it), so telemetry is dead until the restart in step 5 brings up a fresh one.
    # Removing the FIFO here only stops the next process re-wedging on it.
    host.succeed("rm -f /tmp/telemetry.json.tmp")

    # === 3) An honest long directive must not trip the watchdog ===
    # Step 2's question the other way round: a wedged agent must be caught, one doing work that
    # legitimately takes minutes must not be. The observe loop is the only thing that beats
    # systemd, by design (beat.go: a ticker on its own goroutine would report "alive" straight
    # through the wedge step 4 builds), so a directive holding the loop unleased would leave nothing
    # pinging, and anything past WatchdogSec=20 would end in SIGABRT instead of an outcome.
    # `app install` is an act: it runs off the loop (acts.go), which keeps beating, and leases its
    # budget besides (beat.go). The failure guarded against: an `app install` measured at 48.8 s
    # killed at `Watchdog timeout (limit 20s)!`, then SIGABRT and a CLI printing "the agent closed
    # the connection without reporting an outcome", as the admin socket dies with its process.
    #
    # The catalog is blackholed, not merely slow: the step needs the product verb on the real
    # dispatch path spending real time, deterministically, with no second moving part.
    # fetchManifest is the first call under the install budget in applyOneAppInstall, its client
    # has a 30 s timeout (shared/manifest/fetch.go), and a SYN into a DROP rule draws no RST, so the
    # window is 30 s, fixed by the code under test rather than a fixture sleep, and stands for a
    # real stalled channel. -I, not -A: NixOS's firewall accepts loopback early in its own chain,
    # so an appended rule would never see the packet. Nothing is mutated: the hang is upstream of
    # the ReactorActive guard and of every write, so a node failing here was never touched, which
    # is why this can sit mid-test, before the assertion that the guest was never rebooted.
    host.succeed("iptables -I INPUT 1 -p tcp --dport 8098 -j DROP")

    long_since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()
    long_restarts = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    long_pid = host.succeed("systemctl show -p MainPID --value briard-agent").strip()

    t_dir0 = int(host.succeed("date +%s").strip())
    # `briard-agent <verb>` is the CLI (a bare first argument is a subcommand, main.go), and the
    # admin socket is the default on both sides. Flags before the name: Go's flag package stops at
    # the first non-flag argument, so `install fixture -sock ...` would leave the flag positional.
    rc, out = host.execute("${agent}/bin/briard-agent app install fixture 2>&1")
    took = int(host.succeed("date +%s").strip()) - t_dir0
    print(f"app install returned rc={rc} after {took}s:\n{out}")

    # It really did reach dispatch -- the line an install opens with in the journal. Without
    # this, a CLI that failed to connect at all would look like a pass.
    host.succeed(
        f"journalctl -u briard-agent --since='{long_since}' | grep -q 'kind=app-install'"
    )
    # The defect. The journal is the witness to assert on first, the only race-free one: systemd
    # logs the trip at once, while the two counters below, read between the SIGABRT and
    # RestartSec=2, have not caught up yet (NRestarts can still read equal while MainPID is 0).
    # Counted rather than host.fail'd (the idiom elsewhere in this file) because this assertion
    # states the finding: a bare "command unexpectedly succeeded" names the grep, not the defect,
    # and a reader meeting it in CI should not have to open the file.
    trips = host.execute(
        f"journalctl -u briard-agent --since='{long_since}' | grep -c 'Watchdog timeout' || true"
    )[1].strip()
    assert trips == "0", (
        f"the watchdog fired {trips}x during a legitimate directive -- dispatch runs synchronously "
        f"on the observe loop, which is the only pinger, so nothing pinged systemd for the "
        f"{took}s the install took. The agent was SIGABRTed mid-work"
    )

    # ...and it is still the same process that took the directive. MainPID reads 0 while the unit
    # is down, which is what a mid-directive kill looks like caught inside RestartSec.
    long_pid_now = host.succeed("systemctl show -p MainPID --value briard-agent").strip()
    assert long_pid_now == long_pid, (
        f"the agent is no longer the process that took the directive ({long_pid} -> "
        f"{long_pid_now}; 0 means it is down and has not been restarted yet) -- it was killed "
        f"mid-install"
    )
    long_now = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    assert long_now == long_restarts, (
        f"the agent restarted across a {took}s directive ({long_restarts} -> {long_now}) -- "
        f"dispatch is running unleased under the watchdog"
    )

    # Non-vacuous: the assertion that must be able to fail, deliberately placed after survival. The
    # window has to outlast WatchdogSec or "the agent survived" says nothing; but a directive that
    # starves the watchdog dies exactly at the threshold, pinning `took` to ~20 s, so an up-front
    # guard would report a vacuous test in the one case where the honest answer is the defect. A
    # surviving agent spends the full 30 s fetch timeout, so it clears 25 s with room.
    assert took >= 25, (
        f"the directive returned in {took}s -- the catalog fetch is no longer spending its 30 s "
        f"timeout, so this step covers no unpinged window and the survival above proves nothing"
    )

    # And the operator got an answer. The install fails here (the catalog is unreachable by
    # construction) and must say so. A failed directive versus a failed agent is the whole finding:
    # a killed agent does not report the failed install, it leaves the operator holding a dead
    # socket, which reads as "did that work?" rather than "that did not work".
    assert rc != 0, f"the install reported success against an unreachable catalog:\n{out}"
    assert "without reporting an outcome" not in out, (
        f"the CLI lost the socket mid-directive -- the agent died under it:\n{out}"
    )

    host.succeed("iptables -D INPUT -p tcp --dport 8098 -j DROP")
    host.succeed("systemctl is-active briard-agent")
    host.succeed("curl -fsS http://192.168.1.100/healthz")
    print(f"a {took}s directive ran to a reported outcome with the watchdog armed")

    # The window steps 5 and 6 read for the trip. Taken here, after step 3, so a watchdog line
    # found below can only have come from the wedge.
    since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()

    # === 4) The wedge: alive, but the observe goroutine is gone ===
    # The explicit fixture. Same syscall, same loop, same position: the agent opens WEDGE_FIFO
    # each cycle and has been getting ENOENT until now.
    host.succeed("mkfifo /tmp/wedge.fifo")

    # The process must still be alive and running other goroutines: that separates this from
    # SIGSTOP, and is what a bare-timer pinger would sail straight through.
    host.wait_until_succeeds("test $(pgrep -cf 'bin/briard-agent') -ge 1", timeout=10)

    # === 5) The watchdog fires ===
    # WatchdogSec=20, so the miss lands within ~20s of the last beat; allow generous slack for a
    # loaded nested VM. NRestarts growing is the assertion systemd itself makes.
    host.wait_until_succeeds(
        f"test $(systemctl show -p NRestarts --value briard-agent) -gt {restarts_before}",
        timeout=120,
    )
    host.succeed(f"journalctl -u briard-agent --since='{since}' | grep -qi 'watchdog'")

    # === 6) And it left the diagnosis behind ===
    # SIGABRT + GOTRACEBACK=all: every goroutine's stack at the moment it wedged. This is the half
    # of the feature that closes the bug rather than clearing it.
    host.succeed(f"journalctl -u briard-agent --since='{since}' | grep -q 'SIGABRT'")
    # Deliberately loose, never pinning the goroutine-header format: journalctl prefixes every line,
    # so '^goroutine' can never match, and Go prints "goroutine 0 gp=0x.. m=1 mp=0x.. [idle]:"
    # rather than "goroutine 0 [idle]:". Runtime formatting is no business of this test; pinning it
    # would go red on a toolchain bump while the feature worked perfectly.
    dumped = int(host.succeed(
        f"journalctl -u briard-agent --since='{since}' | grep -cE 'goroutine [0-9]+' || true"
    ).strip())
    assert dumped >= 2, f"only {dumped} goroutine header(s) in the dump; expected a full traceback"

    # The assertion that carries the point, and the proof that GOTRACEBACK=all is load-bearing: the
    # signal lands on whichever goroutine happens to take it (goroutine 0, sysmon, parked in futex),
    # not the wedged one, so under the default (`single`) the dump would name that and stop, and
    # this frame could not appear. Its presence means the traceback reached the stuck goroutine and
    # named the exact call:
    #   os.OpenFile -> briard.io/agent/host.Config.wedgeForTest
    host.succeed(
        f"journalctl -u briard-agent --since='{since}' | grep -q 'host.Config.wedgeForTest'"
    )

    # === 7) The guest was never touched ===
    # An involuntary restart must re-adopt exactly as a deliberate one does.
    host.succeed("systemctl is-active briard-guest.service")
    qemu_mid = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    assert qemu_before == qemu_mid, (
        f"the watchdog cost the guest a reboot ({qemu_before} -> {qemu_mid}) — "
        "a cure worse than the disease"
    )

    # === 8) Clear the fault: the agent recovers, and stops tripping ===
    # Deliberately not "wait for another re-adopt": once the FIFO is gone the running agent simply
    # carries on, and demanding a restart asserts a recovery the product correctly does not perform.
    # Removing the FIFO does not unblock an open(2) already waiting on it, so the agent may still be
    # wedged and take one more watchdog cycle. The assertion below holds either way, which is why it
    # is the one worth making: a fresh telemetry file appears only once the observe loop reaches the
    # handoff again, by whichever route. It doubles as proof that the permanently parked writer of
    # step 2 did not outlive its process: only the writer the restart started can write this file.
    host.succeed("rm -f /tmp/wedge.fifo /tmp/telemetry.json")
    host.wait_until_succeeds("test -s /tmp/telemetry.json", timeout=180)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    host.succeed("systemctl is-active briard-agent")

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
