# drbd-reactor's promoter runs the ordered failover unit on the primary.
#
# Steady-state only (no kill): r0 up on both nodes, drbd-reactor promotes exactly one and starts
# the ordered unit {promote → data mount → VIP → apps → reverse proxy → dashboard} there; it must
# converge on the primary and run nowhere else. The app in that chain is runtime-installed (a
# catalogued manifest, rendered by the real renderer, its units started by the promoter chain):
# the chain a user can produce, with no build-time payload slot.
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
        # The image is warmed onto every node before anything promotes, as install and prewarm
        # do in the product; briard-apps renders and chains at promotion, from the volume.
        m.wait_for_unit("briard-test-fixture-install.service")
        m.succeed("modprobe drbd")
        # The product's storage bring-up: build the tier, write the `.res`, create metadata, then
        # attach through the stock unit (drbd@<res>.target → drbd@<res>.service → `drbdadm
        # adjust`), which leaves the node Secondary.
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")

    # The unit names come from the renderer, not this file. The units themselves do not exist yet:
    # converge writes their source and reloads systemd at promotion.
    service_units = fixture_units(node1)
    print(f"app units the renderer produced: {service_units}")

    node1.wait_until_succeeds("drbdadm cstate r0 | grep -q Connected")
    # The seed declared its own disk UpToDate in its bring-up (no initial sync for it), so the
    # resource is promotable without force-promotion while the joiner resyncs. drbd-reactor
    # promotes exactly one (quorum-gated, no --force) and runs the ordered unit there.
    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    # The fixture is installed on the primary: only it has the volume mounted, and everything an
    # install writes (the manifest that is the app's identity, its storage) lives there.
    # install_fixture ends by running the product's own converge.
    import time

    # ⚠️ Wait for a completed promotion, not for a role. A wait on
    # `drbdadm role r0 | grep -qE "Primary|Secondary"` is vacuous: a node is always one or the
    # other, so it returns on the first poll, and the role is read mid-race. Nor is Primary the
    # same as promoted: a node holds the role for exactly as long as its chain takes to fail and
    # hand the resource on (node1 Primary when this looks, a failed member takes it down, node2
    # takes over), and every assertion below then runs against the node no longer serving,
    # printing a symptom ("briard-apps inactive") on the wrong machine that says nothing about
    # the cause. The promoter's target is the honest signal: active only where the whole ordered
    # chain came up.
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
    # The one that did not promote is Secondary, which makes the single-primary assertions at the
    # end mean anything. `-qx`, so a combined "Primary/Secondary" rendering could never match on
    # the peer's half.
    secondary.succeed("drbdadm role r0 | grep -qx Secondary")

    # Before the install the promoter has already run a full promotion with zero apps: briard-apps
    # converged to nothing and the VIP came up anyway -- the shipped state of every node a
    # stranger installs, which must not be a failure.
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    primary.succeed("systemctl is-active briard-apps.service")

    dataroot = install_fixture(primary)

    # Reaching the VIP's /healthz proves the whole chain converged on the primary
    # (promote → mount → VIP claimed → app past its slow start).
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)

    primary.succeed("systemctl is-active drbd-promote@r0.service")  # stock promote ran
    primary.succeed("systemctl is-active briard-primary-storage.service")
    for unit in service_units:
        primary.succeed(f"systemctl cat {unit} >/dev/null")  # converge wrote the source, quadlet generated it
        primary.wait_until_succeeds(f"test $(systemctl is-active {unit}) = active", timeout=120)
    primary.succeed("systemctl is-active briard-vip.service")
    # The DRBD device mount, not the subvolume path: a btrfs subvolume is not reliably reported
    # as a mountpoint (kernel-version dependent); state.json proves the data dir is live.
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
