# The chain member contract: what a unit in the drbd-reactor promoter start-list may and may not
# do to the promotion, one assertion per rule. A is the reverse proxy (a real member, and the one
# that actually fails in the field), driven by SIGKILL, `systemctl restart` and `daemon-reload`,
# never by editing a unit file: a fault injection that writes can trigger the outcome by a path
# you did not intend, and so misattribute a teardown.
#
#   1 A is started on promotion
#   2 A is not started on the node that lost the race
#   3 a manual restart, and a daemon-reload, do not demote      -- so updates are safe
#   4 crashes under the start limit do not demote               -- so a transient fault is cheap
#   5 crossing the start limit does demote, and the peer takes over
#   6 the demotion stops A
#   7 the node that gave up masks its drbd-services@ target -- a local "not me, for now", the only
#     lever that exists (reactor has no anti-flap: an UpToDate, Connected node sleeps zero
#     before racing for a resource it just handed away)
#   8 the hold releases itself and clears the start limit, and the peer keeps serving
#
# Rules 3 and 4 never read the role for their verdict: a torn-down chain rebuilds and is Primary
# again in ~40ms, so `drbdadm role` a settle window later says Primary either way, and an
# assertion on it passes straight through a full demote + data-volume unmount + re-promote.
# `chain_since()` is the window that sees it: the start timestamps of the promote unit and the
# mount move, and stay moved.
#
# Two nodes, because rule 2 needs a loser and the consequence of rule 5 is a handover: on a lone
# node the demote is followed 40ms later by the same node re-promoting, which hides how severe a
# demote is.
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
  # The hold, shortened so the test can drive its whole lifecycle. `promotionHoldSecs` is an
  # option precisely so this is the only thing varying it (a field node runs the 300s default),
  # and the number shows here rather than hiding in a sleep. 60 rather than a token value: rules 5
  # and 6 run inside the hold, which must outlast them, or rule 7 becomes flaky rather than wrong.
  holdSecs = 60;
  heldNode = {
    imports = [ node ];
    briard.promotionHoldSecs = holdSecs;
  };
  member = "briard-reverse-proxy.service";
