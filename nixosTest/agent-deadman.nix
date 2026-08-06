# The host-agent deadman on a LONE (quorum-critical) node MUST NOT self-outage — the
# load-bearing safety property "a deadman reboot can never cause a serving outage".
#
# Single-node harness (like agent-readopt): the guest is DRBD single-node, so it is
# quorum-critical — QuorumSafe(total=1) is false → the deadman must HOLD (keep serving), never
# reboot. We kill the host agent for well past T_deadman (baked short via BRIARD_DEADMAN in the
# guest image) and prove: the guest keeps serving (VIP uninterrupted), the SAME qemu process is
# still running (no reboot), and the in-guest deadman actually evaluated and chose to hold (its
# log rides the guest serial via ForwardToConsole). Then the host agent returns and the deadman
# recovers. (The quorum-SAFE → reboots-and-fails-over path needs a 2-node harness; the reboot
# decision itself is unit-proven, and the mechanism is the ordinary graceful-reboot failover.)
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
          ExecStart = "${agent}/bin/briard-agent";
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
          SERVICE_TAP = "svc0";
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

    host.succeed(
        "ip link add parent type veth peer name parent_peer && ip link set parent_peer up && ip link set parent up && "
        "ip link add link parent name shim0 type macvlan mode bridge && ip addr add 192.168.1.1/24 dev shim0 && ip link set shim0 up && "
        "ip link add link parent name svc0 type macvtap mode bridge && ip link set svc0 up"
    )
    host.succeed("qemu-img create -f qcow2 -b ${guestDisk}/nixos.qcow2 -F qcow2 /tmp/guest.qcow2")
    host.succeed("truncate -s 512M /tmp/data.img")

    # Boot + converge (the agent launches the guest, drives bring-up).
    host.succeed("systemctl start briard-agent")
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)

    qemu_before = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    print(f"guest qemu pid before: {qemu_before}")

    # A continuous VIP poller — a deadman self-outage (a wrong reboot) would show FAIL ticks.
    host.succeed(
        "rm -f /tmp/vip.log; "
        "( i=0; while [ $i -lt 60 ]; do "
        "curl -fsS -m3 http://192.168.1.100/healthz >/dev/null 2>&1 && echo PASS || echo FAIL; "
        "i=$((i+1)); sleep 0.5; done > /tmp/vip.log 2>&1 & )"
    )
    host.wait_until_succeeds("test $(wc -l < /tmp/vip.log) -ge 3", timeout=15)

    # === THE PROOF: kill the host agent and leave it dead well past T_deadman (baked ~8s). ===
    # The guest keeps running (agent lifecycle decoupled from the guest). Its deadman sees
    # no host contact, fires — and, being a lone (quorum-critical) node, must HOLD, not reboot.
    host.succeed("systemctl stop briard-agent")

    # The separate briard-deadman guest service (decoupled from the crash-looping guest agent) sees
    # the contact stamp go stale past T_deadman (~8s) and evaluates.
    host.wait_until_succeeds(
        "grep -q 'briard-deadman' /tmp/guest-serial.log", timeout=90
    )
    # It must have chosen to HOLD (quorum-critical), never reboot.
    host.succeed("grep -q 'degraded, holding' /tmp/guest-serial.log")
    host.fail("grep -q 'deadman: rebooting' /tmp/guest-serial.log")

    # 1) Same qemu process — the lone node did NOT reboot itself.
    qemu_after = host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0]
    print(f"guest qemu pid after deadman fired: {qemu_after}")
    assert qemu_before == qemu_after, f"lone node rebooted ({qemu_before} -> {qemu_after}) — deadman self-outaged"

    # 2) Zero VIP interruption while the agent was dead + the deadman held.
    host.wait_until_succeeds("test $(wc -l < /tmp/vip.log) -ge 60", timeout=60)
    fails = int(host.succeed("grep -c FAIL /tmp/vip.log || true").strip() or "0")
    passes = int(host.succeed("grep -c PASS /tmp/vip.log || true").strip() or "0")
    print(f"VIP poll while the agent was dead: {passes} PASS / {fails} FAIL")
    assert passes >= 20, f"poller barely ran ({passes} PASS) — not a real window"
    assert fails == 0, f"VIP dropped {fails} tick(s) — a lone node self-outaged on the deadman"

    # === Recovery: the host agent returns → it re-adopts the running guest → the deadman clears. ===
    since = host.succeed("date +'%Y-%m-%d %H:%M:%S'").strip()
    host.succeed("systemctl start briard-agent")
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since='{since}' | grep -q 're-adopting running guest'", timeout=120
    )
    host.wait_until_succeeds("grep -q 'link restored' /tmp/guest-serial.log", timeout=60)
    # Still the same guest, still serving.
    assert qemu_before == host.succeed("pgrep -f guest.qcow2").strip().splitlines()[0], "guest changed on recovery"
    host.succeed("curl -fsS http://192.168.1.100/healthz")

    print("a lone node holds (keeps serving) when its host agent dies — the deadman never self-outages")
  '';
}
