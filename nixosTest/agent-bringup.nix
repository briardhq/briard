# The real product `agent` (host mode) boots the real guest disk under nested QEMU, drives
# bring-up over the virtio-serial channel to a converged lone node (no DRBD; it reports quorate
# primary) and holds it in its observe loop: the integration test the net.Pipe unit tests stand
# in for, proving in the agent the host path the driver also exercises. Also covers reconnect: a
# frozen guest drops the channel and the agent re-dials and resyncs past the stale in-flight
# reply rather than going blind. Heavy (a nested VM + a multi-GB guest disk), so it rides the
# `integration` tag; `nix flake check` boots no VM tests. Run:
#   nix build .#tests.agent-bringup -L   (or the whole tag: nix build .#integration)
{ pkgs, guestDisk, agent, netWrap, dressBase }:
pkgs.testers.runNixOSTest {
  name = "agent-bringup";
  skipTypeCheck = true; # systemd-run + backgrounded driver, dynamic asserts

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 4096; # room for L1 + the nested 2G guest
      virtualisation.cores = 4;
      virtualisation.diskSize = 10240; # the data + state disks live here
      virtualisation.vlans = [ ]; # no framework eth1 -- we build our own macvtap parent (192.168.1.0/24, the VIP subnet)
      virtualisation.qemu.options = [ "-cpu" "host" ]; # expose vmx -> nested KVM in L1
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl ];
      # The household's zone, which the agent reads off this host and pushes into its guest at
      # bring-up (sys.timezone). Tokyo: +9 with no DST, so the guest cannot match it by accident.
      time.timeZone = "Asia/Tokyo";
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")  # nested KVM present in L1

    # The shipped NIC contract, which the rig matches rather than improves on: a carrier-bearing
    # parent (a veth whose peer is up; a dummy has no carrier and macvlan bridge-mode won't
    # forward), the guest's two LAN NICs as macvtap children of it in the agent's order (sys0 ->
    # eth1, the node IP and DRBD's NIC on a flock; svc0 -> eth2, the VIP), and the private link as
    # a plain tap holding 10.11.9.1/24. All three or none: qemu.go assigns NICs positionally, so
    # without sys0 the witness NIC lands on eth2 while the agent addresses eth3 (10.11.9.2), and
    # the private link silently fails to exist.
    #
    # ⚠️ No host-side macvlan shim: macvtap isolates host<->guest, and a shim giving L1 the VIP
    # would grant the rig a reachability the product does not have, so no test could fail on the
    # gap. The curl passes because the agent routes the VIP over the private link.
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )

    # The shipped image, which the agent attaches read-only, + a blank data disk.
    host.succeed("ln -s ${guestDisk}/nixos.qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    # The product agent (host mode = plain `run`) boots the guest, drives the ordered bring-up
    # (data -> VIP, no app: the shipped disk runs none) and stays in its observe loop, so
    # systemd-run stays up. Same env names as the driver, read through ConfigFromEnv.
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
        "--setenv=CONTROL_SOCK=/run/briard-ctl.sock --setenv=NODE=guest --setenv=GUEST_SERIAL=/tmp/guest-console.log "
        # The three taps of every shipped node, in the agent's order (SYSTEM_TAP -> eth1,
        # SERVICE_TAP -> eth2, WITNESS_TAP -> eth3, the private link), named by the rig in place
        # of the defaults. eth1 carries the NODE IP (as on a shipped single node), the one address
        # anything reaches the node at; SYSTEM_HOST_CIDR is the host end of that subnet, on the
        # tap. A shipped agent draws these addresses itself (applyDraw); the rig pins them so the
        # addresses here are known.
        "--setenv=SYSTEM_TAP=sys0 --setenv=SYSTEM_DEV=eth1 --setenv=SYSTEM_CIDR=10.0.0.1/24 --setenv=SYSTEM_HOST_CIDR=10.0.0.129/32 --setenv=WITNESS_CIDR=10.11.9.2/24 --setenv=SERVICE_TAP=svc0 --setenv=WITNESS_TAP=briard-priv0 "
        "--setenv=STATUS_EVERY=2s "
        # The test declares the address it curls: the image bakes none, and unset means DHCP,
        # which nothing answers here. HEALTH_URL is deliberately unset, so the agent resolves its
        # probe target from the address the guest holds: the shipped path, not a baked URL.
        "--setenv=VIP_DEV=eth2 --setenv=VIP_ADDR=192.168.1.100/24 "
        # Macvtap substrate: qemu launches behind the fd-passing wrapper, which pins each macvtap
        # MAC to the derived one (matching qemu mac=) and opens /dev/tap<ifindex>. The witness tap
        # does not go through the wrapper: qemu opens it by name, as in production.
        "--setenv=NET_MODE=macvtap --setenv=NET_WRAP_BIN=${netWrap}/bin/briard-net-wrap "
        "${agent}/bin/briard-agent run"
    )

    # The console is the window, for bring-up too: it is the first thing that can fail, and its
    # cause is entirely in the guest (a chain member that would not start, a device the resource
    # names and the guest lacks). Without the dump the only evidence is the agent one-liner "a
    # dependency job for briard-chain.target failed", which names no dependency.
    try:
        # The agent prints CONVERGED once bring-up completes.
        host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)

        # The point: L1 reaches the reverse proxy at the agent-claimed VIP. On the shipped disk,
        # what answers is the node itself, not a workload baked in for the test.
        host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
    except Exception:
        # Failure lines first, then the tail: once the host agent has died the guest agent
        # restarts every second forever, and 250 lines of that scroll the cause off the end.
        # `grep -m`, not `| head`, so the producer is not SIGPIPE'd under the pipefail shell.
        print("=== guest console (failures) ===")
        print(host.succeed("tr -d '\\r' < /tmp/guest-console.log "
                           "| grep -m 200 -iE 'fail|error|refus|dependency|drbd-r0' || true"))
        print("=== guest console (tail) ===")
        print(host.succeed("tr -d '\\r' < /tmp/guest-console.log | tail -80 || true"))
        raise
    # ...the way a real host does: over the private link, on the agent route. Asserted, not
    # inferred from the curl, because a rig-built shim would make the curl pass too.
    route = host.succeed("ip route get 192.168.1.100")
    print(f"host route to the VIP: {route.strip()}")
    assert "briard-priv0" in route and "10.0.0.1" in route, (
        f"the host reaches the VIP some other way than the agent's route over the private link: "
        f"{route!r} -- if a macvlan shim has come back, this test has stopped proving the product"
    )
    # Beyond the driver: the observe loop reports the converged NodeStatus, quorate primary and
    # healthy. Failable: health follows the real probe, true only once the reverse proxy serves.
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'status node=guest role=anchor primary=true quorate=true connected=0 healthy=true'",
        timeout=60,
    )

    # --- the host recovers a dropped control channel ---
    # A SIGSTOPped guest QEMU leaves control calls unanswered; three misses within a minute close
    # the channel (the liveness rule, ErrChannelDown). Without the re-dial that follows, the host
    # would be blind to the guest forever.
    qemu = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    # Counted, not grepped: bring-up already logged one reconnect (the dress restarts the guest
    # agent), so the line existing proves nothing.
    reconnects = int(host.succeed("journalctl -u briard-agent | grep -c 'control channel reconnected' || true").strip())
    host.succeed(f"kill -STOP {qemu}")
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'control channel down'", timeout=60)

    # On thaw the guest flushes the reply it was frozen mid-computing (a stale frame from the
    # dropped session) ahead of the handshake reply, as QEMU keeps the port open across the
    # reconnect, so the handshake must resync past it, not fail on the id mismatch. Proven by the
    # reconnect log, the VIP serving again, and a fresh healthy status line.
    host.succeed(f"kill -CONT {qemu}")
    thawed = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()
    try:
        host.wait_until_succeeds(f"test $(journalctl -u briard-agent | grep -c 'control channel reconnected') -gt {reconnects}", timeout=120)
        host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
        # What the thaw cost: when the host was back on the channel, and the guest agent journal.
        print("=== host agent across the thaw ===")
        print(host.succeed(f"journalctl -u briard-agent -o short-precise --since='{thawed}' | grep -vE 'status node=' | head -40 || true"))
        print("=== guest agent across the thaw ===")
        host.succeed(f"(printf '\\n'; sleep 2; printf 'journalctl -u briard-guest-agent -o short-precise --since=\"{thawed}\" --no-pager | grep -v Consumed | tail -60\\n'; sleep 6; printf '\\035') | briard-agent debug shell > /tmp/thaw.out 2>&1")
        print(host.succeed("tr -d '\\r' < /tmp/thaw.out"))
        # One attempt, or the phases are colliding. The guest agent restarts on the EOF of the
        # freeze, losing a hello written in that gap; the reconnect resends it on the same
        # connection (every 5 s for up to 30 s, host.go). Re-dialling instead gives the fresh agent
        # another EOF, and a thaw takes four to six attempts (~100 s).
        failed = int(host.succeed(f"journalctl -u briard-agent --since='{thawed}' | grep -c 'reconnect attempt' || true").strip())
        assert failed == 0, f"the thaw took {failed + 1} handshake attempts; the window is shorter than a guest agent restart"
    except Exception:
        print("=== host agent since the freeze ===")
        print(host.succeed("journalctl -u briard-agent -o cat | grep -vE 'status node=' | tail -40 || true"))
        print("=== guest console (tail) ===")
        print(host.succeed("tr -d '\\r' < /tmp/guest-console.log | tail -120 || true"))
        raise
    since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since}' | grep -q 'healthy=true'",
        timeout=60,
    )
    print("host recovered a dropped control channel (re-dial + resync)")

    # --- the debug console: closed, opened deliberately, closed again ---
    # Only here can it be asserted that ttyS1 is a root shell in the guest and a normal node does
    # not carry an open one: unit tests prove the argv, a bare QEMU the chardev move, but only a
    # booted product image the getty on the port and an autologin that lands in a shell.
    # Stdin is a pipe, the degraded path the CLI is written for (no terminal, so no raw mode, just
    # relay). The trailing \035 is Ctrl-], the escape: an autologin getty never hangs up itself.
    host.fail("test -e /run/briard/qmp/console.sock")  # a launch must leave the guest closed
    host.succeed(
        "(printf '\\n'; sleep 2; printf 'id\\n'; sleep 2; "
        "printf 'journalctl -b -u systemd-journald -o cat --no-pager | grep -i \"system journal\"\\n'; sleep 5; "
        "printf 'readlink /etc/localtime; date +%%z\\n'; sleep 2; printf '\\035') "
        "| briard-agent debug shell > /tmp/debug-shell.out 2>&1"
    )
    shell_out = host.succeed("cat /tmp/debug-shell.out")
    print(shell_out)
    assert "uid=0(root)" in shell_out, f"no root shell on ttyS1:\n{shell_out}"
    # The guest journal is bounded: journald states the limit it applied when it opened the
    # persistent journal on the state disk ("System Journal (...) is 8.0M, max 128M, ..."; whole
    # numbers print with no decimal, like the default "max 4G"). The typed command never contains
    # the figure.
    assert "max 128M," in shell_out, f"the guest's persistent journal is not capped at 128 MB:\n{shell_out}"
    # The household zone, pushed at bring-up: the guest link and clock say Tokyo. Neither string
    # was typed, and the image sets no zone, so this also proves /etc/localtime is settable at
    # runtime.
    assert "/etc/zoneinfo/Asia/Tokyo" in shell_out, f"the guest's /etc/localtime is not the host's zone:\n{shell_out}"
    assert "+0900" in shell_out, f"the guest's clock does not read the host's zone:\n{shell_out}"
    host.succeed("journalctl -u briard-agent | grep -q 'timezone: the guest runs on Asia/Tokyo'")
    # Closed again on the way out; the socket is the only record, so if it survives the verb a
    # node stays open after a support call.
    host.fail("test -e /run/briard/qmp/console.sock")
    # Scoped to the agent unit: arm and disarm are directives it applies, recorded in its log like
    # every other act on this node. A CLI auditing from outside would satisfy an unscoped
    # `journalctl` just as well.
    host.succeed("journalctl -u briard-agent | grep -q 'debug console ARMED'")
    host.succeed("journalctl -u briard-agent | grep -q 'debug console disarmed'")
    print("debug console: closed -> root shell -> closed")
    print(host.succeed("journalctl -u briard-agent | tail -30"))
  '';
}