in
pkgs.testers.runNixOSTest {
  name = "chain-member-contract";
  # The holder is elected by drbd-reactor, so which machine each assertion runs against is
  # decided at runtime and the static checker cannot follow it.
  skipTypeCheck = true;
  nodes = { node1 = heldNode; node2 = heldNode; };

  testScript = ''
    ${h.fixtureHelpers}
    import time

    # The Nix let-binding, handed to Python once so the unit name has one definition.
    member = "${member}"
    hold_secs = ${toString holdSecs}

    machines = [node1, node2]

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    def state(m, unit):
        return m.execute(f"systemctl is-active {unit}")[1].strip()

    def chain_since(m):
        # The identity of the promotion and of the data mount -- the monotonic witnesses a teardown
        # cannot rewind, unlike the role.
        return m.succeed(
            "systemctl show -p ActiveEnterTimestampMonotonic --value drbd-promote@r0.service",
            "systemctl show -p ActiveEnterTimestampMonotonic --value briard-primary-storage.service",
        ).strip().replace("\n", "/")

    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.wait_for_unit("briard-test-fixture-install.service")
        m.succeed("modprobe drbd")
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")

    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 1")
    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    holder = next(m for m in machines if role(m) == "Primary")
    loser = next(m for m in machines if m != holder)
    print(f"### elected holder={holder.name} loser={loser.name}")

    # === 1 Started on promotion ===========================================================
    holder.wait_until_succeeds(f"systemctl is-active {member}", timeout=60)
    print("### 1 A is started on promotion")

    # === 2 Not started on the race loser ==================================================
    # The member `BindsTo=drbd-promote@r0` and is ordered after it, so on the node that lost it
    # has no promotion to attach to. This is what `target-as = Wants` must not have weakened:
    # the guarantee never came from the target edge.
    assert state(loser, member) != "active", (
        f"RULE 2 VIOLATED: {member} is running on {loser.name}, which holds no promotion "
        f"(role={role(loser)})"
    )
    # ...and the loser must not have held itself down, which is not decoration: a member whose
    # start job fails with result `dependency` fires OnFailure= exactly like one that failed on its
    # own, and on the race loser every member does. Without a guard the standby masks itself for
    # the whole hold on every boot, five minutes at the shipped duration in which it cannot take
    # over. Rule 5 alone would pass anyway, its 90s timeout outlasting the 60s hold; the assertion
    # below is what makes that visible.
    assert loser.execute("test -L /run/systemd/system/drbd-services@r0.target")[0] != 0, (
        "RULE 2 VIOLATED: the race loser masked its own drbd-services@r0.target, so it cannot take over"
    )
    assert loser.execute("systemctl is-active briard-promotion-hold.service")[1].strip() != "active", (
        "RULE 2 VIOLATED: the race loser is running the promotion hold"
    )
    print(f"### 2 A is not started on the loser (state={state(loser, member)})")

    # === 3 Manual restart and daemon-reload do not demote =================================
    # An update does both: a pushed binary set try-restarts the running members it replaces
    # (agent/guestfirmware/bin.go), and writing the units reloads systemd. No maintenance bracket
    # covers either -- the bracket stops drbd-reactor, and none of this is drbd-reactor's doing.
    c = chain_since(holder)
    holder.succeed(f"systemctl restart {member}")
    time.sleep(8)
    assert chain_since(holder) == c, (
        f"RULE 3 VIOLATED: restarting {member} tore the chain down -- demote + data-volume "
        f"unmount + re-promote ({c} -> {chain_since(holder)})"
    )
    holder.succeed("systemctl daemon-reload")
    time.sleep(8)
    assert chain_since(holder) == c, (
        f"RULE 3 VIOLATED: a daemon-reload tore the chain down ({c} -> {chain_since(holder)})"
    )
    assert role(holder) == "Primary", "RULE 3 VIOLATED: the holder is no longer Primary"
    print("### 3 a manual restart and a daemon-reload left the promotion alone")

    # === 4 Crashes under the start limit do not demote ====================================
    # Budget cleared first, so this rule and rule 5 do not share an accounting: `reset-failed`
    # clears the start-rate counter that rule 3's manual restart also consumed.
    holder.succeed(f"systemctl reset-failed {member}")
    holder.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=60)
    c = chain_since(holder)
    for i in range(1, 4):  # StartLimitBurst=5, so three is comfortably under
        holder.succeed(f"systemctl kill -s KILL {member}")
        time.sleep(4)
        assert chain_since(holder) == c, (
            f"RULE 4 VIOLATED: crash {i} (under the start limit) tore the chain down "
            f"({c} -> {chain_since(holder)})"
        )
    holder.wait_until_succeeds(f"systemctl is-active {member}", timeout=30)
    assert role(holder) == "Primary", "RULE 4 VIOLATED: the holder is no longer Primary"
    print("### 4 three crashes under the limit left the promotion alone, and A recovered")

    # === 5 Crossing the start limit demotes, and the peer takes over ======================
    # Keep killing from the same budget. The verdict is the handover, read on the peer.
    for i in range(4, 12):
        holder.execute(f"systemctl kill -s KILL {member}")
        time.sleep(4)
        if state(holder, member) == "failed":
            print(f"### 5 {member} reached `failed` after {i} crashes")
            break
    assert state(holder, member) == "failed", (
        f"RULE 5 PRECOND: {member} never reached `failed`; the start limit was not crossed "
        f"(state={state(holder, member)})"
    )
    loser.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=90)
    assert role(holder) == "Secondary", (
        f"RULE 5 VIOLATED: the node whose member gave up is still {role(holder)}"
    )
    print(f"### 5 the resource handed over: {holder.name} -> {loser.name}")

    # The hand-over went through the unit built for it, not by accident -- and that unit is ours,
    # because the mask has to precede the demote (see briard-promotion-hold).
    holder.succeed("journalctl -u briard-promotion-hold.service --no-pager | grep -q .")
    print("### 5 the handover went through briard-promotion-hold")

    # === 6 The demotion stops A ===========================================================
    assert state(holder, member) != "active", (
        f"RULE 6 VIOLATED: {member} is still active on the demoted node"
    )
    holder.fail("mountpoint -q /var/lib/briard")
    print("### 6 the demotion stopped A and released the data volume")

    # === 7 The node that gave up refuses to take the resource straight back =================
    # The hold exists because reactor re-promotes ~2s after a demote and has no anti-flap of its
    # own (an UpToDate, Connected node sleeps zero before racing), so only the node declining
    # locally, which is what masking drbd-services@r0.target says, can stop a travelling fault
    # ping-ponging.
    assert holder.execute("test -L /run/systemd/system/drbd-services@r0.target")[0] == 0, (
        "RULE 7 VIOLATED: the demoted node did not mask its drbd-services@r0.target, so nothing stops it "
        "racing for the resource it just gave up"
    )
    assert holder.execute("systemctl is-active briard-promotion-hold.service")[1].strip() == "active", (
        "RULE 7 VIOLATED: the hold is not running on the demoted node "
        f"(state={holder.execute('systemctl is-active briard-promotion-hold.service')[1].strip()})"
    )
    print("### 7 the demoted node masked its drbd-services@r0.target and is holding")

    # === 8 The hold releases itself, and clears the start limit ============================
    # Without the reset the member is still inside its StartLimitIntervalSec window when the hold
    # ends, so the next promotion would start a unit systemd refuses -- and the hold length would
    # be silently pinned to that window instead of being a free parameter.
    holder.wait_until_fails("systemctl is-active briard-promotion-hold.service", timeout=hold_secs + 60)
    # Poll the thing being asserted, not a proxy for it. The release is a sequence -- `rm` the
    # mask, `daemon-reload` (~300ms), then `reset-failed` -- so the mask disappearing means step 1
    # is done, not the release. Keyed on the mask, this rule is a coin toss that can fail ~300ms
    # early with the product behaving identically either way.
    holder.wait_until_fails("test -L /run/systemd/system/drbd-services@r0.target", timeout=30)
    for _ in range(30):
        if holder.execute(f"systemctl is-failed {member}")[1].strip() != "failed":
            break
        time.sleep(1)
    assert holder.execute(f"systemctl is-failed {member}")[1].strip() != "failed", (
        f"RULE 8 VIOLATED: {member} is still `failed` after the hold released, so its start limit "
        "was never cleared and the next promotion would refuse to start it"
    )
    # The peer kept serving throughout; the released node simply stays Secondary, which is the
    # right end state -- expiry is not a claim on the resource.
    assert role(loser) == "Primary", "RULE 8 VIOLATED: the peer stopped serving during the hold"
    assert role(holder) == "Secondary", (
        f"RULE 8 VIOLATED: the released node took the resource back (role={role(holder)})"
    )
    print("### 8 the hold released, the start limit was cleared, and the peer kept serving")

    print("CHAIN_MEMBER_CONTRACT_OK")
  '';
}
