# The real product `agent` (host mode) boots the real guest disk under
# NESTED QEMU and drives DRBD bring-up over the virtio-serial channel, converging to
# quorate Primary, then holds it in its observe loop. This is the integration test the
# net.Pipe unit tests stand in for, and the end-to-end proof that the host path
# promoted out of the driver actually works. Also covers reconnect: a frozen
# guest drops the channel and the agent re-dials + resyncs past the stale in-flight reply
# rather than going blind. Heavy (a nested VM + a multi-GB guest disk), so it rides the
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
      virtualisation.diskSize = 10240; # the overlay + data disk live here
      virtualisation.vlans = [ ]; # no framework eth1 -- we build our own macvtap parent (192.168.1.0/24, the VIP subnet)
      virtualisation.qemu.options = [ "-cpu" "host" ]; # expose vmx -> nested KVM in L1
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl ];
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")  # nested KVM present in L1

    # THE SHIPPED NIC CONTRACT, and it is the rig's job to match it rather than improve on it
    # ([V3b.19a]). A carrier-bearing parent (a veth whose peer is up -- a dummy has no carrier and
    # macvlan bridge-mode won't forward), then the guest's two LAN NICs as macvtap CHILDREN of it in
    # install.sh's order -- sys0 -> the guest's eth1 (the DRBD NIC, idle on one node), svc0 -> eth2
    # (the VIP) -- and the private host<->guest link as a plain tap holding 10.11.9.1/24, exactly as
    # install.sh lays it down. qemu.go assigns NICs positionally, so all three or none: omit sys0 and
    # the witness NIC lands on eth2, where the guest's baked 10.11.9.2 is not, and the private link
    # silently fails to exist.
    #
    # ⚠️ THIS USED TO BUILD A HOST-SIDE MACVLAN SHIM INSTEAD (shim0, 192.168.1.1/24), because macvtap
    # isolates host<->guest and without something L1 could not curl the VIP at all. That shim was the
    # rig granting itself a reachability THE PRODUCT DID NOT HAVE -- so no test could fail on the gap,
    # and a stranger found it instead of us ([V3b.19]). The curl below is unchanged and now passes for
    # the shipped reason: the agent routes the VIP over the private link. A shim here would hide that
    # working or broken, which is the whole argument for deleting it.
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )

    # Writable overlay of the read-only store qcow2 + a blank DRBD backing disk.
    host.succeed("qemu-img create -f qcow2 -b ${guestDisk}/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    # Run the product agent (host mode = plain `run`): boots the guest, drives the
    # ordered bring-up (data -> VIP, no service -- the shipped disk runs none), then holds it in
    # the observe loop so systemd-run stays up. Same env contract the driver used (ConfigFromEnv).
    # The host holds a guest bundle tree, as install.sh lays on every install ([B.138]): the image
    # bakes no door, so a guest is dressed by its host or it cannot serve. Copied out of the store
    # because the host writes `guest.good` beside the tree.
    host.succeed("mkdir -p /opt/briard/agent && cp -r ${dressBase}/. /opt/briard/agent/ && chmod -R u+w /opt/briard/agent")

    host.succeed(
        "systemd-run --unit=briard-agent --collect "
        # The PATH install.sh gives the shipped unit (scripts/install.sh, "Environment=PATH="). The
        # agent shells out to systemd-run, systemctl and -- since [V3b.19] -- `ip`, all BY NAME, and
        # a transient unit's default PATH resolves none of them reliably. Pinning the shipped value
        # is the point: the rig gets what the product gets ([V3b.19a]).
        "--setenv=PATH=/usr/sbin:/usr/bin:/sbin:/bin:/run/current-system/sw/bin:/run/wrappers/bin "
        "--setenv=QEMU=${pkgs.qemu}/bin/qemu-system-x86_64 --setenv=ACCEL=kvm:tcg "
        # Where the host keeps its guest bundle tree ([B.138]), the way install.sh sets it.
        "--setenv=UPDATE_BASE=/opt/briard/agent "
        "--setenv=GUEST_DISK=/tmp/guest.qcow2 --setenv=DATA_DISK=/tmp/data.img "
        "--setenv=CONTROL_SOCK=/run/briard-ctl.sock --setenv=NODE=guest --setenv=GUEST_SERIAL=/tmp/guest-console.log "
        # The three taps install.sh sets on every install, in its order: SYSTEM_TAP -> eth1,
        # SERVICE_TAP -> eth2, WITNESS_TAP -> eth3 (the private link). SYSTEM_DEV/SYSTEM_CIDR are
        # set, as they now are on a shipped single node too: eth1 carries this node's NODE IP, the
        # one address anything uses to reach it, and the gate below answers there ([V3b.26b]).
        # SYSTEM_HOST_CIDR is the host's own end of that subnet, on the tap -- the rig states all
        # three because it is standing in for install.sh, which sets them together or not at all.
        "--setenv=SYSTEM_TAP=sys0 --setenv=SYSTEM_DEV=eth1 --setenv=SYSTEM_CIDR=10.0.0.1/24 --setenv=SYSTEM_HOST_CIDR=10.0.0.129/32 --setenv=WITNESS_CIDR=10.11.9.2/24 --setenv=SERVICE_TAP=svc0 --setenv=WITNESS_TAP=briard-priv0 "
        "--setenv=STATUS_EVERY=2s "
        # The test DECLARES the service address it is about to curl. The guest image bakes none
        # (V3.19c step 3) and unset means DHCP, which nothing answers on a nixosTest's L2.
        # HEALTH_URL is deliberately left unset alongside it, so the agent resolves its probe
        # target from the address the guest actually holds -- the shipped path, exercised here
        # rather than bypassed by a baked URL that happens to agree.
        "--setenv=VIP_DEV=eth2 --setenv=VIP_ADDR=192.168.1.100/24 "
        # Macvtap substrate: the agent launches qemu behind the fd-passing wrapper, which pins each
        # macvtap's MAC to the agent's derived MAC (matching qemu's mac=) and opens /dev/tap<ifindex>.
        # The witness tap is NOT passed through the wrapper -- qemu opens it by name, as in production.
        "--setenv=NET_MODE=macvtap --setenv=NET_WRAP_BIN=${netWrap}/bin/briard-net-wrap "
        "${agent}/bin/briard-agent run"
    )

    # THE CONSOLE IS THE WINDOW ([[guest-console-is-the-window]]), and it covers BRING-UP too --
    # not just the front door. Bring-up is the first thing that can fail and the one whose cause is
    # entirely inside the guest: a chain member that would not start, a device the resource names
    # and the guest does not have. Without the dump here, the only evidence is the agent's one-line
    # "a dependency job for drbd@r0.target failed", which names no dependency.
    try:
        # The agent prints CONVERGED once the guest reports quorate Primary.
        host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)

        # The whole point: L1 reaches Briard's front door at the agent-claimed VIP. This boots the
        # SHIPPED disk, so what answers here is the node itself, not a workload baked in for the test.
        host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
    except Exception:
        # The FAILURE LINES FIRST, then the tail. A tail alone is not enough once the host agent
        # has died: the guest agent then restarts every second forever, and 250 lines of that
        # scroll the actual cause off the end (measured, chasing a bring-up failure whose reason
        # sat 600 lines above the tail). `grep -m` rather than `| head`, so the producer is not
        # SIGPIPE'd into a non-zero status under the driver's pipefail shell.
        print("=== guest console (failures) ===")
        print(host.succeed("tr -d '\\r' < /tmp/guest-console.log "
                           "| grep -m 200 -iE 'fail|error|refus|dependency|drbd-r0' || true"))
        print("=== guest console (tail) ===")
        print(host.succeed("tr -d '\\r' < /tmp/guest-console.log | tail -80 || true"))
        raise
    # ...and it reaches it THE WAY A REAL HOST DOES: over the private link, on a route the agent
    # put there. Asserted rather than inferred from the curl, because the curl passing is what a
    # rig-built shim used to buy too -- this is the line that tells the two apart ([V3b.19a]).
    route = host.succeed("ip route get 192.168.1.100")
    print(f"host route to the VIP: {route.strip()}")
    assert "briard-priv0" in route and "10.0.0.1" in route, (
        f"the host reaches the VIP some other way than the agent's route over the private link: "
        f"{route!r} -- if a macvlan shim has come back, this test has stopped proving the product"
    )
    # The piece beyond the driver: the standing observe loop reports the *correct*
    # converged NodeStatus -- quorate primary AND healthy (a failable assertion:
    # health follows the real probe, so it only flips true once the front door actually serves).
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'status node=guest role=anchor primary=true quorate=true connected=0 healthy=true'",
        timeout=60,
    )

    # --- the host recovers a dropped control channel ---
    # Freeze the nested guest (SIGSTOP the guest QEMU) so the host's next control call
    # blocks and hits its 5 s deadline; the wire watcher then closes the channel
    # (ErrChannelDown) -- the event that, older, blinded the host to the guest
    # *forever* (it dialed once and observe never re-dialed).
    qemu = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    host.succeed(f"kill -STOP {qemu}")
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'control channel down'", timeout=60)

    # Thaw it: the reconnect loop re-dials and re-handshakes. On thaw the guest flushes the
    # reply it was frozen mid-computing (a stale frame from the dropped session) ahead of
    # the handshake reply, since QEMU keeps the guest port open across the reconnect -- so
    # the handshake must *resync* past it, not fail on the id mismatch. Proven by the
    # reconnect log, the VIP serving again, and a fresh healthy status line after the thaw.
    host.succeed(f"kill -CONT {qemu}")
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'control channel reconnected'", timeout=120)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
    since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since}' | grep -q 'healthy=true'",
        timeout=60,
    )
    print("host recovered a dropped control channel (re-dial + resync)")

    # --- the debug console: closed, opened deliberately, closed again ---
    # The one assertion that cannot be made anywhere else: that ttyS1 really is a root shell in
    # the guest, and that a normal node is not carrying an open one. Unit tests can prove the
    # argv and a bare QEMU can prove the chardev moves, but only a booted product image proves
    # the getty bound to the port and that autologin lands somewhere with a shell.
    #
    # Driven with stdin as a PIPE, which is also the degraded path the CLI is written for: no
    # terminal, so it skips raw mode and just relays. The trailing \035 is Ctrl-] -- the escape,
    # because the far end is an autologin getty that never hangs up on its own.
    host.fail("test -e /run/briard/qmp/console.sock")  # a launch must leave the guest closed
    host.succeed(
        "(printf '\\n'; sleep 2; printf 'id\\n'; sleep 5; printf '\\035') "
        "| briard-agent debug shell > /tmp/debug-shell.out 2>&1"
    )
    shell_out = host.succeed("cat /tmp/debug-shell.out")
    print(shell_out)
    assert "uid=0(root)" in shell_out, f"no root shell on ttyS1:\n{shell_out}"
    # Closed again on the way out, and the socket is the evidence: nothing else records the
    # state, so if this file survives the verb, a node stays open after a support call.
    host.fail("test -e /run/briard/qmp/console.sock")
    host.succeed("journalctl | grep -q 'debug console ARMED'")
    host.succeed("journalctl | grep -q 'debug console disarmed'")
    print("debug console: closed -> root shell -> closed")
    print(host.succeed("journalctl -u briard-agent | tail -30"))
  '';
}
