# THE LONE NODE RUNS NO DRBD ([B.145c]).
#
# Every user starts single-node, and with one copy DRBD protects nothing: it turns any single
# bad block into a dead node ([B.144]). So a lone anchor mounts its data LV directly, and the
# promoter chain -- the same seven members, in the same order -- is started by a static target
# instead of drbd-reactor. This rig is what single-node-promoter was for that topology: a lone
# node converges, serves, survives the OS-upgrade bracket's gesture, and holds-and-restarts.
#
#   1 the seam builds the LVs and formats; DRBD is nowhere (no device, no module, no .res)
#   2 the chain converges from the target: the volume is mounted, the front door answers, a
#     fixture installs -- and no reactor runs
#   3 a daemon-reload (what switch-to-configuration does inside the OS upgrade's bracket) does not
#     disturb the chain
#   4 a member that spends its start limit is HELD -- the chain stops and the volume unmounts --
#     and then RESTARTED by the hold itself, with no reboot: hold-and-restart, the lone node's
#     symmetry with a flock's demote-and-re-promote ([V3b.5](c))
#
# The status verb's second branch (Primary = chain active) is unit-tested; the install rigs
# exercise it end to end through a real host agent. What this rig owns is the guest side.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # The hold, shortened so the test can drive its whole lifecycle.
  holdSecs = 5;
  node = h.mkNode {
    inherit fixture;
    replicated = false;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  member = "briard-reverse-proxy.service";
in
pkgs.testers.runNixOSTest {
  name = "single-node-chain";
  nodes.node1 = {
    imports = [ node ];
    briard.promotionHoldSecs = holdSecs;
  };

  testScript = ''
    import time
    ${h.fixtureHelpers}
    member = "${member}"
    hold_secs = ${toString holdSecs}

    def state(unit):
        return node1.execute(f"systemctl is-active {unit}")[1].strip()

    def mount_since():
        return node1.succeed(
            "systemctl show -p ActiveEnterTimestampMonotonic --value briard-primary-storage.service"
        ).strip()

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service")  # the image, warmed before anything promotes
    boot_id = node1.succeed("cat /proc/sys/kernel/random/boot_id").strip()

    # === 1 THE SEAM, AND NO DRBD =========================================================
    node1.succeed("briard-test-storage --seed")
    node1.succeed("test -b /dev/mapper/briardservice-data")
    node1.succeed("test -b /dev/mapper/briardservice-metadata")
    assert node1.succeed("cat /run/briard/topology.env").strip() == "BRIARD_TOPOLOGY=alone", \
        node1.succeed("cat /run/briard/topology.env")
    node1.fail("test -e /dev/drbd0")
    node1.fail("lsmod | grep -qw drbd")
    node1.fail("test -e /run/briard/drbd.d/r0.res")
    print("### 1 the LVs are built, the word is alone, and DRBD is nowhere")

    # === 2 THE CHAIN, FROM THE TARGET ====================================================
    node1.succeed("systemctl start briard-chain.target")
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    node1.succeed("mountpoint -q /var/lib/briard")
    assert node1.succeed("findmnt -no SOURCE /var/lib/briard").strip() == "/dev/mapper/briardservice-data"
    node1.fail("systemctl is-active drbd-reactor.service")
    node1.fail("systemctl is-active drbd-promote@r0.service")
    node1.succeed("systemctl is-active briard-primary-storage.service briard-services.service briard-vip.service")
    install_fixture(node1)
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    for unit in fixture_units(node1):
        node1.succeed(f"systemctl is-active {unit}")
    print("### 2 the chain converged on the bare LV and the fixture serves")

    # === 3 THE BRACKET'S GESTURE =========================================================
    since = mount_since()
    node1.succeed("systemctl daemon-reload")
    time.sleep(3)
    assert mount_since() == since, f"a daemon-reload re-mounted the volume ({since} -> {mount_since()})"
    node1.succeed("curl -fsS http://192.168.1.100/healthz")
    print("### 3 a daemon-reload left the chain alone")

    # === 4 HOLD-AND-RESTART ==============================================================
    # Spend the front door's start limit (StartLimitBurst=5). Under RestartMode=direct the
    # crashes below the limit are absorbed; the one that crosses it fires OnFailure into the
    # hold, which on a lone node stops the chain, unmounts, waits, and starts it again.
    node1.succeed(f"systemctl reset-failed {member}")
    for i in range(1, 12):
        node1.execute(f"systemctl kill -s KILL {member}")
        time.sleep(3)
        if state(member) == "failed":
            print(f"### 4 {member} reached `failed` after {i} crashes")
            break
    assert state(member) == "failed", f"PRECOND: {member} never crossed its start limit ({state(member)})"
    # The hold ran, and it took the chain down: target inactive, volume unmounted.
    node1.wait_until_succeeds("journalctl -u briard-promotion-hold.service --no-pager | grep -q .", timeout=30)
    node1.wait_until_fails("mountpoint -q /var/lib/briard", timeout=60)
    node1.fail("systemctl is-active briard-chain.target")
    print("### 4 the hold stopped the chain and unmounted the volume")
    # ...and brought it back, by itself, with the start limit cleared and no reboot.
    node1.wait_until_succeeds("systemctl is-active briard-chain.target", timeout=hold_secs + 60)
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    node1.wait_until_succeeds(f"systemctl is-active {member}", timeout=60)
    node1.succeed("mountpoint -q /var/lib/briard")
    for unit in fixture_units(node1):
        node1.wait_until_succeeds(f"systemctl is-active {unit}", timeout=120)
    assert node1.succeed("cat /proc/sys/kernel/random/boot_id").strip() == boot_id, "the hold REBOOTED the node"
    node1.fail("systemctl is-active drbd-reactor.service")
    print("### 4 the hold restarted the chain: serving again, same boot")
  '';
}
