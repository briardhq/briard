# drbd-reactor's promoter runs the ordered failover unit on the primary.
#
# Steady-state only (no kill): bring r0 up on both nodes, then let drbd-reactor
# promote exactly one and start the ordered unit {promote → data mount → payload
# → VIP} there. Assert it converges on the primary and runs nowhere else.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    resource = h.mkResource [
      { name = "node1"; id = 0; }
      { name = "node2"; id = 1; }
    ];
  };
in
pkgs.testers.runNixOSTest {
  name = "drbd-promote";

  nodes = {
    node1 = node;
    node2 = node;
  };

  testScript = ''
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
        m.succeed("drbdadm create-md --force r0")
        # Fire the stock bring-up unit: drbd@<res>.target → drbd@<res>.service →
        # `drbdadm adjust` (attach + connect, leaves the node Secondary).
        m.succeed("systemctl start drbd@r0.target")

    node1.wait_until_succeeds("drbdadm cstate r0 | grep -q Connected")
    # Skip the initial sync so the resource is promotable without force-promotion.
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")

    # Hand off to the promoter: with r0 provisioned and UpToDate, start
    # drbd-reactor on both nodes. It promotes exactly one (quorum-gated, no
    # --force) and runs the ordered unit there.
    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    # Reaching the VIP's /healthz proves the whole chain converged on the primary
    # (promote → mount → payload past its slow start → VIP claimed).
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")

    # Identify the primary, then assert the unit landed there and nowhere else.
    primary = node1 if node1.execute("drbdadm role r0 | grep -q Primary")[0] == 0 else node2
    secondary = node2 if primary == node1 else node1

    primary.succeed("systemctl is-active drbd-promote@r0.service")  # stock promote ran
    primary.succeed("systemctl is-active briard-data.service")
    primary.succeed("systemctl is-active podman-briard-payload.service")
    primary.succeed("systemctl is-active briard-vip.service")
    # Assert the DRBD device is mounted (the real mount), not the subvolume path —
    # a btrfs subvolume isn't reliably reported as a mountpoint (kernel-version
    # dependent); the state.json check below proves the dummy's subvolume is live.
    primary.succeed("mountpoint -q /var/lib/briard")
    primary.wait_until_succeeds("test -f /var/lib/briard/dummy/state.json")
    primary.succeed("ip -4 addr show dev eth1 | grep -q 192.168.1.100")

    # The secondary is Secondary and runs none of the ordered unit (single-primary).
    secondary.succeed("drbdadm role r0 | grep -q Secondary")
    secondary.fail("systemctl is-active podman-briard-payload.service")
    secondary.fail("systemctl is-active briard-vip.service")
    secondary.fail("ip -4 addr show dev eth1 | grep -q 192.168.1.100")
  '';
}
