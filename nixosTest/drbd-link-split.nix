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
# TWO ACTS, AND WHAT EACH ONE ACTUALLY PROVES.
#
# ACT 1 — the control, and the ROOT CAUSE. Cut the edge, wait, then demote. Both anchors keep
# quorum off the one diskless tiebreaker (asserted; red today, and that assertion IS B.100). But
# the survivor OUTDATES ITSELF ~18ms after noticing (`[far-away]` / `[lost-peer]`), so
# `drbdadm primary` refuses with *"Need access to UpToDate data"* and the pair rejoins clean.
# **A bare link flap does not fork the pair** — DRBD handles it correctly.
#
# ACT 2 — the roll's timing, which is what makes it dangerous, THROUGH TO THE FORK. The eviction is
# armed on DRBD's own event stream so it fires while the survivor is still deciding. The full chain,
# every link of it asserted:
#
#   1. both anchors keep quorum off the one diskless tiebreaker;
#   2. the evicting node writes while peerless-Primary and so starts a new generation;
#   3. the survivor goes `disk( Consistent -> UpToDate ) [lost-peer]` on STALE data and PROMOTES
#      (+42.6ms measured, against the live capture's +34.5ms);
#   4. it writes too, and the generations diverge;
#   5. the link returns → `Split-Brain detected but unresolved` → **StandAlone on both, permanently**,
#      because no `after-sb-*` policy is configured.
#
# ⚠️ THE TWO 4K WRITES ARE LOAD-BEARING, and were the last thing to be understood. DRBD bumps a
# peerless Primary's current UUID **lazily**, on the first write it cannot replicate — that bump IS
# the divergence. With an unmounted device and no I/O it never happens, and the pair rejoins
# `no-sync by rule=reconnected` even though the survivor has already wrongly promoted (measured,
# twice, before the writes were added). Live, n2 was serving a mounted btrfs with a running payload
# for 527ms, so its writes did this for free.
#
# THREE THINGS THE TIMING WORK MEASURED, which outlive this file:
#   * the survivor's decision window is ~20–43ms from its own detection of the link loss;
#   * **the ~1-in-13 rate is a PING-PHASE COIN FLIP** — DRBD pings each peer every `ping-int` with
#     independent phase, so which anchor notices first, and by how much, is luck. Live, n2 won by
#     512ms. With an identical cut both notice ~4ms apart and the survivor always wins, which is
#     why every un-skewed attempt came out safe. Act 2 sets the skew with `net-options` rather than
#     waiting for it;
#   * the demote must be LATE ENOUGH to diverge and EARLY ENOUGH to beat the decision. Demote
#     within ~13ms of noticing and there is nothing to fork even though the survivor still wrongly
#     promotes. Our own evict path (cloud directive → agent poll → `drbd-reactorctl`) is slow, and
#     that latency is what puts a real roll's demote squarely in the dangerous zone.
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
  # `promoter = false; payload = false` -- NOT defaults-by-accident. drbd-reactor is never started
  # here (the roles are driven by hand precisely so the election is not a variable), so the
  # promoter snippet would configure a daemon that never runs, and `payload` would drag the dummy
  # OCI image into a build for a test that never launches a container.
  diskNode = {
    imports = [ (h.mkNode { inherit resource; promoter = false; payload = false; }) ];
    environment.systemPackages = [ pkgs.iptables ];
  };
  witnessNode = h.mkNode {
    inherit resource;
    diskless = true;
    promoter = false;
  };

  # THE EVICTION, ARMED. It must fire from INSIDE the guest: the window between the survivor
  # noticing the peer is gone and disqualifying itself is ~18ms (measured -- see the header), and a
  # command round-tripped through the test driver's console takes an order of magnitude longer. So
  # this waits on `drbdsetup events2`, the same event stream drbd-reactor itself consumes, and
  # demotes the instant the connection breaks.
  #
  # This is a faithful stand-in for what the roll actually does, not a contrivance: `Rollout.System`
  # enqueues a handover, the agent runs an evict, and the demote is therefore ALREADY IN FLIGHT
  # when a link flaps under it. Measured live, the evict landed 5s after the link came back and
  # took 20s to apply. Arming it on the event makes deterministic what is otherwise 1-in-13.
  # ⚠️ `stdbuf -oL` IS LOad-BEARING, not tidiness: `drbdsetup events2` writes to a PIPE, so libc
  # block-buffers it at 4KB and the events sit unseen until the buffer fills -- which, for a
  # handful of state changes, is never. The first version of this watcher armed correctly, saw the
  # link break, and demoted nothing.
  #
  # The key is `connection:`, NOT `conn:` -- read out of drbd-utils 9.33.0
  # (`user/v9/drbdsetup_events2.c:897`) rather than guessed, after a matcher on `conn:` silently
  # matched nothing and the armed eviction never fired. A line reads:
  #
  #   change connection name:r0 peer-node-id:1 conn-name:node1 connection:Connecting role:Unknown
  #
  # `conn-name:` lets this watch ONE peer, so the witness connection cannot trigger it. Every event
  # is logged: the next person to touch this should not have to re-derive the format either.
  raceDemote = pkgs.writeShellScript "race-demote" ''
    export PATH=/run/current-system/sw/bin:$PATH
    exec >>/tmp/race-demote.log 2>&1
    echo "armed $(date +%s.%N)"
    stdbuf -oL drbdsetup events2 r0 | while read -r l; do
      echo "ev: $l"          # NO `date` here: a fork per event cost 7.2ms of the budget below
      case "$l" in
        *conn-name:node1*connection:Connected*) ;;
        *conn-name:node1*connection:*)
          # ⚠️ WRITE BEFORE DEMOTING -- this single 4K write is what makes it a FORK rather than a
          # clean handover. DRBD bumps a peerless Primary's new current UUID LAZILY, on the first
          # write that cannot be replicated; that bump IS the divergence. With an unmounted device
          # and no I/O the bump never happens, and the pair rejoins `no-sync by rule=reconnected`
          # even though the survivor has already wrongly promoted -- measured, twice.
          #
          # Live, n2 was serving a mounted btrfs with a running payload for 527ms before it
          # demoted, so its writes did this for free. Here it has to be asked for.
          #
          # `oflag=direct` so it reaches DRBD instead of sitting in the page cache, and one block
          # so it costs ~1ms: the demote still has to commit before the survivor decides, ~20-43ms
          # after ITS detection.
          dd if=/dev/urandom of=/dev/drbd0 bs=4k count=1 oflag=direct 2>/dev/null
          # DEMOTE, THEN LOG -- nothing forks between here and the demote. `drbdsetup`, not
          # `drbdadm`: drbdadm re-parses drbd.conf every invocation and cost 18.7ms.
          drbdsetup secondary r0
          rc=$?
          echo "demote rc=$rc $(date +%s.%N) on: $l"
          exit 0
          ;;
      esac
    done
  '';
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

    # ACT 1's demote is LATE ON PURPOSE -- seconds, not milliseconds. It is the control: with the
    # window missed, the survivor has already disqualified itself and the pair must rejoin cleanly.
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
    if promoted:
        # Act 1 is the CONTROL and must stay safe. If a late demote ever starts licensing the
        # survivor too, the window has widened and act 2's careful timing is no longer the story.
        failures.append(
            "act 1: node1 promoted on a stale disk even though the demote was seconds late -- "
            "the survivor's self-outdating no longer protects the ordinary case"
        )
    # ================= ACT 2: the same fault with the EVICTION ALREADY IN FLIGHT =================
    # Act 1 missed the window by seconds and the pair was fine. This is the roll's timing: the
    # demote is armed on DRBD's own event stream, so it fires the instant the link breaks -- inside
    # the ~18ms before the survivor disqualifies itself. Nothing else changes.
    for m in disk_nodes:
        m.wait_until_succeeds("drbdadm cstate r0 | grep -c Connected | grep -q 2", timeout=120)
    node1.wait_until_succeeds("test $(drbdadm dstate r0 | cut -d/ -f1) = UpToDate", timeout=120)
    node2.succeed("drbdadm primary r0")
    for m in disk_nodes:
        print(f"{m.name} before act 2: {disks(m)}")

    # ⚠️ BIAS WHICH ANCHOR NOTICES FIRST. This is what makes the fork reachable, and it is why
    # B.100 is ~1-in-13 in the wild rather than every time. DRBD decides a link is dead by ping
    # timeout, per connection per node, so which anchor notices first depends on where each one
    # happens to be in its own ping cycle. The fork needs the EVICTING node to notice first and get
    # its demote committed before the survivor's `[lost-peer]` decision. Live, n2 won by 512ms.
    #
    # MEASURED, and the ordering is the whole of it:
    #   without these options -- node1 noticed FIRST (node2 5.5ms later), the demote arrived at
    #     +45ms, node1 had already decided at +18.6ms -> Outdated -> safe;
    #   with them            -- node2 noticed first, demote committed at +16.7ms, node1 decided at
    #     +42.6ms -> UpToDate -> promotes -> fork.
    #
    # ⚠️ AND THE HALF THAT DOES NOT WORK, so nobody trusts it: node1's slow ping does NOT reliably
    # delay its detection. It still logged `PingAck did not arrive in time` promptly, because a
    # ping was already in flight when the cut landed and it timed out on the old schedule. What
    # does the work is speeding the EVICTOR up, so its demote is already moving when the survivor
    # starts deciding. This biases the order; it does not dictate it -- which is also why the
    # margin, not the mechanism, is what to check if this ever goes flaky.
    node2.succeed("drbdsetup net-options r0 0 --ping-int=1 --ping-timeout=1")    # peer 0 = node1
    node1.succeed("drbdsetup net-options r0 1 --ping-int=120 --ping-timeout=300")  # peer 1 = node2

    node2.succeed("systemd-run --unit=race-demote ${raceDemote}")
    cut(node1, node2)
    cut(node2, node1)

    for m in disk_nodes:
        wait_peer_lost(m)

    # BOUNDED, and the watcher's own log is printed either way -- an armed eviction that did not
    # fire is indistinguishable from one that fired and lost, and only the timestamps tell them
    # apart. (The first version silently never fired: see the stdbuf note on the script.)
    raced = False
    for _ in range(60):
        if node2.execute("test $(drbdadm role r0) = Secondary")[0] == 0:
            raced = True
            break
        time.sleep(1)
    print("race-demote log:\n" + node2.execute("cat /tmp/race-demote.log")[1])
    print(f"the armed eviction demoted node2: {raced}")

    # Did the survivor come out UpToDate (licensed to promote -> fork) or Outdated (disqualified)?
    # This one transition IS the defect; everything else follows from it.
    print(f"node1 after the RACED demote: {disks(node1)}")
    raced_promote = node1.execute("drbdadm primary r0")[0] == 0
    print(f"node1 promoted after the raced demote: {raced_promote}; state {disks(node1)}")
    # ...and the OTHER half of the divergence. node2 started a new generation from its own
    # unreplicable write; node1 must start one too, or there is only one fork and DRBD would simply
    # resync the stale side. This is the write the household's service would have been doing all
    # along -- the moment the wrongly-promoted node serves anything, this is what it does.
    if raced_promote:
        node1.succeed("dd if=/dev/urandom of=/dev/drbd0 bs=4k count=1 oflag=direct")
        print(f"node1 wrote as the wrongly-promoted primary; state {disks(node1)}")

    if raced_promote:
        # THE SAFETY FAILURE, stated as such. A node whose disk was `Consistent` -- stale, peer
        # unreachable -- was licensed to call itself authoritative and take the house. The fork is
        # what follows when there are writes to diverge; this is the step that permits it.
        failures.append(
            "a STALE node promoted while its peer was unreachable: node1 went "
            "disk( Consistent -> UpToDate ) [lost-peer] because the demote beat its self-outdating, "
            "then took the house -- the step every fork of this pair goes through"
        )

    # Put node1's ping timers back before healing: the skew above is for the FAULT, and leaving a
    # 30s ping-timeout in place makes the reconnect handshake -- the thing that detects the fork --
    # take longer than the settle wait. Measured: without this the pair was still Connecting when
    # the wait gave up, so a real split-brain would have gone unrecorded.
    node1.succeed("drbdsetup net-options r0 1 --ping-int=10 --ping-timeout=5")
    heal(node1, node2)
    heal(node2, node1)
    for m in disk_nodes:
        settled = wait_settled(m, timeout=240)
        print(f"{m.name} settled on {settled} after act 2; connections: {conn_states(m)}")
        if m.name not in forked and m.execute("dmesg | grep -q 'Split-Brain detected but unresolved'")[0] == 0:
            forked.append(m.name)

    if forked:
        failures.append(
            f"the pair forked and will not re-join: split-brain on {forked}, left StandAlone "
            "-- permanent, since no after-sb-* policy is configured"
        )
    assert not failures, "[B.100] " + "; ".join(failures)
  '';
}
