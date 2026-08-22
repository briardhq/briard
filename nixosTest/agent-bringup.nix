# The real product `agent` (host mode) boots the real guest disk under
# NESTED QEMU and drives DRBD bring-up over the virtio-serial channel, converging to
# quorate Primary, then holds it in its observe loop. This is the integration test the
# net.Pipe unit tests stand in for, and the end-to-end proof that the host path
# promoted out of the driver actually works. Also covers reconnect: a frozen
# guest drops the channel and the agent re-dials + resyncs past the stale in-flight reply
# rather than going blind. Heavy (a nested VM + a multi-GB guest disk), so it rides the
# `integration` tag; `nix flake check` boots no VM tests. Run:
#   nix build .#tests.agent-bringup -L   (or the whole tag: nix build .#integration)
{ pkgs, guestDisk, agent, netWrap }:
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

    # The service L2 on the MACVTAP substrate (default): a carrier-bearing parent (a veth whose
    # peer is up -- a dummy has no carrier and macvlan bridge-mode won't forward), the guest's service
    # NIC as a macvtap CHILD of it (svc0 -> the guest's eth1, the VIP), and a host-side macvlan SHIM on
    # the same parent so L1 can still reach the guest VIP -- macvtap deliberately isolates host<->guest,
    # so without the shim L1 could not curl 192.168.1.100 (proven in the evaluation). shim0 and svc0
    # are bridge-mode siblings, so they forward internally (no external switch needed).
    #
    # ⚠️ THE SHIM IS RIG PLUMBING AND NOT THE PRODUCT'S ANSWER to that isolation, and the
    # distinction is worth stating here because this rig is where it was easiest to lose: a real
    # install carries the private host<->guest link and the agent routes the VIP over it
    # ([V3b.19]), which is what lets the machine running the guest reach it. This test sets no
    # WITNESS_TAP and keeps the legacy positional layout (one service tap -> the guest's eth1), so
    # there is no such link and no route to have; L1 stands in for the rest of the LAN. The
    # shipped shape -- uniform NIC layout, private link, route -- is proved in install-macvtap.
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name shim0 type macvlan mode bridge && ip addr add 192.168.1.1/24 dev shim0 && ip link set shim0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up"
    )

    # Writable overlay of the read-only store qcow2 + a blank DRBD backing disk.
    host.succeed("qemu-img create -f qcow2 -b ${guestDisk}/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    # Run the product agent (host mode = no --guest): boots the guest, drives the
    # ordered bring-up (data -> VIP, no payload -- the shipped disk runs none), then holds it in
    # the observe loop so systemd-run stays up. Same env contract the driver used (ConfigFromEnv).
    host.succeed(
        "systemd-run --unit=briard-agent --collect "
        "--setenv=QEMU=${pkgs.qemu}/bin/qemu-system-x86_64 --setenv=ACCEL=kvm:tcg "
        "--setenv=GUEST_DISK=/tmp/guest.qcow2 --setenv=DATA_DISK=/tmp/data.img "
        "--setenv=CONTROL_SOCK=/run/briard-ctl.sock --setenv=NODE=guest "
        "--setenv=SERVICE_TAP=svc0 --setenv=STATUS_EVERY=2s "
        # The test DECLARES the service address it is about to curl. The guest image bakes none
        # (V3.19c step 3) and unset means DHCP, which nothing answers on a nixosTest's L2.
        # HEALTH_URL is deliberately left unset alongside it, so the agent resolves its probe
        # target from the address the guest actually holds -- the shipped path, exercised here
        # rather than bypassed by a baked URL that happens to agree.
        "--setenv=VIP_DEV=eth1 --setenv=VIP_ADDR=192.168.1.100/24 "
        # Macvtap substrate: the agent launches qemu behind the fd-passing wrapper, which pins
        # svc0's MAC to the agent's derived svc MAC (matching qemu's mac=) and opens /dev/tap<ifindex>.
        "--setenv=NET_MODE=macvtap --setenv=NET_WRAP_BIN=${netWrap}/bin/briard-net-wrap "
        "${agent}/bin/briard-agent"
    )

    # The agent prints CONVERGED once the guest reports quorate Primary.
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)

    # The whole point: L1 reaches Briard's front door at the agent-claimed VIP. This boots the
    # SHIPPED disk, so what answers here is the node itself, not a workload baked in for the test.
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
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
    print(host.succeed("journalctl -u briard-agent | tail -30"))
  '';
}
