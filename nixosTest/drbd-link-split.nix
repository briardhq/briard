# [DRBD.3] A demote landing during a link split BETWEEN THE TWO ANCHORS forks the pair -- the
# hermetic, briard-free reproduction backing the upstream report.
#
# HISTORY, one line: found as [B.100] (an ha-roll forked the household, ~1-in-13), whose trigger --
# an ARP-flux blackhole of our own single-L2 topology -- was explained and fixed at the source
# ([B.101], guest sysctls). Both are CLOSED. What this file keeps alive is the half that is not
# ours to fix: DRBD's tiebreaker licenses a STALE node to promote when a demote lands during the
# split -- on a plain packet cut, no loss, no asymmetry, no orchestrator. docs/DRBD.md DRBD.3
# (farm) holds the full analysis, the measured windows and the upstream plan; this test is its
# evidence, and once the report is filed it doubles as the PIN-BUMP PROBE: if a later DRBD outdates
# the survivor correctly, this test goes green and the pin can move.
#
# THE FAULT SHAPE is the one the rest of the DRBD net does not cover. Every other test cuts a
# NODE off -- `drbd-witness-loss` blocks the witness, `drbd-fence` isolates the primary,
# `drbd-failover` crashes one -- and `block()`/`crash()` can only express that: a node leaves, so
# whatever remains has a clear majority side. Here nothing leaves. Only the n1↔n2 EDGE fails,
# both anchors keep talking to the witness:
#
#      C             C
#     / \     ->    / \
#    /   \         /   \
#   A --- B       A -x- B
#
# DRBD counts only the two DISKFUL nodes as voters, so on losing each other BOTH anchors fall one
# vote short, and BOTH take the tiebreaker branch in `calc_quorum()`:
#
#   if (!have_quorum && voters != 0 && voters % 2 == 0 && qd.up_to_date + qd.present == quorum_at - 1 &&
#       qd.diskless >= diskless_majority_at && device->quorum[NOW]) { have_quorum = true; }
#
# The only discriminating term is `device->quorum[NOW]` -- the node's real quorum an instant
# earlier -- and until the link broke both anchors had it.
#
# ⚠️ THE DOUBLE QUORUM IS BY DESIGN, NOT THE DEFECT. LINBIT's own `quorum-tiebreaker` test_case1
# is this exact fault and ASSERTS both anchors keep quorum (`forbidden_patterns.add(r'quorum:no')`).
# Their safety property is the next line of that test: the survivor OUTDATES ITSELF
# (`disk( Consistent -> Outdated ) [lost-peer]`), so it cannot promote on stale data. The defect is
# that ONE extra event inverts the property: if the other anchor DEMOTES before the survivor's
# lost-peer evaluation, the survivor sees `primary_nodes=0` and the same code path emits
# `disk( Consistent -> UpToDate )` -- a stale node licensed to take the house. Writes on both sides
# then diverge the generations, and the reconnect ends `Split-Brain detected but unresolved` →
# StandAlone on both, permanently (no `after-sb-*` policy; DRBD 9's default `disconnect` applies --
# deliberate, we never silently discard a side's writes). An earlier revision of this file asserted
# the double-keep itself as a failure; that was [B.100]'s framing and it was wrong -- fact 1 below
# is now a PRECONDITION for the race, not a property violation.
#
# PAIRS WITH `drbd-survivor-restart`, AND IS ITS MIRROR IMAGE. That test is the GAIN side of the
# same guard: a restarted survivor has no runtime quorum state, so `device->quorum[NOW]` is false
# and a diskless node can never GRANT it quorum. This is the KEEP side: when the term is true on
# both anchors at once, the guard grants nothing to nobody. Read together: the tiebreaker is only
# safe when exactly one side was quorate a moment ago -- which a node failure guarantees and a
# LINK failure does not.
#
# TWO ACTS, ONE VARIABLE: WHEN THE DEMOTE LANDS.
#
# ACT 1 -- the control. Cut, wait out both detections, demote seconds LATE. The survivor has long
# outdated itself, `drbdadm primary` refuses with "Need access to UpToDate data", the pair rejoins
# clean. A bare link flap does not fork the pair -- DRBD handles it correctly. Everything stock.
#
# ACT 2 -- the defect. The demote is issued while the pair is split and the survivor is still
# deciding -- which is exactly what a real roll does (`Rollout.System` enqueues a handover, the
# agent runs an evict, the link flaps under it; the evict's demote was in flight ~20s live, and
# the serialization below gives it the same ~20s here).
#
# ⚠️ THE MEASURED PHYSICS (2026-08-19, kernel timestamps, local box + L0; the wrong turns are
# recorded in docs/DRBD.md DRBD.3). "Demote before the survivor detects" DOES NOT EXIST as an
# ordering, and neither side's detection is an independent timer:
#
#   1. The evictor detects on its stock schedule (~10.5s) and its teardown-as-Primary becomes a
#      cluster-wide two-phase commit. The survivor, still believing the link up, answers that
#      prepare by pinging the evictor and HOLDING the commit for its own ping-timeout. Everything
#      serializes behind the hold: the divergence write (protocol C) completes only when the
#      evictor gives up on the peer, and the demote queues in-kernel behind the in-flight
#      teardown commit.
#
#   2. What happens when the hold ends is a pure function of two documented tunables:
#      * survivor ping-timeout << twopc-timeout (STOCK, 500ms vs 30s): the teardown is still
#        valid at expiry; the survivor acks it and its lost-peer evaluation runs INLINE on that
#        commit -- `disk( Consistent -> Outdated ) [far-away]` +1.6ms later, while the demote's
#        first prepare needs a wake-up plus two witness hops, +3.8ms. On an idle rig the safety
#        property holds by ~2ms of SCHEDULING, not design. Live, real hardware inverted the
#        ratio (relay +12.6ms, eval +34.5ms) and the household forked; that ratio is the whole
#        "~1 in 13", and the live "evicting node won by 512ms" was this hold, not ping luck.
#      * survivor ping-timeout > twopc-timeout: the evictor RE-prepares its teardown, the fresh
#        prepare commits at the expiry still carrying primary_nodes=2, and the inline evaluation
#        voids the run deterministically (measured on the L0 box with twopc-timeout=100).
#      * survivor ping-timeout == twopc-timeout == 30s -- ping-timeout's RANGE MAXIMUM equals the
#        twopc default, and this is the one reachable winning shape: both sides abandon the
#        teardown in the same instant (all runs, both boxes: expiry at cut+40.5s), the survivor
#        is forced onto ITS OWN cluster-wide commit (~10-20ms round through the witness), and the
#        demote gets its first fresh prepare slot right then. Landing inside the survivor's round
#        it CONFLICTS, the survivor backs off (`Retrying cluster-wide state change after 87ms`,
#        measured), the demote commits during the backoff, and the same code path emits
#        `disk( Consistent -> UpToDate ) [lost-peer]` -- a stale node licensed to take the house.
#
#   3. The reproducer therefore ORDERS the two expiries instead of racing them:
#      `twopc-timeout=250` (25s; documented range 50-600, default 300) makes the evictor abandon
#      its teardown -- and the queued demote take its first fresh prepare slot -- a full FIVE
#      SECONDS before the survivor's verify expiry. Prepares arriving mid-hold are DEFERRED, not
#      rejected (measured), so the demote's live prepare is already waiting when the survivor's
#      window opens: the abandoned teardown is processed as stale, the demote commits
#      (primary_nodes=0), and the evaluation that follows is the licensed transition. Every
#      margin is seconds; nothing races. (The opposite ordering was measured and fails:
#      twopc-timeout=100 aborts at +10s and the abort CYCLE outruns the hold -- the evictor
#      re-prepares its teardown, the fresh teardown is the live change at the expiry, and the
#      inline evaluation voids the run. The demote must be the live change at the expiry; 25s
#      puts it there with seconds to spare on both sides.)
#
# ⚠️ THE TWO 4K WRITES ARE LOAD-BEARING. DRBD bumps a peerless Primary's current UUID LAZILY, on
# the first write it cannot replicate; that bump IS the divergence. With an unmounted device and
# no I/O it never happens, and the pair rejoins `no-sync by rule=reconnected` even after a wrong
# promotion -- measured, twice. Live, the evictee was serving a mounted btrfs with a running
# payload, so its writes did this for free. Here they have to be asked for.
#
# TWO DRBD KNOBS, BOTH DOCUMENTED TUNABLES INSIDE THEIR LEGAL RANGES, AND THE DECISION LOGIC
# RUNS STOCK CODE ON STOCK INPUTS: the survivor's keepalive (`--ping-int=120 --ping-timeout=300`
# -- a WAN-witness install tunes exactly this) and the resource's `twopc-timeout=250`. A safety
# invariant that only holds for SOME legal timer settings is the defect, not a misconfiguration
# -- every value here is a supported deployment someone runs on purpose. The evictor's keepalive
# is fully stock. An earlier revision also sped the evictor's ping "so it notices first" and
# armed the demote on an in-guest `events2` watcher; both are measured INERT here -- the 30s
# hold gives the demote ~20 SECONDS of slack, so plain driver
# pacing is enough (the watcher's hard-won events2 lore -- `stdbuf -oL`, `connection:` not
# `conn:` -- is preserved in docs/DRBD.md DRBD.3).
#
# ⚠️ THE VOID-RUN GUARD. A run where the demote APPLIED after the survivor's evaluation proves
# nothing -- and looks exactly like a FIXED DRBD, which is fatal for the pin-bump probe. So every
# attempt checks the ORDER in the survivor's own kernel log (the demote's `primary_nodes=0`
# computed/committed vs the evaluation `disk( Consistent -> `; arrival alone is NOT enough, a
# prepare can arrive and be aborted), retries a lost race up to three times, and aborts loudly
# rather than reporting a void as an outcome.
#
# ⚠️ THIS TEST IS EXPECTED TO FAIL on DRBD 9.2.19 -- it asserts the property, not the behaviour
# ([[verification-assertions-must-fail]]). It lives in the `debug` tag for the same reason
# `drbd-survivor-restart` does, stated there: the nightly must stay a statement about what works,
# and a gate that is red for a known reason trains you to stop reading it. It goes green the day
# the pinned DRBD outdates the survivor despite the relayed demote.
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
    imports = [ (h.mkNode { inherit resource; promoter = false; }) ];
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

    # THE CUT IS DELIBERATELY DUMB: DROP rules for the one peer address, both directions. No
    # asymmetry, no loss, no timing -- the wild trigger's one-way blackhole ([B.101]) played no
    # part in the mechanism (any single dead direction breaks BOTH ping round-trips), so the
    # reproducer must not encode it: the report is strongest when the cut is the dumbest possible
    # one.
    #
    # ⚠️ AN `ip route add blackhole` WAS TRIED FIRST AND SILENTLY DID NOTHING: the DRBD sockets
    # are already ESTABLISHED and hold their cached dst, so a more-specific route added afterwards
    # never enters the picture. The nodes stayed Connected and the test sat in the wait below for
    # its full 900s before anyone learned the cut had not happened. Hence both the packet filter
    # and the non-vacuity check under it -- a fault that does not land must fail FAST and say so,
    # or a red test becomes an expensive way to prove nothing ([[verification-assertions-must-fail]]).
    def cut(a, b):
        a.succeed(f"iptables -I INPUT -s {ADDR[b.name]} -j DROP")
        a.succeed(f"iptables -I OUTPUT -d {ADDR[b.name]} -j DROP")

    def heal(a, b):
        a.succeed(f"iptables -D INPUT -s {ADDR[b.name]} -j DROP")
        a.succeed(f"iptables -D OUTPUT -d {ADDR[b.name]} -j DROP")

    # THE FAULT: only the node1↔node2 edge, dropped in BOTH directions on BOTH ends. The witness
    # (10.0.0.3) is untouched and stays reachable from both -- which `block()` cannot express, and
    # which is the entire point.
    cut(node1, node2)
    cut(node2, node1)

    # Both must NOTICE the peer go (independent ping timers -- stock, so up to ~10.5s each).
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

    # ---- FACT 1: both keep quorum off the tiebreaker. BY DESIGN (see header) -- upstream's own
    # test asserts it, so it is the race's PRECONDITION here, not a property violation. If it ever
    # stops holding, the tiebreaker semantics changed upstream, a stale promotion is no longer
    # reachable this way, and the property below holds trivially.
    both_quorate = [m.name for m in disk_nodes if quorate(m)]
    tiebreakers = [m.name for m in disk_nodes
                   if m.execute("dmesg | grep -q 'using tiebreaker logic to keep'")[0] == 0]
    print(f"quorate after the cut: {both_quorate}; used the tiebreaker: {tiebreakers}")
    if len(both_quorate) < 2:
        print("NOTE: the double-keep is GONE -- upstream changed the tiebreaker semantics and the "
              "precondition for this race no longer holds; expect the rest of this test to come "
              "out safe, and re-read docs/DRBD.md DRBD.3 (farm) before trusting anything else here")

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

    # Failures are collected and reported together rather than asserted as they are found: a red
    # test that stops at the first failure hides the fork, and the fork is the harm while the
    # licensed promotion is the cause. Whoever fixes this wants both in one run.
    failures = []
    if promoted:
        # Act 1 is the CONTROL and must stay safe. If a late demote ever starts licensing the
        # survivor too, the window has widened and act 2's ordering is no longer the story.
        failures.append(
            "act 1: node1 promoted on a stale disk even though the demote was seconds late -- "
            "the survivor's self-outdating no longer protects the ordinary case"
        )

    # ================= ACT 2: the same fault with the EVICTION ALREADY IN FLIGHT =================
    # Act 1 demoted seconds late and the pair was fine. Here the demote is issued mid-split and
    # queues in-kernel behind the evictor's own teardown commit, and the knobs below make the
    # hold's end the simultaneous abandon where the demote's fresh prepare conflicts the
    # survivor's own commit -- see the header. Nothing else changes.

    def establish():
        for m in disk_nodes:
            m.wait_until_succeeds("drbdadm cstate r0 | grep -c Connected | grep -q 2", timeout=120)
        node1.wait_until_succeeds("test $(drbdadm dstate r0 | cut -d/ -f1) = UpToDate", timeout=120)
        node2.succeed("test $(drbdadm role r0) = Primary || drbdadm primary r0")

    def first_line(m, pattern):
        # Line number of the first dmesg match (extended regex), 0 if absent. dmesg is cleared per
        # attempt, and line numbers only grow between the two greps, so the comparison is stable.
        out = m.execute(f"dmesg | grep -nE -m1 '{pattern}' | cut -d: -f1")[1].strip()
        return int(out) if out else 0

    outcome = None  # "raced" = defect exercised | "held" = survivor outdated DESPITE the demote
    for attempt in range(1, 4):
        establish()
        # Cleared so the ordering greps below see act 2 of THIS attempt only. Act 1's dmesg
        # evidence (tiebreaker, split-brain) was already collected above.
        for m in disk_nodes:
            m.succeed("dmesg -C")
        # THE ONE DRBD KNOB (header): the survivor's keepalive, pinning it in the 30s hold whose
        # expiry coincides with the evictor's twopc abandon. The evictor runs stock.
        node1.succeed("drbdsetup net-options r0 1 --ping-int=120 --ping-timeout=300")  # peer 1 = node2
        # THE OTHER KNOB (header §3): the evictor's twopc abandons the teardown 5 SECONDS before
        # the survivor's expiry, which is what hands the queued demote its fresh prepare slot
        # while the survivor is still holding. Set on every node so no participant holds a stale
        # prepare longer than the initiator keeps it alive.
        for m in machines:
            m.succeed("drbdsetup resource-options r0 --twopc-timeout=250")
        for m in disk_nodes:
            print(f"{m.name} before act 2 attempt {attempt}: {disks(m)}")

        cut(node1, node2)
        cut(node2, node1)
        # Kernel-clock markers so the dmesg dumps below carry each node's latencies relative to
        # the cut in ITS OWN clock -- the geometry question is all relative timing.
        for m in disk_nodes:
            m.succeed("echo 'DRBDTEST: cut applied' > /dev/kmsg")

        # The evictor notices on its stock schedule (<=10.5s); the survivor is pinned in its 30s
        # hold, so everything from here has ~20 SECONDS of slack -- plain driver pacing is enough.
        wait_peer_lost(node2)
        # WRITE BEFORE DEMOTING -- the 4K write is what makes it a FORK rather than a clean
        # handover (the lazy UUID bump, header); it completes locally at once because node2 has
        # already declared the peer gone. The demote then BLOCKS IN-KERNEL for ~20s, queued
        # behind node2's own teardown twopc, and gets its first fresh prepare slot at the
        # simultaneous abandon -- which is the entire trick.
        node2.succeed("dd if=/dev/urandom of=/dev/drbd0 bs=4k count=1 oflag=direct 2>/dev/null"
                      " && echo 'DRBDTEST: demote issued' > /dev/kmsg"
                      " && drbdsetup secondary r0"
                      " && echo 'DRBDTEST: demote returned' > /dev/kmsg")
        print(f"attempt {attempt}: node2 demoted; {disks(node2)}")
        wait_peer_lost(node1)
        decision = None
        for _ in range(30):
            d = status(node1)["devices"][0]["disk-state"]
            if d in ("UpToDate", "Outdated"):
                decision = d
                break
            time.sleep(1)

        # THE ORDER, from the survivor's own kernel log: did the demote APPLY before node1's
        # lost-peer evaluation? Without this check a lost race is indistinguishable from a FIXED
        # DRBD (see the void-run guard in the header). Two markers, both read out of real runs'
        # dmesg, not guessed: the demote's prepare ARRIVING (`1->all role( Secondary )`; node-id 1
        # is node2, the initiator) and the demote APPLYING (`primary_nodes=0` computed or
        # committed). ⚠️ Arrival alone is NOT enough -- a prepare can arrive and be ABORTED while
        # the survivor holds (seen on the L0 box), and a run classified on arrival would report a
        # rejected demote as "property held".
        demote_seen = first_line(node1, r"1->all role\( Secondary \)|peer\( Primary -> Secondary \)")
        demote_applied = first_line(node1, r"State change [0-9]+: primary_nodes=0|Committing remote state change.*\(primary_nodes=0\)")
        eval_ln = first_line(node1, r"disk\( Consistent -> ")
        print(f"attempt {attempt}: node1 decided {decision}; demote seen@dmesg:{demote_seen} "
              f"applied@dmesg:{demote_applied} eval@dmesg:{eval_ln}")
        print("node2 drbd dmesg:\n" + node2.execute("dmesg | grep -E 'drbd|DRBDTEST' | tail -45")[1])
        print("node1 drbd dmesg:\n" + node1.execute("dmesg | grep -E 'drbd|DRBDTEST' | head -60")[1])

        if decision == "UpToDate":
            # A licensed stale node REQUIRES primary_nodes=0, i.e. the demote applied; if the
            # grep did not see it, the pattern has gone stale, not the mechanism.
            if not (0 < demote_applied < eval_ln):
                print("NOTE: node1 went UpToDate but the dmesg ordering greps disagree "
                      "(patterns stale?) -- trusting the state, fix the patterns")
            outcome = "raced"
            break
        if decision == "Outdated" and 0 < demote_applied < eval_ln:
            outcome = "held"
            break
        print(f"attempt {attempt} VOID: node1 evaluated before the demote applied -- "
              f"healing and retrying")
        # Put node1's ping timers back BEFORE healing: a 30s ping-timeout makes the reconnect
        # handshake -- the thing the next establish() waits for -- take longer than its wait.
        node1.succeed("drbdsetup net-options r0 1 --ping-int=10 --ping-timeout=5")
        heal(node1, node2)
        heal(node2, node1)

    if outcome is None:
        raise Exception(
            "act 2 was void 3 times running: the demote never reached the survivor before its "
            "lost-peer evaluation -- the margins have moved and this rig currently proves nothing"
        )

    print(f"node1 after the demote-first cut: {disks(node1)}")
    if outcome == "raced":
        # Did the survivor come out UpToDate (licensed to promote -> fork) or Outdated
        # (disqualified)? This one transition IS the defect; everything else follows from it.
        raced_promote = node1.execute("drbdadm primary r0")[0] == 0
        print(f"node1 promoted after the raced demote: {raced_promote}; state {disks(node1)}")
        if raced_promote:
            # ...and the OTHER half of the divergence. node2 started a new generation from its own
            # unreplicable write; node1 must start one too, or there is only one fork and DRBD
            # would simply resync the stale side. This is the write the household's service would
            # have been doing all along -- the moment the wrongly-promoted node serves anything,
            # this is what it does.
            node1.succeed("dd if=/dev/urandom of=/dev/drbd0 bs=4k count=1 oflag=direct")
            print(f"node1 wrote as the wrongly-promoted primary; state {disks(node1)}")
            # THE SAFETY FAILURE, stated as such. A node whose disk was `Consistent` -- stale,
            # peer unreachable -- was licensed to call itself authoritative and take the house.
            # The fork is what follows when there are writes to diverge; this is the step that
            # permits it.
            failures.append(
                "a STALE node promoted while its peer was unreachable: node1 went "
                "disk( Consistent -> UpToDate ) [lost-peer] because the demote beat its "
                "self-outdating, then took the house -- the step every fork of this pair goes through"
            )
    else:
        print("the survivor outdated itself DESPITE the relayed demote -- the [lost-peer] "
              "decision no longer takes primary_nodes=0 as a licence. If the DRBD pin moved, "
              "this answers the pin-bump question in docs/DRBD.md DRBD.3 (farm)")

    # Put node1's ping timers back before healing (same reason as in the void path): the skew is
    # for the FAULT, and a 30s ping-timeout makes the reconnect handshake -- the thing that
    # detects the fork -- take longer than the settle wait. Measured: without this the pair was
    # still Connecting when the wait gave up, so a real split-brain would have gone unrecorded.
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
    assert not failures, "[DRBD.3] " + "; ".join(failures)
  '';
}
