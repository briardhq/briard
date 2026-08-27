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
        # The fixture is installed the way a service is installed, before anything promotes:
        # tarball staged, real renderer run, quadlet units generated, chain written.
        m.wait_for_unit("briard-test-fixture-install.service")
        m.succeed("modprobe drbd")
        m.succeed("drbdadm create-md --force r0")
        # Fire the stock bring-up unit: drbd@<res>.target → drbd@<res>.service →
        # `drbdadm adjust` (attach + connect, leaves the node Secondary).
        m.succeed("systemctl start drbd@r0.target")

    # The chain the promoter runs came from the RENDERER, not from this file.
    service_units = fixture_units(node1)
    print(f"service units in the renderer's chain: {service_units}")
    for unit in service_units:  # quadlet really generated them at daemon-reload
        node1.succeed(f"systemctl cat {unit} >/dev/null")

    node1.wait_until_succeeds("drbdadm cstate r0 | grep -q Connected")
    # Skip the initial sync so the resource is promotable without force-promotion.
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")

    # Hand off to the promoter: with r0 provisioned and UpToDate, start
    # drbd-reactor on both nodes. It promotes exactly one (quorum-gated, no
    # --force) and runs the ordered unit there.
    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    # Identify the primary, then give it the service's storage — only it has the volume mounted.
    node1.wait_until_succeeds("drbdadm role r0 | grep -qE 'Primary|Secondary'", timeout=90)
    primary = node1 if node1.execute("drbdadm role r0 | grep -q Primary")[0] == 0 else node2
    secondary = node2 if primary == node1 else node1
    dataroot = provision_fixture(primary)

    # Reaching the VIP's /healthz proves the whole chain converged on the primary
    # (promote → mount → service past its slow start → VIP claimed).
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)

    primary.succeed("systemctl is-active drbd-promote@r0.service")  # stock promote ran
    primary.succeed("systemctl is-active briard-data.service")
    for unit in service_units:
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
