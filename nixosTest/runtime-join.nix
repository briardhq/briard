# Runtime anchor pairing: grow a single-node DRBD green into a 3-node mesh (2 anchors +
# a diskless witness) at RUNTIME, without the static-PEERS-edit + restart the fleet tests bake.
#
# This is the substrate proof for the pairing mechanism (the "second anchor joins
# blank" join). The host agent's role (a DirectivePair -> cfg.applyPair -> Adjust/BringUp) is
# validated at-rest in agent/host/pair_test.go + agent/guestagent (the drbd.adjust verb); HERE we
# drive the exact DRBD operation that verb runs -- rewrite the .res + `drbdadm adjust` -- to prove
# the SUBSTRATE supports it: the serving primary keeps its data + quorum while a blank anchor
# resyncs to UpToDate and the witness casts the 3rd vote, and the whole thing survives a failover
# onto the freshly-joined anchor. Same split as the rest of the HA net (lib.nix drives the DRBD
# primitives, the agent logic is at-rest -- [[v3-2-real-ha-upgrade]]).
#
# Runtime growth REWRITES the resource config, so each node is brought up on the `.res` it should
# START from -- anchor1 alone, the joiners already naming all three -- and the growth itself
# rewrites the file in place, which is exactly what the guest's drbd.adjust verb does.
#
# No nested KVM (the L1 node runs DRBD directly), so it rides the fast `drbd` tag.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };

  # THE NODE IS lib.nix's, NOT A COPY OF IT. This file used to roll its own mkNode for one
  # reason: lib.nix baked the `.res` as a read-only store symlink, and rewriting that file at
  # runtime is this test's whole subject. [V3b.33](d) deleted the symlink -- briard-node-storage
  # writes the `.res` from the spec now -- so the ~60 duplicated lines had nothing left to differ
  # about, and a rig that quietly diverges from the shared node is how [B.141] and [B.125] both
  # got their bugs.
  #
  # Each machine is handed the resource it should COME UP on, which is the only per-node
  # difference this test needs at boot: anchor1 alone in a mesh-of-one, the joiners already
  # naming all three. The growth itself is a runtime rewrite, below.
  singleRes = h.mkResource [ { name = "anchor1"; id = 0; } ];
  threeRes = h.mkResource [
    { name = "anchor1"; id = 0; }
    { name = "anchor2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  # ...and as a FILE, for the one step that rewrites the running node's config in place.
  threeResFile = pkgs.writeText "r0-three.res" threeRes;
in
pkgs.testers.runNixOSTest {
  name = "runtime-join";
  skipTypeCheck = true;

  # Declaration order fixes nodeNumber -> the DRBD address: anchor1=10.0.0.1, anchor2=.2, witness=.3.
  nodes = {
    anchor1 = h.mkNode { resource = singleRes; inherit fixture; };
    anchor2 = h.mkNode { resource = threeRes; inherit fixture; };
    # A diskless voter: no tier to build, no promoter, and no fixture to warm -- it never serves.
    witness = h.mkNode {
      resource = threeRes;
      diskless = true;
      promoter = false;
    };
  };

  testScript = ''
    ${h.fixtureHelpers}

    start_all()
    for m in [anchor1, anchor2, witness]:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
    for m in [anchor1, anchor2]:
        m.wait_for_unit("briard-test-fixture-install.service") # the image, warm on both anchors

    # --- Phase 1: anchor1 comes up single-node (mesh-of-one) green, serving the VIP ---
    # The SEED of a new flock: the unit creates the LV, creates metadata on it, attaches, and --
    # being the seed -- declares it UpToDate and arms the one-time format. All of it in one place
    # now ([V3b.33](d)), where the harness used to hand-roll the last two by hand.
    anchor1.succeed("briard-test-storage --seed")
    anchor1.succeed("systemctl start drbd-reactor.service")
    anchor1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    # The front door first, with nothing installed: that is what a node coming up single-handed
    # actually looks like. Then the service is installed onto the volume it now holds, which is
    # what the joining anchor later replicates and converges from.
    anchor1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    install_fixture(anchor1)
    anchor1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)

    # Let the service persist a margin of ticks on the single node, then snapshot the counter --
    # our data-survival handle across the pairing and the later failover.
    anchor1.wait_until_succeeds(
        "test $(curl -fsS http://192.168.1.100:8080/state | tr -cd 0-9) -ge 10", timeout=60
    )
    pre = int(anchor1.succeed("curl -fsS http://192.168.1.100:8080/state | tr -cd 0-9").strip())
    print(f"single-node green, pre-pairing ticks={pre}")

    # --- Phase 2: bring the joiners up BLANK on the 3-node config, ready to connect ---
    # anchor2: a fresh (blank) replica -> Inconsistent, it will SyncTarget from anchor1. It must
    # NOT new-current-uuid (that would declare it UpToDate and split-brain the primary's data).
    anchor2.succeed("briard-test-storage")
    # Witness: diskless quorum voter -- no tier, no metadata, no create-md.
    witness.succeed("briard-test-storage")

    # --- THE RUNTIME GROWTH: adjust anchor1 mesh-of-one -> 3-node mesh, in place ---
    # Exactly what the drbd.adjust verb runs (rewrite the .res + `drbdadm adjust`): no create-md,
    # no restart, the primary's disk stays attached + UpToDate and it keeps serving.
    anchor1.succeed("cp ${threeResFile} /run/briard/drbd.d/r0.res")
    anchor1.succeed("drbdadm adjust r0")

    # Both peers connect; the blank anchor resyncs from anchor1 and reaches UpToDate; quorum is now
    # majority-of-3 (survives one loss). cstate counts connected peers; dstate is the disk state.
    anchor1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -eq 2", timeout=180)
    # A 3-node dstate prints local + per-peer fields (e.g. UpToDate/UpToDate/Diskless); match the
    # LOCAL field so we assert anchor2's own disk finished resyncing, whatever the peers report.
    anchor2.wait_until_succeeds("drbdadm dstate r0 | grep -q '^UpToDate'", timeout=240)
    print("second anchor joined at runtime and resynced to UpToDate; 3-node mesh connected")

    # --- Data survived + VIP unbroken: the primary kept serving across the growth ---
    anchor1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=60)
    post = int(anchor1.succeed("curl -fsS http://192.168.1.100:8080/state | tr -cd 0-9").strip())
    assert post >= pre, f"data regressed across pairing: {pre} -> {post}"
    print(f"primary kept its data across the growth: ticks {pre} -> {post}")

    # --- Failover onto the freshly-joined anchor: the pre-pairing data crossed the link ---
    anchor2.succeed("systemctl start drbd-reactor.service")  # so it can be promoted
    anchor1.crash()  # anchor2 + witness = 2/3 quorum -> anchor2 promotes, VIP moves
    anchor2.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=120)
    anchor2.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=180)
    survived = int(anchor2.succeed("curl -fsS http://192.168.1.100:8080/state | tr -cd 0-9").strip())
    assert survived >= pre, f"pre-pairing data not on the new primary after failover: pre={pre} got={survived}"
    print(f"runtime pairing proven: single-node -> 3-node mesh, data {pre} present on the joined anchor after failover ({survived})")
  '';
}
