# The lone node runs no DRBD.
#
# Every user starts single-node, and with one copy DRBD protects nothing: it turns any single bad
# block into a dead node (measured by the media-error rigs). So a lone anchor mounts its data LV
# directly, and a static target starts the promoter chain (the same five members, same order)
# instead of drbd-reactor. This rig proves that topology end to end on the guest:
#   1 the seam builds the LVs and formats; DRBD is nowhere (no device, no .res; the module
#     may be loaded)
#   2 the chain converges from the target: volume mounted, reverse proxy answering, a fixture
#     installed, and no reactor running
#   3 a daemon-reload (what switch-to-configuration does inside the OS upgrade bracket) does not
#     disturb the chain
#   4 a member that spends its start limit is held (the chain stops, the volume unmounts) and then
#     restarted by the hold itself, with no reboot: hold-and-restart, the lone node's symmetry
#     with a flock's demote-and-re-promote (the chain-member contract)
#   5 a member whose start fails once is retried, not held: the chain comes up with no hold, so
#     a oneshot's budget is real (RestartMode=direct)
#   6 a member whose start keeps failing spends that budget, and only then is held
#
# The status verb's second branch (Primary = chain active) is unit-tested, and the install rigs
# run it end to end through a real host agent; this rig owns the guest side.
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

    # === 1 The seam, and no DRBD =========================================================
    node1.succeed("briard-test-storage --seed")
    node1.succeed("test -b /dev/mapper/briardvol-data")
    node1.succeed("test -b /dev/mapper/briardvol-metadata")
    assert node1.succeed("cat /run/briard/topology.env").strip() == "BRIARD_TOPOLOGY=alone", \
        node1.succeed("cat /run/briard/topology.env")
    node1.fail("test -e /dev/drbd0")
    # The module may be loaded (the image loads it at boot); what matters is that no resource
    # is configured on it.
    rc, out = node1.execute("drbdsetup status --json")
    assert rc != 0 or "".join(out.split()) == "[]", f"a DRBD resource exists on a lone node: {out}"
    node1.fail("test -e /run/briard/drbd.d/r0.res")
    node1.fail("test -e /run/briard/drbd.d/r0.res")
    print("### 1 the LVs are built, the word is alone, and DRBD is nowhere")

    # === 2 The chain, from the target ====================================================
    node1.succeed("systemctl start briard-chain.target")
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    node1.succeed("mountpoint -q /var/lib/briard")
    assert node1.succeed("findmnt -no SOURCE /var/lib/briard").strip() == "/dev/mapper/briardvol-data"
    node1.fail("systemctl is-active drbd-reactor.service")
    node1.fail("systemctl is-active drbd-promote@r0.service")
    node1.succeed("systemctl is-active briard-primary-storage.service briard-apps.service briard-vip.service")
    install_fixture(node1)
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    for unit in fixture_units(node1):
        node1.succeed(f"systemctl is-active {unit}")
    print("### 2 the chain converged on the bare LV and the fixture serves")

    # === 3 The bracket gesture ===========================================================
    since = mount_since()
    node1.succeed("systemctl daemon-reload")
    time.sleep(3)
    assert mount_since() == since, f"a daemon-reload re-mounted the volume ({since} -> {mount_since()})"
    node1.succeed("curl -fsS http://192.168.1.100/healthz")
    print("### 3 a daemon-reload left the chain alone")

    # === 4 Hold-and-restart ==============================================================
    # Spend the reverse proxy's start limit (StartLimitBurst=5). Under RestartMode=direct the
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
    node1.wait_until_succeeds("journalctl -q -u briard-promotion-hold.service --no-pager | grep -q .", timeout=30)
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

    # === 5 A start that fails once is retried ============================================
    # briard-apps fails its first start and succeeds on the retry: a manifest that does not
    # parse, removed by a watcher the moment converge has refused it. The fault is data on the
    # volume, never a unit file. The chain starts from its target, the shape of a promotion or
    # the hold's own restart, so every member queued behind briard-apps sees the failed start.
    fault = "/var/lib/briard/.apps/zz-broken.json"
    refused = "zz-broken.json does not parse"

    def apps_refusals(since):
        return int(node1.succeed(
            f"journalctl -q -o cat -u briard-apps.service --since=@{since} | grep -c '{refused}' || true"
        ).strip())

    def held_since(since):
        return node1.execute(f"journalctl -q -u briard-promotion-hold.service --since=@{since} | grep -q .")[0] == 0

    def restart_chain():
        node1.succeed("systemctl stop briard-chain.target")
        node1.wait_until_fails("mountpoint -q /var/lib/briard", timeout=60)
        node1.wait_until_fails("systemctl is-active briard-primary-storage.service", timeout=60)
        node1.succeed("systemctl start --no-block briard-chain.target")

    time.sleep(1)  # the journal reads below are by the second; keep step 4's lines out
    t5 = node1.succeed("date +%s").strip()
    node1.succeed(f"echo '{{' > {fault} && sync")
    node1.succeed(
        "systemd-run --unit=briard-test-clear-fault --collect --setenv=PATH=\"$PATH\" "
        f"sh -c 'until journalctl -q -o cat -u briard-apps.service --since=@{t5} | grep -q \"{refused}\"; "
        f"do sleep 0.1; done; rm {fault}'"
    )
    restart_chain()
    node1.wait_until_succeeds("systemctl is-active briard-chain.target briard-apps.service", timeout=120)
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    n = apps_refusals(t5)
    assert n == 1, f"PRECOND: the fault should have refused exactly one start, it refused {n}"
    node1.fail(f"test -e {fault}")
    assert not held_since(t5), "one failed start of briard-apps fired the hold"
    for unit in fixture_units(node1):
        node1.wait_until_succeeds(f"systemctl is-active {unit}", timeout=120)
    print("### 5 briard-apps failed once, was retried, and the chain came up with no hold")

    # === 6 A start that keeps failing is held, after its budget ==========================
    # The same fault, left in place. Step 5 spent two of the five starts, so the budget is
    # cleared first; the hold must come only once all five are spent.
    node1.succeed("systemctl reset-failed briard-apps.service")
    time.sleep(1)
    t6 = node1.succeed("date +%s").strip()
    node1.succeed(f"echo '{{' > {fault} && sync")
    restart_chain()
    node1.wait_until_succeeds(
        f"journalctl -q -u briard-promotion-hold.service --since=@{t6} | grep -q .", timeout=120)
    n = apps_refusals(t6)
    assert n >= 5, f"briard-apps was held after {n} failed starts; its budget is 5"
    node1.wait_until_fails("mountpoint -q /var/lib/briard", timeout=60)
    print(f"### 6 briard-apps was held after {n} failed starts, and the hold stopped the chain")
  '';
}
