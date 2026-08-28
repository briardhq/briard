# drbd-reactor's promoter runs the ordered failover unit on the primary.
#
# Steady-state only (no kill): bring r0 up on both nodes, then let drbd-reactor
# promote exactly one and start the ordered unit {promote → data mount → service
# → VIP} there. Assert it converges on the primary and runs nowhere else.
#
# The service in that chain is a RUNTIME-INSTALLED one ([V3b.3](e)): the fixture arrives as a
# catalogued manifest, is rendered by the real renderer at boot, and its units are what the
# promoter starts. It used to be the build-time payload slot — a mechanism no shipped node has —
# so the chain this test drove was one no user could produce.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    inherit fixture;
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
    ${h.fixtureHelpers}
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        # The fixture's image is warmed onto every node before anything promotes, the way
        # install and prewarm warm it in the product. Nothing is rendered or chained here --
        # briard-services does that at promotion, from the volume ([V3b.3](f)).
        m.wait_for_unit("briard-test-fixture-install.service")
        m.succeed("modprobe drbd")
        m.succeed("drbdadm create-md --force r0")
        # Fire the stock bring-up unit: drbd@<res>.target → drbd@<res>.service →
        # `drbdadm adjust` (attach + connect, leaves the node Secondary).
        m.succeed("systemctl start drbd@r0.target")

    # The service units come from the RENDERER, not from this file. They are NOT generated yet:
    # converge writes their source and reloads systemd at promotion, so asking for them here
    # would be asking before the product has done its half.
    service_units = fixture_units(node1)
    print(f"service units the renderer produced: {service_units}")

    node1.wait_until_succeeds("drbdadm cstate r0 | grep -q Connected")
    # Skip the initial sync so the resource is promotable without force-promotion.
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")

    # Hand off to the promoter: with r0 provisioned and UpToDate, start
    # drbd-reactor on both nodes. It promotes exactly one (quorum-gated, no
    # --force) and runs the ordered unit there.
    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    # Identify the primary, then install the fixture on it -- only it has the volume mounted, and
    # everything an install writes (the manifest that IS the service identity, its storage) lives
    # there. install_fixture ends by running the product's own converge.
    node1.wait_until_succeeds("drbdadm role r0 | grep -qE 'Primary|Secondary'", timeout=90)
    primary = node1 if node1.execute("drbdadm role r0 | grep -q Primary")[0] == 0 else node2
    secondary = node2 if primary == node1 else node1

    # BEFORE the install, the promoter has already run a full promotion with ZERO services --
    # briard-services converged to nothing and the VIP came up anyway. That is the shipped state
    # of every node a stranger installs, and it must not be a failure ([V3.15]).
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    primary.succeed("systemctl is-active briard-services.service")

    dataroot = install_fixture(primary)

    # Reaching the VIP's /healthz proves the whole chain converged on the primary
    # (promote → mount → service past its slow start → VIP claimed).
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)

    primary.succeed("systemctl is-active drbd-promote@r0.service")  # stock promote ran
    primary.succeed("systemctl is-active briard-data.service")
    for unit in service_units:
        primary.succeed(f"systemctl cat {unit} >/dev/null")  # converge wrote the source, quadlet generated it
        primary.wait_until_succeeds(f"test $(systemctl is-active {unit}) = active", timeout=120)
    primary.succeed("systemctl is-active briard-vip.service")
    # Assert the DRBD device is mounted (the real mount), not the subvolume path —
    # a btrfs subvolume isn't reliably reported as a mountpoint (kernel-version
    # dependent); the state.json check below proves the fixture's data dir is live.
    primary.succeed("mountpoint -q /var/lib/briard")
    primary.wait_until_succeeds(f"test -s {dataroot}/app/state.json", timeout=60)
    primary.succeed("ip -4 addr show dev eth1 | grep -q 192.168.1.100")

    # The secondary is Secondary and runs none of the ordered unit (single-primary).
    secondary.succeed("drbdadm role r0 | grep -q Secondary")
    for unit in service_units:
        secondary.fail(f"systemctl is-active {unit}")
    secondary.fail("systemctl is-active briard-vip.service")
    secondary.fail("ip -4 addr show dev eth1 | grep -q 192.168.1.100")
  '';
}
