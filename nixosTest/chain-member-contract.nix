# The CHAIN MEMBER contract: what a unit in drbd-reactor's promoter start-list may and may not do
# to the promotion ([V3b.5](c)).
#
# The rules, one assertion each. A is the front door -- a real member, and the one that actually
# fails in the field -- driven by SIGKILL, `systemctl restart` and `daemon-reload`, never by
# editing a unit file (a fault injection that WRITES can trigger the outcome by a path you did not
# intend, which is how the first cut of this investigation misattributed a teardown).
#
#   1 A is STARTED on promotion
#   2 A is NOT started on the node that LOST the race
#   3 a manual restart, and a daemon-reload, do NOT demote      -- so updates are safe
#   4 crashes UNDER the start limit do NOT demote               -- so a transient fault is cheap
#   5 crossing the start limit DOES demote, and the peer takes over
#   6 the demotion STOPS A
#
# NOTHING HERE READS THE ROLE FOR ITS VERDICT on rules 3 and 4. A torn-down chain rebuilds and is
# Primary again in ~40ms, so `drbdadm role` a settle window later says Primary either way -- the
# first version of this measurement asserted exactly that and passed straight through a full
# demote + data-volume unmount + re-promote. `chain_since()` is the window that sees it: the
# promote unit's and the mount's start timestamps move, and stay moved.
#
# WHY TWO NODES. Rule 2 needs a loser, and rule 5's consequence is a HANDOVER -- on a lone node the
# demote is followed 40ms later by the same node re-promoting, which is what hid this defect's
# severity for the whole first day of measuring it.
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
  member = "briard-reverse-proxy.service";
in
pkgs.testers.runNixOSTest {
  name = "chain-member-contract";
  # The holder is elected by drbd-reactor, so which machine each assertion runs against is
  # decided at runtime and the static checker cannot follow it.
  skipTypeCheck = true;
  nodes = { node1 = node; node2 = node; };

  testScript = ''
    ${h.fixtureHelpers}
    import time

    # The Nix let-binding, handed to Python once so the unit name has ONE definition.
    member = "${member}"

    machines = [node1, node2]

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    def state(m, unit):
        return m.execute(f"systemctl is-active {unit}")[1].strip()

    def chain_since(m):
        # The PROMOTION's identity and the data mount's -- the monotonic witnesses a teardown
        # cannot rewind, unlike the role.
        return m.succeed(
            "systemctl show -p ActiveEnterTimestampMonotonic --value drbd-promote@r0.service",
            "systemctl show -p ActiveEnterTimestampMonotonic --value briard-data.service",
        ).strip().replace("\n", "/")

    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.wait_for_unit("briard-test-fixture-install.service")
        m.succeed("modprobe drbd")
        m.succeed("drbdadm create-md --force r0")
        m.succeed("systemctl start drbd@r0.target")

    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 1")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    # Arm the one-time format the way BRING-UP does ([B.126]).
    for m in machines:
        m.succeed("mkdir -p /run/briard && touch /run/briard/data.format")
    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    holder = next(m for m in machines if role(m) == "Primary")
    loser = next(m for m in machines if m != holder)
    print(f"### elected holder={holder.name} loser={loser.name}")

    # === 1 STARTED ON PROMOTION ===========================================================
    holder.wait_until_succeeds(f"systemctl is-active {member}", timeout=60)
    print("### 1 A is started on promotion")

    # === 2 NOT STARTED ON THE RACE LOSER ==================================================
    # The member `BindsTo=drbd-promote@r0` and is ordered after it, so on the node that lost it
    # has no promotion to attach to. This is what `target-as = Wants` must NOT have weakened:
    # the guarantee never came from the target edge.
    assert state(loser, member) != "active", (
        f"RULE 2 VIOLATED: {member} is running on {loser.name}, which holds no promotion "
        f"(role={role(loser)})"
    )
    print(f"### 2 A is not started on the loser (state={state(loser, member)})")

    # === 3 MANUAL RESTART AND DAEMON-RELOAD DO NOT DEMOTE =================================
    # switch-to-configuration does both during an OS upgrade, inside the maintenance bracket --
    # which never covered this, because the bracket stops drbd-reactor and none of this is
    # drbd-reactor's doing.
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

    # === 4 CRASHES UNDER THE START LIMIT DO NOT DEMOTE ====================================
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

    # === 5 CROSSING THE START LIMIT DEMOTES, AND THE PEER TAKES OVER ======================
    # Keep killing from the same budget. The verdict is the HANDOVER, read on the peer.
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

    # The demote ran through the unit built for it, not by accident.
    holder.succeed("journalctl -u drbd-demote-or-escalate@r0.service --no-pager | grep -q .")
    print("### 5 the handover went through drbd-demote-or-escalate@r0")

    # === 6 THE DEMOTION STOPS A ===========================================================
    assert state(holder, member) != "active", (
        f"RULE 6 VIOLATED: {member} is still active on the demoted node"
    )
    holder.fail("mountpoint -q /var/lib/briard")
    print("### 6 the demotion stopped A and released the data volume")

    print("CHAIN_MEMBER_CONTRACT_OK")
  '';
}
