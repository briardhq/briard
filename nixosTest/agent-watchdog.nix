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
# is the real failure shape: open(2) on a FIFO with no reader blocks forever — an uninterruptible
# syscall, no deadline, no ctx to honour. Go hands the P off to another thread, so the runtime, the
# timers and every other goroutine keep running perfectly. A bare `time.Ticker` pinger would go on
# reporting liveness through the whole thing.
#
# THAT WEDGE USED TO BE THE PRODUCT'S OWN, and why it no longer is belongs here. writeTelemetry did
# an un-`ctx`'d os.WriteFile in the observe loop every cycle, so pointing TELEMETRY_PATH's ".tmp"
# sibling at a reader-less FIFO wedged the agent with no fault-injection hook at all. That was a
# real latent defect (B.87) and not a contrivance — telemetry, the least important thing in the
# loop, could take the node's supervisor down — and fixing it moved the write onto its own
# goroutine. Which took the lever away: the shape this test must produce is exactly the shape the
# fix makes unreachable. So the lever is now EXPLICIT, `BRIARD_WEDGE_FIFO`, opened at the same
# point in the same loop the write used to sit at. It is a fixture, it is named like one, nothing
# else sets it, and install.sh writes no such variable.
#
# The old lever is not gone though — it is now an ASSERTION (step 2). The same reader-less FIFO
# under TELEMETRY_PATH must NOT take the agent down any more, and under the pre-B.87 code that
# step fails by tripping the watchdog it exists to prove is not needed there. The defect this test
# was built on has become one of the things it defends.
#
# THAT IS THE MUTATION CHECK, and it is the point of the test: replace beat/lease with a timer
# goroutine and this test must FAIL (no restart, the wait_until_succeeds times out). A test that
# cannot fail against the design it rejects proves only that systemd works.
#
# AND IT PROVES THE OTHER DIRECTION, which is the one that shipped broken. A watchdog is only
# as good as its false-positive rate: an agent doing work that legitimately takes minutes must
# NOT be killed for it. `dispatch` runs synchronously on the observe loop and that loop is the
# only pinger, so every directive longer than WatchdogSec is a SIGABRT rather than an outcome --
# which is exactly what [V3b.15] found in the field, on a verb measured working two months
# earlier. Step 3 is that case, and it fails without the lease.
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
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl pkgs.iptables ];

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
          # Off on a shipped install; the soak sets it, and so do we — step 2 needs a real
          # telemetry path to hang, to prove that hanging it no longer costs anything.
          TELEMETRY_PATH = "/tmp/telemetry.json";
          # The wedge point (step 4). A fixture, and the only consumer of this variable anywhere.
          # The path does not exist yet, which is the disarmed state: the agent opens it every
          # cycle and gets ENOENT until the test mkfifos it.
          BRIARD_WEDGE_FIFO = "/tmp/wedge.fifo";
          # Step 3's catalog. A shipped node reads the real one over the WAN; this harness is
          # hermetic, so the fetch needs somewhere local to go -- and step 3 makes that somewhere
          # unreachable on purpose. Set here rather than in the step because the agent reads its
          # configuration once, at start.
          CATALOG_URL = "http://127.0.0.1:8098/catalog";
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

    # Prove telemetry is actually being written before we sabotage it, or step 2 would be a
    # no-op that this test could not distinguish from a working fix.
    host.wait_until_succeeds("test -s /tmp/telemetry.json", timeout=30)

    # === 2) B.87: A HUNG TELEMETRY PATH IS NO LONGER THE AGENT'S PROBLEM ===
    # This is the wedge this test used to USE, kept as the thing it now DEFENDS. os.WriteFile
    # opens "<TELEMETRY_PATH>.tmp" O_WRONLY|O_CREATE|O_TRUNC; on a FIFO with no reader that
    # open(2) blocks forever, and it stands in for the realistic trigger — an NFS or FUSE mount
    # under TELEMETRY_PATH gone unresponsive. It must now cost samples and nothing else.
    b87_since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()
    b87_restarts = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    host.succeed("rm -f /tmp/telemetry.json.tmp && mkfifo /tmp/telemetry.json.tmp")

    # Non-vacuous first: the writer really is stuck, not merely slow. Without this the step
    # would pass just as well against a FIFO nothing ever tried to open.
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{b87_since}' | grep -q 'telemetry write is not keeping up'",
        timeout=60,
    )
    # Now the assertion. Well past WatchdogSec=20 with the write hung, the agent must not have
    # been restarted and must still be serving. Pre-B.87 this is where the watchdog fired.
    host.succeed("sleep 45")
    b87_now = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    assert b87_now == b87_restarts, (
        f"a hung TELEMETRY_PATH took the agent down ({b87_restarts} -> {b87_now} restarts) — "
        "the telemetry write is back on the observe goroutine (B.87)"
    )
    host.succeed("systemctl is-active briard-agent")
    host.succeed("curl -fsS http://192.168.1.100/healthz")
    # The writer goroutine stays parked in that open(2) for the life of this process — nothing
    # short of a reader frees it — so telemetry is dead until the restart in step 5 brings up a
    # fresh one. Removing the FIFO here only stops the NEXT process re-wedging on it.
    host.succeed("rm -f /tmp/telemetry.json.tmp")

    # === 3) V3b.15: AN HONEST LONG DIRECTIVE MUST NOT TRIP THE WATCHDOG ===
    # Step 2's question asked the other way round, and this is the half that shipped broken.
    # A wedged agent must be caught; an agent doing work that legitimately takes minutes must not
    # be. `dispatch` is called synchronously on the observe loop (host.go), and that loop is the
    # ONLY thing that pings systemd -- by design, per beat.go: a ticker on its own goroutine would
    # report "alive" straight through the wedge step 4 builds. So while a directive runs, nothing
    # pings, and any directive longer than WatchdogSec=20 ends in SIGABRT instead of an outcome.
    #
    # NOT A RACE -- IT SHIPPED. `WatchdogSec=20` landed 2026-08-13 on a `service install` measured
    # at 48.8 s from the published channel a week earlier, and the next person to run the verb was
    # a stranger, seven days after that ([V3.29] -> [V3b.15]). Twenty-one seconds from `directive
    # kind=service-install submitted locally` to `Watchdog timeout (limit 20s)!`, then SIGABRT and
    # a CLI printing "the agent closed the connection without reporting an outcome" -- because the
    # admin socket died with the process holding it.
    #
    # WHY THE CATALOG IS BLACKHOLED RATHER THAN MERELY SLOW. What this step needs is the product's
    # own verb, on the real dispatch path, spending real time -- deterministically, with no second
    # moving part to go wrong. A DROPped port gives exactly that: fetchManifest is the FIRST thing
    # applyServiceInstall does, its client carries a 30 s timeout (shared/manifest/fetch.go), and a
    # SYN into a DROP rule draws no RST -- so the window is 30 s, fixed by the code under test
    # rather than by a fixture's sleep. -I, not -A: NixOS's firewall accepts loopback early in its
    # own chain, so an appended rule would never see the packet. It stands for something real, too
    # -- a stalled channel is a failure this product has already met (the 4 h stale-artifact
    # window, 2026-08-19).
    #
    # NOTHING IS MUTATED. The hang is upstream of the ReactorActive guard and of every write, so a
    # node that fails here is a node that was never touched -- which is why this can sit in the
    # middle of a test that goes on to assert the guest was never rebooted.
    host.succeed("iptables -I INPUT 1 -p tcp --dport 8098 -j DROP")

    long_since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()
    long_restarts = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    long_pid = host.succeed("systemctl show -p MainPID --value briard-agent").strip()

    t_dir0 = int(host.succeed("date +%s").strip())
    # `briard-agent <verb>` IS the CLI (a bare first argument is a subcommand, main.go), and the
    # admin socket is the default on both sides. Flags BEFORE the name: Go's flag package stops at
    # the first non-flag argument, so `install fixture -sock ...` would leave the flag positional.
    rc, out = host.execute("${agent}/bin/briard-agent service install fixture 2>&1")
    took = int(host.succeed("date +%s").strip()) - t_dir0
    print(f"service install returned rc={rc} after {took}s:\n{out}")

    # It really did reach dispatch -- the same line the stranger's journal opens with. Without
    # this, a CLI that failed to connect at all would look like a pass.
    host.succeed(
        f"journalctl -u briard-agent --since='{long_since}' | grep -q 'kind=service-install'"
    )
    # NON-VACUOUS, and this is the assertion that must be able to fail. If the call returned
    # inside the watchdog interval it never entered the unpinged window this step exists to test,
    # and everything below would pass against the defect.
    assert took >= 20, (
        f"the directive returned in {took}s, inside WatchdogSec=20 -- this step no longer "
        f"exercises an unpinged window, so the survival asserted below proves nothing"
    )

    # THE DEFECT. Pre-lease this is where it lands: the watchdog fires mid-directive, systemd
    # SIGABRTs the agent, and Restart=on-failure brings up a successor 2 s later.
    long_now = int(host.succeed("systemctl show -p NRestarts --value briard-agent").strip())
    assert long_now == long_restarts, (
        f"a {took}s directive took the agent down ({long_restarts} -> {long_now} restarts) -- "
        f"dispatch is running unleased under the watchdog (V3b.15)"
    )
    long_pid_now = host.succeed("systemctl show -p MainPID --value briard-agent").strip()
    assert long_pid_now == long_pid, (
        f"the agent process changed across the directive ({long_pid} -> {long_pid_now}) -- it was "
        f"restarted mid-install"
    )
    host.fail(f"journalctl -u briard-agent --since='{long_since}' | grep -qi 'watchdog timeout'")

    # AND THE OPERATOR GOT AN ANSWER. The install FAILS here -- the catalog is unreachable by
    # construction -- and it must say so. The difference between a failed DIRECTIVE and a failed
    # AGENT is the whole finding: the stranger was not told an install had failed, they were left
    # holding a dead socket, which reads as "did that work?" rather than "that did not work".
    assert rc != 0, f"the install reported success against an unreachable catalog:\n{out}"
    assert "without reporting an outcome" not in out, (
        f"the CLI lost the socket mid-directive -- the agent died under it (V3b.15):\n{out}"
    )

    host.succeed("iptables -D INPUT -p tcp --dport 8098 -j DROP")
    host.succeed("systemctl is-active briard-agent")
    host.succeed("curl -fsS http://192.168.1.100/healthz")
    print(f"a {took}s directive ran to a reported outcome with the watchdog armed")

    # The window steps 5 and 6 read for the trip. Taken here, AFTER step 3, so a watchdog line
    # found below can only have come from the wedge.
    since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()

    # === 4) THE WEDGE: alive, but the observe goroutine is gone ===
    # The explicit fixture. Same syscall, same loop, same position — the agent opens
    # BRIARD_WEDGE_FIFO each cycle and has been getting ENOENT until now.
    host.succeed("mkfifo /tmp/wedge.fifo")

    # The process must still be ALIVE and running other goroutines — that is what separates this
    # from SIGSTOP, and what a bare-timer pinger would sail straight through.
    host.wait_until_succeeds("test $(pgrep -cf 'bin/briard-agent') -ge 1", timeout=10)

    # === 5) THE WATCHDOG FIRES ===
    # WatchdogSec=20, so the miss lands within ~20s of the last beat; allow generous slack for a
    # loaded nested VM. NRestarts growing is the assertion systemd itself makes.
    host.wait_until_succeeds(
        f"test $(systemctl show -p NRestarts --value briard-agent) -gt {restarts_before}",
        timeout=120,
    )
    host.succeed(f"journalctl -u briard-agent --since='{since}' | grep -qi 'watchdog'")

    # === 6) AND IT LEFT THE DIAGNOSIS BEHIND ===
    # SIGABRT + GOTRACEBACK=all: every goroutine's stack at the moment it wedged. This is the half
    # of the feature that closes the bug rather than clearing it.
    host.succeed(f"journalctl -u briard-agent --since='{since}' | grep -q 'SIGABRT'")
    # Deliberately loose. Two earlier versions of this assertion pinned the goroutine-header
    # FORMAT and both were wrong -- journalctl prefixes every line, so '^goroutine' can never
    # match, and current Go prints "goroutine 0 gp=0x.. m=1 mp=0x.. [idle]:" rather than
    # "goroutine 0 [idle]:". Runtime formatting is not a property this test has any business
    # depending on; it would go red on a toolchain bump while the feature worked perfectly.
    dumped = int(host.succeed(
        f"journalctl -u briard-agent --since='{since}' | grep -cE 'goroutine [0-9]+' || true"
    ).strip())
    assert dumped >= 2, f"only {dumped} goroutine header(s) in the dump; expected a full traceback"

    # THE ASSERTION THAT CARRIES THE POINT, and it doubles as the proof that GOTRACEBACK=all is
    # load-bearing rather than decorative. The signal lands on whichever goroutine happens to take
    # it -- goroutine 0, sysmon, parked in futex -- NOT on the wedged one. So under the default
    # (`single`) the dump would name that and stop, and this frame could not appear. Its presence
    # means the traceback reached the goroutine that is actually stuck, and named the exact call:
    #   os.OpenFile -> briard.io/agent/host.Config.wedgeForTest
    # (before B.87 this frame read host.Config.writeTelemetry, which is the whole story of this
    # test in one line: the same assertion, now naming a fixture instead of a defect.)
    host.succeed(
        f"journalctl -u briard-agent --since='{since}' | grep -q 'host.Config.wedgeForTest'"
    )

    # === 7) THE GUEST WAS NEVER TOUCHED ===
    # An involuntary restart must re-adopt exactly as a deliberate one does.
    host.succeed("systemctl is-active briard-guest.service")
    qemu_mid = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    assert qemu_before == qemu_mid, (
        f"the watchdog cost the guest a reboot ({qemu_before} -> {qemu_mid}) — "
        "a cure worse than the disease"
    )

    # === 8) CLEAR THE FAULT: the agent recovers, and stops tripping ===
    # Deliberately NOT "wait for another re-adopt". Once the FIFO is gone the agent that is
    # already running simply carries on; no further restart is needed, and demanding one asserts
    # a recovery the product correctly does not perform. (An earlier version did exactly that and
    # timed out — on behaviour that was right.)
    #
    # Removing the FIFO also does not unblock an open(2) already waiting on it, so the agent may
    # still be wedged here and take one more watchdog cycle. The assertion below is true either
    # way, which is why it is the one worth making: a FRESH telemetry file can only appear if the
    # observe loop reached the handoff again, whichever route it took to get there. It doubles as
    # the proof that step 2's permanently-parked writer did not outlive its process — this file
    # can only be written by the writer the restart started.
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
