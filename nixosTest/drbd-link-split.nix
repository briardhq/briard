# [B.100] A broken link BETWEEN THE TWO ANCHORS, with the witness still reachable from both.
#
# This is the one fault shape the rest of the DRBD net does not cover, and it is the shape that
# forks the household's data. Every other test cuts a NODE off — `drbd-witness-loss` blocks the
# witness, `drbd-fence` isolates the primary, `drbd-failover` crashes one — and `block()`/`crash()`
# can only express that: a node leaves the cluster, so whatever remains is a partition with a
# clear majority side. Here nothing leaves. Only the n1↔n2 EDGE fails, both anchors keep talking
# to the witness, and that is what the tiebreaker cannot survive.
#
# WHY IT MATTERS, in one line: DRBD counts only the two DISKFUL nodes as voters
# (`1 of 2 nodes visible, need 2 for quorum`), so on losing each other BOTH anchors fall one vote
# short, and BOTH then take the tiebreaker branch in `calc_quorum()`:
#
#   if (!have_quorum && voters != 0 && voters % 2 == 0 && qd.up_to_date + qd.present == quorum_at - 1 &&
#       qd.diskless >= diskless_majority_at && device->quorum[NOW]) { have_quorum = true; }
#
# The only discriminating term is `device->quorum[NOW]` — the node's REAL quorum an instant
# earlier — and until the link broke both anchors had it. So both keep quorum, both may write,
# and a fork follows the moment either of them bumps a current UUID. Measured live on the fleet
# 2026-08-15: n2 forks 12ms after losing the peer, n1 forks 1.6s later on taking over, and the
# reconnect ends in `Split-Brain detected but unresolved` → StandAlone, which is permanent
# (no `after-sb-*` policy, so DRBD 9's default `disconnect` applies).
#
# PAIRS WITH `drbd-survivor-restart`, AND IS ITS MIRROR IMAGE. That test is the GAIN side of the
# same guard: a survivor that restarts has no runtime quorum state, so `device->quorum[NOW]` is
# false and it can never gain quorum from a diskless node — "a diskless witness can KEEP quorum
# but never GRANT it". This is the KEEP side, and the same term read the other way: when the term
# is true on BOTH anchors at once, both keep quorum and the guard grants nothing to nobody. Read
# together they say the tiebreaker is only ever safe when exactly one side was quorate a moment
# ago — which a node failure guarantees and a LINK failure does not.
#
# WHAT IT PROVES, AND WHAT IT DELIBERATELY DOES NOT. It reproduces the ROOT CAUSE — both anchors
# quorate at once off one diskless tiebreaker — deterministically, in ~30s. It does NOT reach the
# FORK, and that is a finding rather than a shortfall: driven at test speed, the surviving node
# OUTDATES ITSELF (`[far-away]`, the standard response to an unreachable Primary) and is then
# disqualified — `drbdadm primary` refuses with *"Need access to UpToDate data"*, the pair rejoins
# Connected, and nothing forks. **A bare link flap therefore does not fork the pair.**
#
# The live fork needed the demote to land INSIDE a millisecond-wide window. Measured from the
# 2026-08-15 capture, relative to n1 noticing the link was gone: `UpToDate -> Consistent` at
# +3.3ms, n2's demote arriving via the witness at **+12.6ms**, `primary_nodes=0` at +16.5ms, and
# `Consistent -> UpToDate [lost-peer]` at +34.5ms. The demote beat the self-outdating, so the
# stale node was licensed to call itself authoritative. Wait even a second, as this test does, and
# the window has closed. **That is why B.100 only ever appears during a ROLL:** a planned handover
# is the one thing that issues a demote in the instant a link is flapping.
#
# So the fork half wants a variant that races the demote against the survivor's self-outdating
# (a watcher on the primary that demotes the moment it sees the peer go). Left unwritten rather
# than half-written: the value is as the acceptance test for whichever fix is chosen, and what
# that fix is has not been decided.
#
# ⚠️ THIS TEST IS EXPECTED TO FAIL until B.100 is fixed — it asserts the property, not the
# behaviour ([[verification-assertions-must-fail]]). It lives in the `debug` tag for the same
# reason `drbd-survivor-restart` does, stated there: the nightly must stay a statement about what
# works, and a gate that is red for a known reason trains you to stop reading it. What it buys is
# that reproducing B.100 stops costing a 13-minute loop at 1-in-13 odds and becomes a hermetic run
# of a couple of minutes, with no nested KVM, no guest, no OS upgrade and no rollout — none of
# which turned out to be load-bearing, which is itself the finding.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  # iptables is NOT in the shipped guest's path (`networking.firewall.enable = false`), and this
  # test needs to drop packets for one peer only. Added here rather than in lib.nix: no other
  # topology cuts an edge, and the shipped image should not grow a tool for a test's benefit.
  diskNode = {
    imports = [ (h.mkNode { inherit resource; }) ];
    environment.systemPackages = [ pkgs.iptables ];
  };
  witnessNode = h.mkNode {
    inherit resource;
    diskless = true;
    promoter = false;
  };
