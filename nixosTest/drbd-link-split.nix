# A demote landing during a link split between the two anchors forks the pair: the hermetic,
# briard-free reproduction backing the upstream report, and the half of a system-roll fork that is
# not ours to fix. DRBD's tiebreaker licenses a stale node to promote when a demote lands during
# the split, on a plain packet cut (no loss, no asymmetry, no orchestrator). The full analysis,
# measured windows and upstream plan live with the upstream report; this test is their evidence
# and doubles as the pin-bump probe: if a later DRBD outdates the survivor correctly, it goes green
# and the pin can move.
#
# The fault shape is the one the rest of the DRBD net does not cover. Every other test cuts a node
# off (`drbd-witness-loss` blocks the witness, `drbd-fence` isolates the primary, `drbd-failover`
# crashes one), which is all `block()`/`crash()` can express, so whatever remains has a clear
# majority side. Here nothing leaves: only the n1↔n2 edge fails, and both anchors keep talking to
# the witness:
#
#      C             C
#     / \     ->    / \
#    /   \         /   \
#   A --- B       A -x- B
#
# DRBD counts only the two diskful nodes as voters, so on losing each other both anchors fall one
# vote short and both take the tiebreaker branch in `calc_quorum()`:
#
#   if (!have_quorum && voters != 0 && voters % 2 == 0 && qd.up_to_date + qd.present == quorum_at - 1 &&
#       qd.diskless >= diskless_majority_at && device->quorum[NOW]) { have_quorum = true; }
#
# The only discriminating term is `device->quorum[NOW]` (the node's real quorum an instant
# earlier), and until the link broke both anchors had it.
#
# ⚠️ The double quorum is by design, not the defect. LINBIT's own `quorum-tiebreaker` test_case1 is
# this exact fault and asserts both anchors keep quorum (`forbidden_patterns.add(r'quorum:no')`);
# their safety property is that test's next line: the survivor outdates itself
# (`disk( Consistent -> Outdated ) [lost-peer]`), so it cannot promote on stale data. The defect:
# one extra event inverts the property. If the other anchor demotes before the survivor's lost-peer
# evaluation, the survivor sees `primary_nodes=0` and the same code path emits
# `disk( Consistent -> UpToDate )`, a stale node licensed to take the house. Writes on both sides
# then diverge the generations, and the reconnect ends `Split-Brain detected but unresolved` →
# StandAlone on both, permanently (no `after-sb-*` policy, so DRBD 9's default `disconnect`
# applies: deliberate, we never silently discard a side's writes). So fact 1 below (the
# double-keep) is a precondition for the race, not a property violation.
#
# Pairs with `drbd-survivor-restart` as its mirror image. That test is the gain side of the same
# guard: a restarted survivor has no runtime quorum state, so `device->quorum[NOW]` is false and a
# diskless node can never grant it quorum. This is the keep side: with the term true on both
# anchors at once, the guard refuses nobody. The tiebreaker is safe only when exactly one side was
# quorate a moment ago, which a node failure guarantees and a link failure does not.
#
# Two acts, one variable: when the demote lands.
#
# Act 1, the control: cut, wait out both detections, demote seconds late. The survivor has long
# outdated itself, `drbdadm primary` refuses with "Need access to UpToDate data", and the pair
# rejoins clean: a bare link flap does not fork the pair. Everything stock.
#
# Act 2, the defect: the demote is issued while the pair is split and the survivor is still
# deciding, exactly what a real system roll does (it hands the house over, the agent runs an
# evict, the link flaps under it; the evict's demote was in flight ~20s live, and the
# serialization below gives it the same ~20s here).
#
# ⚠️ The measured physics (kernel timestamps). "Demote before the survivor detects" does not exist
# as an ordering, and neither side's detection is an independent timer:
#
#   1. The evictor detects on its stock schedule (~10.5s) and its teardown-as-Primary becomes a
#      cluster-wide two-phase commit. The survivor, still believing the link up, answers that
#      prepare by pinging the evictor and holding the commit for its own ping-timeout. Everything
#      serializes behind the hold: the divergence write (protocol C) completes only when the
#      evictor gives up on the peer, and the demote queues in-kernel behind the in-flight teardown.
#
#   2. What happens when the hold ends is a pure function of two documented tunables:
#      * survivor ping-timeout << twopc-timeout (stock, 500ms vs 30s): the teardown is still valid
#        at expiry; the survivor acks it and its lost-peer evaluation runs inline on that commit,
#        `disk( Consistent -> Outdated ) [far-away]` +1.6ms later, while the demote's first
#        prepare needs a wake-up plus two witness hops, +3.8ms. On an idle rig the safety property
#        holds by ~2ms of scheduling, not design; real hardware can invert the ratio (relay
#        +12.6ms, eval +34.5ms), and then the pair forks.
#      * survivor ping-timeout > twopc-timeout: the evictor re-prepares its teardown, the fresh
#        prepare commits at the expiry still carrying primary_nodes=2, and the inline evaluation
#        voids the run deterministically (twopc-timeout=100).
#      * survivor ping-timeout == twopc-timeout == 30s (ping-timeout's range maximum equals the
#        twopc default), the one reachable winning shape: both sides abandon the teardown in the
#        same instant (expiry at cut+40.5s), the survivor is forced onto its own cluster-wide
#        commit (~10-20ms round through the witness), and the demote gets its first fresh prepare
#        slot right then. Landing inside the survivor's round it conflicts, the survivor backs off
#        (`Retrying cluster-wide state change after 87ms`, measured), the demote commits during
#        the backoff, and the same code path emits `disk( Consistent -> UpToDate ) [lost-peer]`.
#
#   3. The reproducer therefore orders the two expiries instead of racing them:
#      `twopc-timeout=250` (25s; documented range 50-600, default 300) makes the evictor abandon
#      its teardown, and the queued demote take its first fresh prepare slot, a full five seconds
#      before the survivor's verify expiry. Prepares arriving mid-hold are deferred, not rejected
#      (measured), so the demote's live prepare is already waiting when the survivor's window
#      opens: the abandoned teardown is processed as stale, the demote commits (primary_nodes=0),
#      and the evaluation that follows is the licensed transition. Every margin is seconds;
#      nothing races. The opposite ordering fails: twopc-timeout=100 aborts at +10s and the abort
#      cycle outruns the hold, so the evictor re-prepares its teardown, the fresh teardown is the
#      live change at the expiry, and the inline evaluation voids the run. The demote must be the
#      live change at the expiry; 25s puts it there with seconds to spare on both sides.
#
# ⚠️ The two 4K writes are load-bearing. DRBD bumps a peerless Primary's current UUID lazily, on
# the first write it cannot replicate, and that bump is the divergence. With an unmounted device
# and no I/O it never happens, and the pair rejoins `no-sync by rule=reconnected` even after a
# wrong promotion. On a real node the evictee serves a mounted btrfs with a running app, whose
# writes do this for free; here they have to be asked for.
#
# Two DRBD knobs, both documented tunables inside their legal ranges, and the decision logic runs
# stock code on stock inputs: the survivor's keepalive (`--ping-int=120 --ping-timeout=300`, which a
# WAN-witness install tunes exactly) and the resource's `twopc-timeout=250`. A safety invariant
# that holds only for some legal timer settings is the defect, not a misconfiguration: every value
# here is a supported deployment someone runs on purpose. The evictor's keepalive is fully stock,
# and the demote is armed by plain driver pacing: the 30s hold gives it ~20s of slack, so neither
# a faster evictor ping nor an in-guest `events2` watcher changes the outcome.
#
# ⚠️ The void-run guard. A run where the demote applied after the survivor's evaluation proves
# nothing and looks exactly like a fixed DRBD, which is fatal for the pin-bump probe. So every
# attempt checks the order in the survivor's own kernel log (the demote's `primary_nodes=0`
# computed/committed vs the evaluation `disk( Consistent -> `; arrival alone is not enough, a
# prepare can arrive and be aborted), retries a lost race up to three times, and aborts loudly
# rather than reporting a void as an outcome.
#
# ⚠️ This test is expected to fail on DRBD 9.2.19: it asserts the property, not the behaviour (an
# assertion has to be able to fail). It lives in the `debug` tag for the reason
# `drbd-survivor-restart` does, stated at its entry in outputs.nix: the nightly must stay a
# statement about what works, and a gate red for a known reason trains you to stop reading it. It
# goes green the day the pinned DRBD outdates the survivor despite the relayed demote.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  # iptables is not in the shipped guest's path (`networking.firewall.enable = false`), and this
  # test drops packets for one peer only. Added here, not in lib.nix: no other topology cuts an
  # edge, and the shipped image should not grow a tool for a test. `promoter = false` is
  # deliberate: drbd-reactor never starts here (roles are driven by hand so the election is not a
  # variable), so the snippet would configure a daemon that never runs. No `fixture` either: no
  # container runs, and one would drag the dummy OCI image into the build for nothing.
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
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")
    # The witness has no tier to build and still needs its `.res` and its attach;
    # briard-node-storage runs on every node, which is why one call covers both.
    witness.succeed("briard-test-storage")
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    # The seed syncs its joiner for real. A cut that lands mid-resync finds node2 Inconsistent,
    # and a survivor that holds the only good copy is right to go UpToDate and promote: act 1
    # would then report a stale promotion that is no such thing.
    node2.wait_until_succeeds("drbdadm dstate r0 | grep -q '^UpToDate'", timeout=300)

    def status(m):
        return json.loads(m.succeed("drbdsetup status r0 --json"))[0]

    def quorate(m):
        return status(m)["devices"][0]["quorum"]

    # drbd-reactor never starts: who elects the primary is not under test, and driving the roles
    # by hand keeps the fault the only moving part. node2 serves.
    node2.succeed("drbdadm primary r0")
    for m in disk_nodes:
        assert quorate(m), f"{m.name} is not quorate before the fault -- the rig is wrong, not the product"

    # Reachability is read from DRBD's own per-connection state, the thing under test: same JSON
    # as above, no tooling in the image. ⚠️ A `/dev/tcp/<peer>/7789` probe is worse than useless:
    # DRBD closes its listening socket once its peers are connected, so the probe fails for every
    # peer whether or not the link is cut ("the cut landed" passes vacuously, "the witness
    # survived" fails spuriously).
    def conn_states(m):
        return {c["name"]: c["connection-state"] for c in status(m)["connections"]}

    ADDR = {"node1": "10.0.0.1", "node2": "10.0.0.2", "witness": "10.0.0.3"}

    # The cut is deliberately dumb: DROP rules for the one peer address, both directions. No
    # asymmetry, loss or timing: the wild one-way blackhole played no part in the mechanism (any
    # single dead direction breaks both ping round-trips), so the reproducer must not encode it;
    # and the report is strongest with the dumbest possible cut.
    #
    # ⚠️ `ip route add blackhole` silently does nothing: the DRBD sockets are already established
    # and hold their cached dst, so a later more-specific route never applies. The nodes stay
    # Connected, and without a bounded wait the test would sit out a full 900s before anyone
    # learned the cut had not happened. Hence the packet filter and the non-vacuity check under
    # it: a fault that does not land must fail fast and say so, or a red test is an expensive way
    # to prove nothing.
    def cut(a, b):
        a.succeed(f"iptables -I INPUT -s {ADDR[b.name]} -j DROP")
        a.succeed(f"iptables -I OUTPUT -d {ADDR[b.name]} -j DROP")

    def heal(a, b):
        a.succeed(f"iptables -D INPUT -s {ADDR[b.name]} -j DROP")
        a.succeed(f"iptables -D OUTPUT -d {ADDR[b.name]} -j DROP")

    # The fault: only the node1↔node2 edge, dropped both ways on both ends. The witness (10.0.0.3)
    # stays reachable from both, which `block()` cannot express and is the entire point.
    cut(node1, node2)
    cut(node2, node1)

    # Both must notice the peer go (independent stock ping timers, up to ~10.5s each). Short
    # timeout: if DRBD has not reacted to a cut link within ~a minute, something else is wrong and
    # waiting fifteen more will not reveal it.
    #
    # ⚠️ Polled through the parsed JSON, never grepped: `drbdsetup --json` pretty-prints, so
    # `grep '"connection-state":"Connecting"'` matches nothing (the space after the colon). The one
    # reader of this JSON is `json.loads`.
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

    # Non-vacuity, both halves, in DRBD's own terms: the anchors have lost each other (above)...
    for m in disk_nodes:
        st = conn_states(m)
        # ...and both still hold the witness. Otherwise this is an ordinary partition with a clean
        # majority side, the tiebreaker is doing its job, and the test proves nothing at all.
        assert st["witness"] == "Connected", (
            f"{m.name} lost the witness too ({st}) -- that is a partition, not a link split"
        )

    # ---- Fact 1: both keep quorum off the tiebreaker, by design (header; upstream's own test
    # asserts it), so it is the race's precondition, not a property violation. If it stops
    # holding, upstream changed the tiebreaker semantics, a stale promotion is no longer reachable
    # this way, and the property below holds trivially.
    both_quorate = [m.name for m in disk_nodes if quorate(m)]
    tiebreakers = [m.name for m in disk_nodes
                   if m.execute("dmesg | grep -q 'using tiebreaker logic to keep'")[0] == 0]
    print(f"quorate after the cut: {both_quorate}; used the tiebreaker: {tiebreakers}")
    if len(both_quorate) < 2:
        print("NOTE: the double-keep is GONE -- upstream changed the tiebreaker semantics and the "
              "precondition for this race no longer holds; expect the rest of this test to come "
              "out safe, and re-read the analysis before trusting anything else here")

    # Act 1, the control: the demote is late on purpose (seconds, not milliseconds). With the
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

    # Bounded, and a fact rather than a precondition. In the wild, the survivor went
    # `disk( Consistent -> UpToDate ) [lost-peer]` 31ms after the demote landed and promoted a
    # second later. If it will not promote here, DRBD is refusing to let a stale node take the
    # house: the protection working, the most interesting thing this test can report, so not to
    # be spent as a 900s `wait_until_succeeds` that aborts before the fork question is asked.
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

    # ---- Fact 2: did the pair come back, or fork? A pair that reconnects lands Connected; a
    # forked one lands StandAlone and stays there.
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

    # Failures are collected and reported together: stopping at the first hides the fork, and the
    # fork is the harm while the licensed promotion is the cause. A fixer wants both in one run.
    failures = []
    if promoted:
        # Act 1 is the control and must stay safe. If a late demote ever licenses the survivor
        # too, the window has widened and act 2's ordering is no longer the story.
        failures.append(
            "act 1: node1 promoted on a stale disk even though the demote was seconds late -- "
            "the survivor's self-outdating no longer protects the ordinary case"
        )

    # ================= Act 2: the same fault with the eviction already in flight =================
    # The demote is issued mid-split and queues in-kernel behind the evictor's own teardown
    # commit; the knobs below make the evictor abandon that teardown 5s before the survivor's hold
    # ends, so the demote's fresh prepare is already waiting when the survivor's window opens
    # (header, point 3). Nothing else changes.

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

    outcome = None  # "raced" = defect exercised | "held" = survivor outdated despite the demote
    for attempt in range(1, 4):
        establish()
        # Cleared so the ordering greps below see this attempt of act 2 only; act 1's dmesg
        # evidence (tiebreaker, split-brain) is already collected.
        for m in disk_nodes:
            m.succeed("dmesg -C")
        # The first DRBD knob (header): the survivor's keepalive, pinning it in a 30s hold that
        # ends 5s after the evictor's twopc abandon (the next knob). The evictor runs stock.
        node1.succeed("drbdsetup net-options r0 1 --ping-int=120 --ping-timeout=300")  # peer 1 = node2
        # The other knob (header, point 3): the evictor's twopc abandons the teardown 5 seconds
        # before the survivor's expiry, handing the queued demote its fresh prepare slot while the
        # survivor still holds. Set on every node so no participant holds a stale prepare longer
        # than the initiator keeps it alive.
        for m in machines:
            m.succeed("drbdsetup resource-options r0 --twopc-timeout=250")
        for m in disk_nodes:
            print(f"{m.name} before act 2 attempt {attempt}: {disks(m)}")

        cut(node1, node2)
        cut(node2, node1)
        # Kernel-clock markers, so the dmesg dumps carry each node's latencies relative to the cut
        # in its own clock: the geometry question is all relative timing.
        for m in disk_nodes:
            m.succeed("echo 'DRBDTEST: cut applied' > /dev/kmsg")

        # The evictor notices on its stock schedule (<=10.5s); the survivor is pinned in its 30s
        # hold, so everything from here has ~20s of slack and plain driver pacing is enough.
        wait_peer_lost(node2)
        # Write before demoting: the 4K write makes it a fork rather than a clean handover (the
        # lazy UUID bump, header), and completes locally at once because node2 has already
        # declared the peer gone. The demote then blocks in-kernel behind node2's own teardown
        # twopc and gets its first fresh prepare slot when node2 abandons that teardown, 5s
        # before node1's hold ends: the entire trick.
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

        # The order, from the survivor's own kernel log: did the demote apply before node1's
        # lost-peer evaluation? Without it a lost race is indistinguishable from a fixed DRBD (the
        # void-run guard, header). Two markers, both read out of real runs' dmesg, not guessed:
        # the demote's prepare arriving (`1->all role( Secondary )`; node-id 1 is node2, the
        # initiator) and applying (`primary_nodes=0` computed or committed). ⚠️ Arrival alone is
        # not enough: a prepare can arrive and be aborted while the survivor holds, and a run
        # classified on arrival would report a rejected demote as "property held".
        demote_seen = first_line(node1, r"1->all role\( Secondary \)|peer\( Primary -> Secondary \)")
        demote_applied = first_line(node1, r"State change [0-9]+: primary_nodes=0|Committing remote state change.*\(primary_nodes=0\)")
        eval_ln = first_line(node1, r"disk\( Consistent -> ")
        print(f"attempt {attempt}: node1 decided {decision}; demote seen@dmesg:{demote_seen} "
              f"applied@dmesg:{demote_applied} eval@dmesg:{eval_ln}")
        print("node2 drbd dmesg:\n" + node2.execute("dmesg | grep -E 'drbd|DRBDTEST' | tail -45")[1])
        print("node1 drbd dmesg:\n" + node1.execute("dmesg | grep -E 'drbd|DRBDTEST' | head -60")[1])

        if decision == "UpToDate":
            # A licensed stale node requires primary_nodes=0, i.e. the demote applied; if the
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
        # Put node1's ping timers back before healing: a 30s ping-timeout makes the reconnect
        # handshake, which the next establish() waits for, take longer than its wait.
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
        # (disqualified)? This one transition is the defect; everything else follows from it.
        raced_promote = node1.execute("drbdadm primary r0")[0] == 0
        print(f"node1 promoted after the raced demote: {raced_promote}; state {disks(node1)}")
        if raced_promote:
            # ...and the other half of the divergence. node2 started a new generation from its own
            # unreplicable write; node1 must start one too, or there is only one fork and DRBD
            # simply resyncs the stale side. It is the write the household app would make the
            # moment the wrongly-promoted node serves anything.
            node1.succeed("dd if=/dev/urandom of=/dev/drbd0 bs=4k count=1 oflag=direct")
            print(f"node1 wrote as the wrongly-promoted primary; state {disks(node1)}")
            # The safety failure, stated as such: a node whose disk was `Consistent` (stale, peer
            # unreachable) was licensed to call itself authoritative and take the house. The fork
            # follows when there are writes to diverge; this is the step that permits it.
            failures.append(
                "a STALE node promoted while its peer was unreachable: node1 went "
                "disk( Consistent -> UpToDate ) [lost-peer] because the demote beat its "
                "self-outdating, then took the house -- the step every fork of this pair goes through"
            )
    else:
        print("the survivor outdated itself DESPITE the relayed demote -- the [lost-peer] "
              "decision no longer takes primary_nodes=0 as a licence. If the DRBD pin moved, "
              "this answers the pin-bump question")

    # Put node1's ping timers back before healing, as in the void path: the skew is for the
    # fault, and a 30s ping-timeout makes the reconnect handshake (which detects the fork) outlast
    # the settle wait, leaving the pair Connecting and a real split-brain unrecorded.
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
    assert not failures, "link-split: " + "; ".join(failures)
  '';
}
