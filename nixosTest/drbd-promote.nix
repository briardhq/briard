# drbd-reactor's promoter runs the ordered failover unit on the primary.
#
# Steady-state only (no kill): bring r0 up on both nodes, then let drbd-reactor
# promote exactly one and start the ordered unit {promote → data mount → service
# → VIP} there. Assert it converges on the primary and runs nowhere else.
#
# The service in that chain is a RUNTIME-INSTALLED one ([V3b.3](e)): the fixture arrives as a
# catalogued manifest, is rendered by the real renderer at boot, and its units are what the
# promoter starts. It used to be a build-time payload slot -- a mechanism no shipped node had --
# so the chain this test drove was one no user could produce ([V3b.3](e2) deleted it).
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
        # The product's own storage bring-up ([V3b.33](d)): build the tier, write the `.res`,
        # create metadata, then attach through the STOCK unit (drbd@<res>.target →
        # drbd@<res>.service → `drbdadm adjust`), which leaves the node Secondary.
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")

    # The service units come from the RENDERER, not from this file. They are NOT generated yet:
    # converge writes their source and reloads systemd at promotion, so asking for them here
    # would be asking before the product has done its half.
    service_units = fixture_units(node1)
    print(f"service units the renderer produced: {service_units}")

    node1.wait_until_succeeds("drbdadm cstate r0 | grep -q Connected")
    # Skip the initial sync so the resource is promotable without force-promotion.

    # Hand off to the promoter: with r0 provisioned and UpToDate, start
    # drbd-reactor on both nodes. It promotes exactly one (quorum-gated, no
    # --force) and runs the ordered unit there.
    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    # Identify the primary, then install the fixture on it -- only it has the volume mounted, and
    # everything an install writes (the manifest that IS the service identity, its storage) lives
    # there. install_fixture ends by running the product's own converge.
    import time

    # ⚠️ WAIT FOR A COMPLETED PROMOTION, NOT FOR A ROLE ([B.151]).
    #
    # This used to be `wait_until_succeeds("drbdadm role r0 | grep -qE 'Primary|Secondary'")`,
    # which is VACUOUS: a node is always one or the other, so it returned on the first poll having
    # waited for nothing. The role was then read out of the middle of the promotion race.
    #
    # And being Primary is not the same as having promoted. A node can hold the role for exactly
    # as long as it takes its chain to fail and hand the resource on -- which is what happened on
    # the 2026-09-16 nightly: node1 was Primary when this line looked, a failed avahi killed its
    # chain, node2 took over, and every assertion below ran against the node that was no longer
    # serving. What it printed ("briard-services inactive") was a symptom on the wrong machine and
    # said nothing about the cause.
    #
    # The promoter's TARGET is the honest signal: it is active only where the whole ordered chain
    # came up.
    def serving_node(timeout=120):
        deadline = time.time() + timeout
        while time.time() < deadline:
            for m in machines:
                if m.execute("systemctl is-active drbd-services@r0.target")[0] == 0:
                    return m
            time.sleep(2)
        roles = " ".join(f"{m.name}={m.execute('drbdadm role r0')[1].strip()}" for m in machines)
        raise Exception(f"no node completed a promotion within {timeout}s (roles: {roles})")

    primary = serving_node()
    secondary = node2 if primary == node1 else node1
    # ...and the one that did not promote is Secondary, which is what makes the single-primary
    # assertions at the end of this file mean anything. `-qx`, so a combined "Primary/Secondary"
    # rendering could never match on the peer's half.
    secondary.succeed("drbdadm role r0 | grep -qx Secondary")

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
    primary.succeed("systemctl is-active briard-primary-storage.service")
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