in
pkgs.testers.runNixOSTest {
  name = "drbd-link-split";

  # Dynamic node lists + json parsing off `execute`, as in the rest of the DRBD net.
  skipTypeCheck = true;

  nodes = {
    node1 = diskNode;
    node2 = diskNode;
    witness = witnessNode;
  };

  testScript = ''
    import json
    import time

    disk_nodes = [node1, node2]
    machines = [node1, node2, witness]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
    for m in disk_nodes:
        m.succeed("drbdadm create-md --force r0")
    for m in machines:
        m.succeed("systemctl start drbd@r0.target")

    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")

    def status(m):
        return json.loads(m.succeed("drbdsetup status r0 --json"))[0]

    def quorate(m):
        return status(m)["devices"][0]["quorum"]

    # drbd-reactor is never started: WHO elects the primary is not what is under test, and
    # driving the roles by hand keeps the fault the only moving part. node2 serves, as it does
    # at the point in an `ha-roll` where this was first measured.
    node2.succeed("drbdadm primary r0")
    for m in disk_nodes:
        assert quorate(m), f"{m.name} is not quorate before the fault -- the rig is wrong, not the product"

    # Reachability is read from DRBD'S OWN per-connection state rather than a side-channel probe.
    # ⚠️ A `/dev/tcp/<peer>/7789` probe was tried first and is WORSE THAN USELESS HERE: DRBD closes
    # its listening socket once its peers are connected, so the probe fails for EVERY peer whether
    # or not the link is cut -- which made "the cut landed" pass vacuously and "the witness
    # survived" fail spuriously. The connection map is what the test is actually about, it comes
    # from the same JSON already parsed above, and it needs no tooling in the image.
    def conn_states(m):
        return {c["name"]: c["connection-state"] for c in status(m)["connections"]}

    ADDR = {"node1": "10.0.0.1", "node2": "10.0.0.2", "witness": "10.0.0.3"}

    def cut(a, b):
        a.succeed(f"iptables -I INPUT -s {ADDR[b.name]} -j DROP")
        a.succeed(f"iptables -I OUTPUT -d {ADDR[b.name]} -j DROP")

    def heal(a, b):
        a.succeed(f"iptables -D INPUT -s {ADDR[b.name]} -j DROP")
        a.succeed(f"iptables -D OUTPUT -d {ADDR[b.name]} -j DROP")

    # THE FAULT: only the node1↔node2 edge, dropped in BOTH directions on BOTH ends. The witness
    # (10.0.0.3) is untouched and stays reachable from both -- which `block()` cannot express, and
    # which is the entire point.
    #
    # ⚠️ AN `ip route add blackhole` WAS TRIED FIRST AND SILENTLY DID NOTHING: the DRBD sockets
    # are already ESTABLISHED and hold their cached dst, so a more-specific route added afterwards
    # never enters the picture. The nodes stayed Connected and the test sat in the wait below for
    # its full 900s before anyone learned the cut had not happened. Hence both the packet filter
    # and the non-vacuity check under it -- a fault that does not land must fail FAST and say so,
    # or a red test becomes an expensive way to prove nothing ([[verification-assertions-must-fail]]).
    cut(node1, node2)
    cut(node2, node1)

    # Both must NOTICE the peer go (independent PingAck timers, ~0.5s apart when measured live).
    # Short timeout: if the link is cut and DRBD has not reacted within ~a minute, something else
    # is wrong and waiting fifteen more will not reveal it.
    #
    # ⚠️ POLLED THROUGH THE PARSED JSON, NEVER GREPPED. `drbdsetup --json` pretty-prints, so a
    # `grep '"connection-state":"Connecting"'` matches nothing -- the space after the colon is the
    # whole bug, and it cost this test a third 90s timeout after the blackhole route and the
    # /dev/tcp probe. There is exactly one reader of this JSON now, and it is `json.loads`.
    peer_of = {"node1": "node2", "node2": "node1"}

    def wait_peer_lost(m, timeout=90):
        for _ in range(timeout):
            if conn_states(m).get(peer_of[m.name]) != "Connected":
                return
            time.sleep(1)
        raise Exception(f"the cut did not land: {m.name} still sees its peer -- {conn_states(m)}")

    for m in disk_nodes:
        wait_peer_lost(m)
        print(f"{m.name} connections after the cut: {conn_states(m)}")

    # NON-VACUITY, both halves, in DRBD's own terms: the anchors have lost EACH OTHER (above)...
    for m in disk_nodes:
        st = conn_states(m)
        # ...and BOTH still hold the witness. If they did not, this would be an ordinary partition
        # with a clean majority side, the tiebreaker would be doing exactly its job, and the test
        # would prove nothing at all.
        assert st["witness"] == "Connected", (
            f"{m.name} lost the witness too ({st}) -- that is a partition, not a link split"
        )

    # ---- FACT 1: did both keep quorum? (the root defect) ----
    both_quorate = [m.name for m in disk_nodes if quorate(m)]
    tiebreakers = [m.name for m in disk_nodes
                   if m.execute("dmesg | grep -q 'using tiebreaker logic to keep'")[0] == 0]
    print(f"quorate after the cut: {both_quorate}; used the tiebreaker: {tiebreakers}")

    # Drive the rest of the measured sequence: the serving node lets go, the other takes over.
    # Neither step is exotic -- it is an ordinary planned handover, which is exactly the point.
    node2.succeed("drbdadm secondary r0")

    def disks(m):
        s = status(m)
        return {
            "role": s["role"],
            "disk": s["devices"][0]["disk-state"],
            "quorum": s["devices"][0]["quorum"],
            "peers": {c["name"]: c["peer_devices"][0].get("peer-disk-state") for c in s["connections"]},
        }

    for m in disk_nodes:
        print(f"{m.name} after the demote: {disks(m)}")

    # BOUNDED, and a FACT rather than a precondition. Measured live, n1 went
    # `disk( Consistent -> UpToDate ) [lost-peer]` 31ms after the demote landed and promoted a
    # second later. If it will NOT promote here, DRBD is refusing to let a stale node take the
    # house -- which is the protection working, and is the single most interesting thing this test
    # can report. It must not be spent as a 900s `wait_until_succeeds` that aborts the run before
    # the fork question is even asked.
    promoted = False
    for _ in range(60):
        if node1.execute("drbdadm primary r0")[0] == 0:
            promoted = True
            break
        time.sleep(1)
    print(f"node1 promoted while its peer was unreachable: {promoted}; state now {disks(node1)}")

    # ...and the link comes back.
    heal(node1, node2)
    heal(node2, node1)

    # ---- FACT 2: did the pair come back, or fork? ----
    # Give the handshake time to complete either way, then read what it decided. A pair that
    # reconnects lands Connected; a forked one lands StandAlone and stays there.
    def wait_settled(m, timeout=120):
        for _ in range(timeout):
            if conn_states(m).get(peer_of[m.name]) in ("Connected", "StandAlone"):
                return conn_states(m)[peer_of[m.name]]
            time.sleep(1)
        return conn_states(m).get(peer_of[m.name])

    forked = []
    for m in disk_nodes:
        settled = wait_settled(m)
        print(f"{m.name} settled on {settled} after the link returned; connections: {conn_states(m)}")
        if m.execute("dmesg | grep -q 'Split-Brain detected but unresolved'")[0] == 0:
            forked.append(m.name)

    # Both facts are reported together rather than asserted as they are found: a red test that
    # stops at the first failure hides whether the fork happened, and the fork is the harm while
    # the double quorum is the cause. Whoever fixes this wants both in one run.
    failures = []
    if len(both_quorate) > 1:
        failures.append(
            f"BOTH anchors kept quorum on a single-link failure ({both_quorate}, tiebreaker: {tiebreakers}) "
            "-- exactly one may, or both may write and the data forks"
        )
    if not promoted:
        # NOT a failure -- the opposite. Recorded so a future green run cannot be misread as "the
        # fork was fixed" when what actually happened is that this scenario never reached a fork.
        print("NOTE: node1 refused to promote on a Consistent disk ('Need access to UpToDate data'). "
              "The double quorum above is real, but THIS sequence does not reach a fork -- the live "
              "B.100 run did, so something else in it let the stale node become UpToDate.")
    if forked:
        failures.append(
            f"the pair forked and will not re-join: split-brain on {forked}, left StandAlone "
            "-- permanent, since no after-sb-* policy is configured"
        )
    assert not failures, "[B.100] " + "; ".join(failures)
  '';
}
