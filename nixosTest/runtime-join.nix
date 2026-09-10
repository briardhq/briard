# Runtime anchor pairing, both directions ([B.145d]): a LONE node -- running its volume with no
# DRBD at all ([B.145]) -- gains its first peer, and later the flock ends and the survivor goes
# back to running alone.
#
# This is the substrate proof for the pairing mechanism. The host agent's role (a DirectivePair or
# DirectiveUnpair -> applyPair/applyUnpair -> record the mesh, reboot the guest) is validated
# at-rest in agent/host/pair_test.go; HERE we drive exactly what the guest does on the bring-up
# that follows -- node-storage's spec × disk rows -- to prove the SUBSTRATE supports it:
#
#   join   the lone anchor's data LV, already holding data, gets DRBD put UNDER it in place:
#          create-md on the metadata LV the layout reserved, this copy declared UpToDate BEFORE
#          any peer connects, no mkfs -- and the blank anchor then resyncs from it while the
#          witness casts the 3rd vote; the whole thing survives a failover onto the joined anchor;
#   leave  the survivor of that failover, told the flock has ended, comes back from a reboot with
#          DRBD metadata under a spec that says alone: REFUSED without the intent (a forgotten
#          flock looks exactly like this), and with it the metadata is wiped and the chain comes
#          up from its target, the data intact and no resource anywhere.
#
# The harness stands in for the host's one act, the guest reboot: for the join the chain comes
# down and node-storage is re-run on the rewritten spec (the fs must be unmounted either way);
# for the leave the node is rebooted for real, which is also what empties /run of the flock's
# `.res`. Same split as the rest of the HA net (lib.nix drives the primitives, the agent logic is
# at-rest -- [[v3-2-real-ha-upgrade]]).
#
# No nested KVM (the L1 node runs DRBD directly), so it rides the fast `drbd` tag.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };

  # THE NODE IS lib.nix's, NOT A COPY OF IT: a rig that quietly diverges from the shared node is
  # how [B.141] and [B.125] both got their bugs. Every machine carries the three-node `.res` in
  # its spec; the lone anchor's bring-up ignores it (it writes no `.res` alone) and its conversion
  # writes it, which is the whole point -- there is no mesh-of-one config to grow from any more.
  threeRes = h.mkResource [
    { name = "anchor1"; id = 0; }
    { name = "anchor2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
in
pkgs.testers.runNixOSTest {
  name = "runtime-join";
  skipTypeCheck = true;

  # Declaration order fixes nodeNumber -> the DRBD address: anchor1=10.0.0.1, anchor2=.2, witness=.3.
  nodes = {
    anchor1 = h.mkNode {
      resource = threeRes;
      replicated = false; # a lone node: no DRBD until its first peer joins
      inherit fixture;
    };
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

    def ticks(m):
        return int(m.succeed("curl -fsS http://192.168.1.100:8080/state | tr -cd 0-9").strip())

    def no_resource(m):
        rc, out = m.execute("drbdsetup status --json")
        assert rc != 0 or "".join(out.split()) == "[]", f"a DRBD resource exists on {m.name}: {out}"

    start_all()
    for m in [anchor1, anchor2, witness]:
        m.wait_for_unit("multi-user.target")
    for m in [anchor1, anchor2]:
        m.wait_for_unit("briard-test-fixture-install.service") # the image, warm on both anchors

    # --- Phase 1: anchor1 comes up ALONE -- btrfs on its LV, the chain from its target ---
    anchor1.succeed("briard-test-storage --seed")
    assert anchor1.succeed("cat /run/briard/topology.env").strip() == "BRIARD_TOPOLOGY=alone"
    no_resource(anchor1)
    anchor1.succeed("systemctl start briard-chain.target")
    # The front door first, with nothing installed: that is what a node coming up single-handed
    # actually looks like. Then the service is installed onto the volume it now holds, which is
    # what the joining anchor later replicates and converges from.
    anchor1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    install_fixture(anchor1)
    anchor1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)

    # Let the service persist a margin of ticks on the lone node, then snapshot the counter --
    # our data-survival handle across the conversion, the failover and the leave.
    anchor1.wait_until_succeeds(
        "test $(curl -fsS http://192.168.1.100:8080/state | tr -cd 0-9) -ge 10", timeout=60
    )
    pre = ticks(anchor1)
    print(f"lone node green with no DRBD, pre-pairing ticks={pre}")

    # --- Phase 2: bring the joiners up BLANK on the 3-node config, ready to connect ---
    # anchor2: a fresh (blank) replica -> Inconsistent, it will SyncTarget from anchor1. It must
    # NOT new-current-uuid (that would declare it UpToDate and split-brain the primary's data).
    for m in [anchor2, witness]:
        m.succeed("modprobe drbd")
    anchor2.succeed("briard-test-storage")
    # Witness: diskless quorum voter -- no tier, no metadata, no create-md.
    witness.succeed("briard-test-storage")
    # ...and they are already dialling anchor1, which is the shape the conversion has to be safe
    # against: a peer connected at the moment of `new-current-uuid --clear-bitmap` would be
    # declared UpToDate with no sync ([B.145a]). The convert row declares this disk UpToDate
    # attached-but-not-connected, so the joiner below syncs for real.

    # --- THE CONVERSION: anchor1's data LV gets DRBD put under it, in place ---
    # What the host does on the pair directive is record the mesh and reboot the guest; what the
    # bring-up then does is this. The chain comes down (the fs must be unmounted), the spec is
    # rewritten replicated + seed, node-storage runs its convert row, the promoter is armed.
    anchor1.succeed("systemctl stop briard-chain.target briard-primary-storage.service")
    anchor1.fail("mountpoint -q /var/lib/briard")
    anchor1.succeed("briard-test-storage --seed --flock")
    assert anchor1.succeed("cat /run/briard/topology.env").strip() == "BRIARD_TOPOLOGY=flock"
    anchor1.succeed("test -e /run/briard/drbd.d/r0.res")
    # UpToDate on its own, before the resync: the lone copy IS the data, and nothing formatted it.
    anchor1.succeed("drbdadm dstate r0 | grep -q '^UpToDate'")
    anchor1.succeed("systemctl start drbd-reactor.service")
    anchor1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)

    # Both peers connect; the blank anchor resyncs from anchor1 and reaches UpToDate; quorum is now
    # majority-of-3 (survives one loss). cstate counts connected peers; dstate is the disk state.
    anchor1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -eq 2", timeout=180)
    # A 3-node dstate prints local + per-peer fields (e.g. UpToDate/UpToDate/Diskless); match the
    # LOCAL field so we assert anchor2's own disk finished resyncing, whatever the peers report.
    anchor2.wait_until_succeeds("drbdadm dstate r0 | grep -q '^UpToDate'", timeout=240)
    print("second anchor joined at runtime and resynced to UpToDate; 3-node mesh connected")

    # --- Data survived + the VIP is back: the conversion kept the data ---
    anchor1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    post = ticks(anchor1)
    assert post >= pre, f"data regressed across the conversion: {pre} -> {post}"
    print(f"the lone node kept its data through the conversion: ticks {pre} -> {post}")

    # --- Failover onto the freshly-joined anchor: the pre-pairing data crossed the link ---
    anchor2.succeed("systemctl start drbd-reactor.service")  # so it can be promoted
    anchor1.crash()  # anchor2 + witness = 2/3 quorum -> anchor2 promotes, VIP moves
    anchor2.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=120)
    anchor2.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=180)
    survived = ticks(anchor2)
    assert survived >= pre, f"pre-pairing data not on the new primary after failover: pre={pre} got={survived}"
    print(f"runtime pairing proven: lone node -> 3-node mesh, data {pre} present on the joined anchor after failover ({survived})")

    # --- THE LEAVE: anchor1 is gone for good, and anchor2 keeps the home alone ---
    # The host, on the unpair directive, checks the survivor is serving and UpToDate, records the
    # one-member mesh, asserts convert=disable and reboots the guest. The node is rebooted for
    # real here; what follows is what its bring-up does, in the order that proves the cell.
    anchor2.succeed("drbdadm dstate r0 | grep -q '^UpToDate'")
    anchor2.shutdown()
    anchor2.start()
    anchor2.wait_for_unit("multi-user.target")
    anchor2.wait_for_unit("briard-test-fixture-install.service")
    # The REFUSE cell: alone in the spec, DRBD metadata on the LV, no intent -- exactly what a
    # host that forgot its flock would write. Nothing is mounted, nothing is wiped.
    anchor2.fail("briard-test-storage --alone")
    anchor2.fail("mountpoint -q /var/lib/briard")
    anchor2.succeed("journalctl -q -u briard-node-storage.service --no-pager | grep -q 'holds DRBD metadata'")
    # With the asserted intent: the metadata is wiped, no resource comes up, the chain does.
    anchor2.succeed("briard-test-storage --alone --disable")
    assert anchor2.succeed("cat /run/briard/topology.env").strip() == "BRIARD_TOPOLOGY=alone"
    no_resource(anchor2)
    anchor2.fail("test -e /run/briard/drbd.d/r0.res")
    anchor2.fail("drbdmeta /dev/drbd0 v09 /dev/mapper/briardservice-metadata flex-external dump-md")
    anchor2.succeed("systemctl start briard-chain.target")
    anchor2.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=180)
    alone = ticks(anchor2)
    assert alone >= survived, f"data regressed across the leave: {survived} -> {alone}"
    anchor2.fail("systemctl is-active drbd-reactor.service")
    print(f"the flock ended: the survivor runs alone with no DRBD and its data ({survived} -> {alone})")
  '';
}
