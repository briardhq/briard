# The host-agent deadman on a lone node reboots, and the reboot surfaces to the supervisor.
#
# Derived from the majority formula, the single-node case would hold (departing loses quorum):
# true, and irrelevant. There is no peer to outage, quorum is guaranteed the moment it comes back,
# and the data is on its own disk; holding would be a reflex that did nothing at all on the shape
# most briard installs have, the node alerting once and then sitting wedged and silent
# indefinitely. So single-node is its own clause in deadman.RebootAllowed.
#
# Single-node harness (like agent-readopt): the guest is a lone node, which runs no DRBD and
# reads as no peers, so it exercises exactly that clause. With the host agent killed for well
# past T_deadman (baked short via BRIARD_DEADMAN in the guest image), it proves:
#   1. the in-guest deadman evaluated and chose to reboot (its log rides the guest serial);
#   2. QEMU exited rather than resetting inside itself, the assertion most worth having:
#      `-no-reboot` makes a guest restart an event the supervisor can see, not a gap in the
#      control channel indistinguishable from every other gap. A guest reset in place would keep
#      the same QEMU pid, the VIP would blip, and every other check would still pass while the
#      agent had silently stopped being able to observe or count the restarts of its charge;
#   3. the host agent, on returning, brings the guest back.
#
# Heavy (nested VM) → the `integration` tag. Run one:
#   nix build .#tests.agent-deadman -L
{ pkgs, guestDisk, agent, netWrap, dressBase }:
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
          GUEST_IMAGE = "/tmp/guest.qcow2";
          # Where the host keeps its guest bundle tree, the default an install gets: the
          # image bakes only the firmware, so the deadman's own binary arrives by the dress
          # and a host holding no tree could never start it.
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
          GUEST_SERIAL = "/tmp/guest-serial.log"; # the guest journal (incl. the deadman) forwards here
        };
      };
    };

  testScript = ''
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")

    # The shipped NIC contract: a carrier-bearing veth parent, the two LAN NICs of the guest as
    # macvtap children in the agent order (sys0 -> eth1, svc0 -> eth2), and the private
    # host<->guest link as a plain tap (no macvlan shim: that would be the rig granting itself
    # reachability the product lacks). The tap carries two addresses: 10.0.0.129/32, the host end
    # of the system subnet, which is what everything here uses; and 10.11.9.1/24, the host end of
    # the private link (PRIV_HOST_CIDR in the product), which nothing dials but which avahi needs
    # on both ends to join the IPv4 mDNS group on the link.
    #
    # The private link matters twice here, the second reason being this test subject: the host
    # recovery rung reads the guest reboot gate across it. The gate answers at the guest node IP
    # (10.0.0.1:7790), not a private-link address (one address per node, whatever is asking), and
    # the host resolves it over this tap through the permanent neighbour entry the agent pins,
    # because the guest will not answer ARP for an eth1 address on eth3. Without the link the gate
    # has nowhere to be read, and the rig would prove the guest reboots while structurally unable
    # to exercise the guard that decides whether it may.
    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name sys0 type macvtap mode bridge && ip link set sys0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up && "
        "ip tuntap add briard-priv0 mode tap && ip addr add 10.11.9.1/24 dev briard-priv0 && ip addr add 10.0.0.129/32 dev briard-priv0 && ip link set briard-priv0 up"
    )
    host.succeed("ln -s ${guestDisk}/nixos.qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")
    # The bundle tree an install lays (briard-agent install), copied out of the store because the
    # host writes `guest.good` beside it.
    host.succeed("mkdir -p /opt/briard/agent && cp -r ${dressBase}/. /opt/briard/agent/ && chmod -R u+w /opt/briard/agent")

    # Boot + converge (the agent launches the guest, drives bring-up).
    host.succeed("systemctl start briard-agent")
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)

    # The host reaches its guest at the node IP, asserted before the VIP curl because it is the hop
    # the curl depends on: a failure here says which half broke, where a failed curl cannot tell
    # the private link from the VIP route. The link is unnumbered, so both ends pin a permanent
    # neighbour instead of ARPing. Drop either pin and this fails: with no address on its outgoing
    # NIC the guest ARP borrows a source from another one (arp_announce=2), the eth0 slirp
    # address, which on this rig is the outer host's own, so nothing ever answers.
    host.wait_until_succeeds("ping -c1 -W2 10.0.0.1", timeout=60)
    print("the host reaches its guest at the node IP over the unnumbered private link")

    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)

    qemu_before = host.succeed("pgrep -f 'qemu-system-x86_64.*guest.qcow2'").strip().splitlines()[0]
    print(f"guest qemu pid before: {qemu_before}")

    # === The proof: kill the host agent and leave it dead well past T_deadman (baked ~8s). ===
    # The guest keeps running (agent lifecycle decoupled from the guest). Its deadman sees no host
    # contact and fires, and as a lone node with nobody to outage it must reboot.
    host.succeed("systemctl stop briard-agent")

    # The separate briard-deadman.service in the guest (decoupled from the crash-looping guest
    # agent) sees the contact stamp go stale past T_deadman (~8s) and evaluates.
    host.wait_until_succeeds("grep -q 'briard-deadman' /tmp/guest-serial.log", timeout=90)

    # 1) It chose to reboot, and said so. "degraded, holding" is what a lone node would log under a
    # gate derived from the majority formula; if the gate is ever re-derived that way, a lone node
    # silently goes back to holding and this is the line that catches it.
    host.wait_until_succeeds("grep -q 'deadman: rebooting' /tmp/guest-serial.log", timeout=120)
    host.fail("grep -q 'degraded, holding' /tmp/guest-serial.log")

    # 2) QEMU exited: `-no-reboot` makes the restart visible to the supervisor rather than hidden
    # inside the VM, which lets the host count restarts, damp a crash loop, and relaunch a stopped
    # guest without waiting out the silence window.
    host.wait_until_fails(f"kill -0 {qemu_before}", timeout=180)
    host.succeed("! systemctl is-active --quiet briard-guest.service")
    print("guest rebooted itself; qemu exited and the unit stopped (visible to the supervisor)")

    # === Recovery: the host agent returns and brings the guest back. ===
    # A lone node's self-reboot cannot complete without the agent — nothing else launches VMs — so
    # this half is the other side of the same design: the agent is the sole policy supervisor, and
    # every restart passes through it.
    host.succeed("systemctl start briard-agent")
    # One query, matching QEMU rather than anything mentioning the disk: a bare
    # `pgrep -f guest.qcow2` also matches the transient `systemd-run … -drive file=/tmp/guest.qcow2`
    # wrapper the agent launches through, so a wait would return on the wrapper (possibly before
    # the guest unit has even started), the wrapper would exit, and a second pgrep a moment later
    # would find nothing. Taking the pid from the wait output also closes the gap between two calls.
    qemu_after = host.wait_until_succeeds(
        "pgrep -f 'qemu-system-x86_64.*guest.qcow2'", timeout=300
    ).strip().splitlines()[0]
    assert qemu_after != qemu_before, f"same qemu pid {qemu_after} — the guest never actually restarted"
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    print(f"guest came back: pid {qemu_before} -> {qemu_after}, converged and serving")

    # 4) The guest agent did not spend the outage crash-looping. It serves one host connection and
    # exits on EOF (Restart=always puts it back on a freshly opened port); with the host end gone
    # for good every reopen EOFs the instant it is read, ~48 restarts in 30s without a pause. It
    # pauses ~5s before that exit, so a whole run costs a couple of restarts. The upper bound
    # catches the busy loop coming back; the lower one catches this grep going stale against a
    # systemd message that moved (a rename would make the assertion pass vacuously, and nothing
    # else counts these).
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
