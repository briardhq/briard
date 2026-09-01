# Single-node (peer-less) DRBD is a first-class promoter mode.
#
# Every user starts single-node (onboarding is one node; a second joins later), so the
# promoter must work with NO peer. This is the regression guard for the drbd-reactor bug
# fixed by 0001-drbdstatus-default-connections-peerless.patch: on a mesh-of-one resource
# `drbdsetup show --json` OMITS the `connections` key (a peer-less config has zero
# connection stanzas), which unpatched drbd-reactor 1.11.0 cannot deserialize — its
# split-brain-avoidance detector then logs `IGNORING resource 'r0'` on every events2
# event and the event-driven reaction path goes dead (the resource is adopted once, then
# limps: reactive state never updates). The patch marks the field `#[serde(default)]`.
#
# Asserts a lone node promotes, the split-brain check parses cleanly (no IGNORING, policy
# detected), and the state survives the maintenance stop/restart (enter/exitMaintenance).
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # A single node: mesh-of-one — exactly what agent/drbd/config.go emits for empty PEERS
  # (connection-mesh { hosts node1; } -> zero DRBD connections).
  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
in
pkgs.testers.runNixOSTest {
  name = "single-node-promoter";
  nodes.node1 = node;

  testScript = ''
    ${h.fixtureHelpers}
    def assert_healthy_promoter(m, label):
        # The regression guard: the split-brain-avoidance detector must parse the
        # peer-less `drbdsetup show --json` — no IGNORING, and the success path ran.
        m.fail("journalctl -u drbd-reactor.service | grep -q 'IGNORING resource'")
        m.succeed("journalctl -u drbd-reactor.service | grep -q \"split-brain avoidance policy\"")
        m.succeed("drbd-reactorctl status | grep -q 'currently active on this node'")
        print(f"[{label}] promoter healthy: promoted, no IGNORING, policy detected")

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service") # the image, warmed before anything promotes
    node1.succeed("modprobe drbd")
    node1.succeed("drbdadm create-md --force r0")
    node1.succeed("systemctl start drbd@r0.target")
    # Make the peer-less resource UpToDate so the promoter can promote without --force.
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    # Arm the one-time format the way BRING-UP does ([B.126]): the product no longer formats on
    # the promotion path, so a harness that seeds a resource by hand leaves the same marker.
    node1.succeed("mkdir -p /run/briard && touch /run/briard/data.format")

    # Hand off to the promoter. On a mesh-of-one it must promote AND keep its event path
    # live — the bug left it adopted-but-unreactive (IGNORING every event).
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    # The whole ordered chain converged on the (sole) primary: promote -> mount -> services
    # -> VIP. The front door answers first, with zero services, because that is the shipped
    # state of a lone node; the fixture is installed onto the volume afterwards, which is the
    # only order a real single node can do it in.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    install_fixture(node1)
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    assert_healthy_promoter(node1, "first start")

    # The maintenance cycle stops then restarts the daemon (enter/exitMaintenance):
    # the resource stays Primary + services up throughout, and the restart must re-adopt
    # cleanly. This is the path where the bug bit a single-node managed upgrade.
    node1.succeed("systemctl stop drbd-reactor.service")
    node1.succeed("drbdadm role r0 | grep -q Primary")  # stays Primary while paused
    node1.succeed("systemctl start drbd-reactor.service")
    node1.sleep(10)
    node1.succeed("drbdadm role r0 | grep -q Primary")
    assert_healthy_promoter(node1, "after maintenance restart")
    node1.succeed("systemctl is-active briard-data.service briard-services.service briard-vip.service")
    for unit in fixture_units(node1):
        node1.succeed(f"systemctl is-active {unit}")
  '';
}
