# The host-agent deadman on a LONE node REBOOTS — and the reboot surfaces to the supervisor.
#
# This test used to assert the opposite (a lone node holds, keeps serving, never reboots) because
# the gate derived the single-node case from the majority formula: departing loses quorum, so hold.
# True, and irrelevant — there is no peer to outage, quorum is guaranteed the moment it comes back,
# and the data is on its own disk. What holding actually bought was a reflex that did nothing at
# all on the shape most briard installs have: the node alerted once and then sat wedged and silent
# indefinitely. V3.31 made single-node its own clause in deadman.RebootAllowed.
#
# Single-node harness (like agent-readopt): the guest is DRBD single-node, so it exercises exactly
# that clause. We kill the host agent for well past T_deadman (baked short via BRIARD_DEADMAN in
# the guest image) and prove three things:
#
#   1. the in-guest deadman evaluated and chose to REBOOT (its log rides the guest serial),
#   2. QEMU EXITED rather than resetting inside itself — `-no-reboot` is what makes a guest restart
#      an event the supervisor can see, instead of a gap in the control channel indistinguishable
#      from every other gap,
#   3. the host agent, on returning, brings the guest back.
#
# (2) is the assertion most worth having. A guest that reset in place would leave the same QEMU pid
# running, the VIP would blip, and every other check here would still pass — while the agent had
# silently stopped being able to observe or count its charge's restarts.
#
# Heavy (nested VM) → the `integration` tag. Run on the self-hosted L0:
#   gh workflow run vm-test.yml -f test=agent-deadman
{ pkgs, guestDisk, agent, netWrap }:
pkgs.testers.runNixOSTest {
  name = "agent-deadman";
  skipTypeCheck = true; # dynamic asserts, backgrounded poller

  nodes.host =
    { ... }:
    {
      virtualisation.memorySize = 4096;
      virtualisation.cores = 4;
      virtualisation.diskSize = 10240;
      virtualisation.vlans = [ ];
      virtualisation.qemu.options = [ "-cpu" "host" ]; # nested KVM
      environment.systemPackages = [ pkgs.qemu agent pkgs.iproute2 pkgs.curl ];

      systemd.services.briard-agent = {
        description = "Briard host agent (deadman proof)";
        wantedBy = [ ];
        path = [ pkgs.qemu pkgs.iproute2 pkgs.systemd ];
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
          GUEST_SERIAL = "/tmp/guest-serial.log"; # the guest journal (incl. the deadman) forwards here
        };
      };
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # The shipped NIC contract: carrier-bearing veth parent, the guest's two LAN NICs as macvtap
    # children in install.sh's order (sys0 -> eth1, svc0 -> eth2), and the private host<->guest
    # link as a plain tap. The macvlan shim this used to build was the rig granting itself
    # reachability the product lacked ([V3b.19a]).
    #
    # The tap carries TWO addresses for now: 10.0.0.129/32, this host's own end of the system
    # subnet, which is what everything here actually uses; and 10.11.9.1/24, which survives for the
    # single consumer that has not moved yet (the cloud-witness forwarder) and goes with it.
    #
    # THE PRIVATE LINK MATTERS TWICE HERE, and the second reason is this test's own subject: the
    # host's recovery rung reads the guest's reboot GATE across it. The gate now answers at the
    # guest's NODE IP (10.0.0.1:7790) rather than a baked private-link address -- one address per
    # node, whatever is asking ([V3b.26b]) -- and the host resolves it over this tap through the
    # permanent neighbour entry the agent pins, because the guest will not ARP for an eth1 address
    # on eth3 ([B.101]). Without the link the gate had nowhere to be read, so this rig proved the
    # guest reboots while being structurally unable to exercise the guard that decides whether it
    # may -- a second place the rig was quietly narrower than the product.
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )
    host.succeed("qemu-img create -f qcow2 -b ${guestDisk}/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    # Boot + converge (the agent launches the guest, drives bring-up).
    host.succeed("systemctl start briard-agent")
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)

    # THE HOST REACHES ITS GUEST AT THE NODE IP -- asserted before the VIP curl below, because it
    # is the hop the curl depends on and a failure here says WHICH half broke. This rig had no such
    # assertion when [V3b.26b] moved the reboot gate onto this address, and its absence cost a
    # debugging round: the curl failed and nothing said whether the private link or the VIP route
    # was at fault.
    #
    # What it catches, concretely: the link is UNNUMBERED, so both ends pin a permanent neighbour
    # instead of ARPing. Drop either pin and this fails -- with no address on its outgoing NIC the
    # guest's ARP borrows a source from another one (arp_announce=2), and what it borrows is eth0's
    # slirp address, which on this rig is the outer host's own, so nothing ever answers. Measured
    # on the wire, not reasoned about.
    host.wait_until_succeeds("ping -c1 -W2 10.0.0.1", timeout=60)
    print("the host reaches its guest at the node IP over the unnumbered private link")

    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)

    qemu_before = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    print(f"guest qemu pid before: {qemu_before}")

    # === THE PROOF: kill the host agent and leave it dead well past T_deadman (baked ~8s). ===
    # The guest keeps running (agent lifecycle decoupled from the guest). Its deadman sees no host
    # contact, fires — and, being a lone node with nobody to outage, must REBOOT.
    host.succeed("systemctl stop briard-agent")

    # The separate briard-deadman guest service (decoupled from the crash-looping guest agent) sees
    # the contact stamp go stale past T_deadman (~8s) and evaluates.
    host.wait_until_succeeds("grep -q 'briard-deadman' /tmp/guest-serial.log", timeout=90)

    # 1) It chose to REBOOT, and said so. The inverse assertion ("degraded, holding") is what this
    # test carried until V3.31; if the gate is ever re-derived from the majority formula, a lone
    # node silently goes back to holding and this is the line that catches it.
    host.wait_until_succeeds("grep -q 'deadman: rebooting' /tmp/guest-serial.log", timeout=120)
    host.fail("grep -q 'degraded, holding' /tmp/guest-serial.log")

    # 2) QEMU EXITED — the restart is visible to the supervisor rather than hidden inside the VM.
    # This is `-no-reboot` doing its job, and it is what lets the host count restarts, damp a crash
    # loop, and relaunch a stopped guest without waiting out the silence window.
    host.wait_until_fails(f"kill -0 {qemu_before}", timeout=180)
    host.succeed("! systemctl is-active --quiet briard-guest.service")
    print("guest rebooted itself; qemu exited and the unit stopped (visible to the supervisor)")

    # === Recovery: the host agent returns and brings the guest back. ===
    # A lone node's self-reboot cannot complete without the agent — nothing else launches VMs — so
    # this half is the other side of the same design: the agent is the sole policy supervisor, and
    # every restart passes through it.
    host.succeed("systemctl start briard-agent")
    # ONE query, and it must match QEMU rather than anything mentioning the disk. A bare
    # `pgrep -f guest.qcow2` also matches the transient `systemd-run … -drive file=/tmp/guest.qcow2`
    # wrapper the agent launches through, so a wait would return on the wrapper, the wrapper would
    # exit, and a second pgrep a moment later would find nothing. (Measured: the wait returned in
    # 0.02 s, before the guest unit had even started.) Taking the pid from the wait's own output
    # closes the gap between the two calls as well.
    qemu_after = host.wait_until_succeeds(
        "pgrep -f 'qemu-system-x86_64.*guest.qcow2'", timeout=300
    ).strip().splitlines()[0]
    assert qemu_after != qemu_before, f"same qemu pid {qemu_after} — the guest never actually restarted"
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    print(f"guest came back: pid {qemu_before} -> {qemu_after}, converged and serving")

    # 4) The guest agent did not spend the outage crash-looping. It serves ONE host connection and
    # exits on EOF (Restart=always puts it back on a freshly opened port), and with the host end gone
    # for good every reopen EOFs the instant it is read — ~48 restarts in 30s before [B.35]. It now
    # pauses ~5s before that exit, so a whole run costs a couple of restarts. Both bounds matter:
    # the upper one catches the busy loop coming back, the lower one catches this grep going stale
    # against a systemd message that moved (a rename would make the assertion vacuously pass, and
    # this is the only place that counts these).
    restarts = int(
        host.succeed(
            "grep -c 'briard-guest-agent.service: Scheduled restart job' /tmp/guest-serial.log || true"
        ).strip()
    )
    print(f"guest-agent restarts across the run: {restarts}")
    assert restarts >= 1, "no guest-agent restart logged at all — the grep string has gone stale"
    assert restarts < 8, f"{restarts} guest-agent restarts — the EOF crash loop is back"

    print("a lone node reboots itself when its host agent dies, and the reboot goes through the supervisor")
  '';
}
