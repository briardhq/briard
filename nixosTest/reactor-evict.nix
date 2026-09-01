# A PLANNED handover: `drbd-reactorctl evict` moves the work to a peer.
#
# Every failover proof we have is a FAILURE (drbd-failover crashes the primary, drbd-fence
# partitions it). The upgrade path needs the opposite: a node that is perfectly healthy giving
# the work up deliberately, so it can reboot into a new generation while its peer serves. That
# is a different operation with different risks — nothing is broken, so nothing forces the move,
# and the question is whether it happens at all, cleanly, and how fast.
#
# WHY THE CLI AND NOT A DEMOTE DANCE. drbd-reactor ships `drbd-reactorctl evict`, which
# runtime-masks the promoter target, stops it so a peer can promote, then unmasks. Writing our
# own stop-and-hope against the promoter would be reimplementing it with less knowledge of its
# own state machine. But we have prior form with this CLI — `drbd-reactorctl disable`'s systemctl
# reload proved flaky and was deferred (guestagent.go) — so `evict` is PROVEN here before
# anything sequences it.
#
# It also produces the number (c2-iv-6) is missing: how long a clean eviction takes to move the
# VIP. The post-*failure* timing is unmeasured; this is the planned case, which ought
# to be faster and more deterministic, and "ought to" is not a measurement.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [
      { name = "node1"; id = 0; }
      { name = "node2"; id = 1; }
      { name = "node3"; id = 2; }
    ];
  };
in
pkgs.testers.runNixOSTest {
  name = "reactor-evict";

  # The primary is elected dynamically, so the static checker can't follow which machine
  # object the assertions run against.
  skipTypeCheck = true;

  nodes = {
    node1 = node;
    node2 = node;
    node3 = node;
  };

  testScript = ''
    ${h.fixtureHelpers}
    import json
    import time

    machines = [node1, node2, node3]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.wait_for_unit("briard-test-fixture-install.service") # warm everywhere: a handover must not pull
        m.succeed("modprobe drbd")
        m.succeed("drbdadm create-md --force r0")
        m.succeed("systemctl start drbd@r0.target")

    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    # Format the fresh volume the way the INSTALLER now does ([B.126]): the product stopped
    # formatting on the promotion path, so a harness that seeds a resource by hand states it.
    node1.succeed("drbdadm primary r0 && mkfs.btrfs -f $(drbdadm sh-dev r0/0) && drbdadm secondary r0")
    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    # The front door answers before anything is installed -- the shipped state -- and the service
    # then goes onto the volume from whichever node promoted. Every later handover renders it
    # again from there ([V3b.3](f)), which is what an eviction is really moving.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    def primary_of(candidates):
        return next((m for m in candidates if role(m) == "Primary"), None)

    first = primary_of(machines)
    assert first is not None, "no primary was elected"
    dataroot = install_fixture(first)
    first.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    # Let the dummy's counter climb so "the data came with it" is decisive rather than
    # coincidental — a re-formatted volume restarts near zero, far below this.
    first.wait_until_succeeds(f"[ $(grep -oE '[0-9]+' {dataroot}/app/state.json) -ge 3 ]")
    t1 = int(json.loads(first.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    print(f"### primary before the eviction: {first.name} (tick {t1})")

    # ---- ACT 1: the eviction moves the work, and the front door follows -------------------
    #
    # Timed from the far side: the useful number is not how long the command takes to return
    # but how long the HOUSE is without a front door, which is what a household notices and
    # what (c2-iv-6) has to size a roll against.
    others = [m for m in machines if m != first]
    t_start = time.monotonic()
    first.succeed("drbd-reactorctl evict")

    # Some node other than the evicted one now serves. wait_until_succeeds polls, so the
    # elapsed time is an upper bound at its granularity, not a stopwatch — good enough to
    # tell "seconds" from "minutes", which is the decision it feeds.
    others[0].wait_until_succeeds("curl -fsS --max-time 2 http://192.168.1.100:8080/healthz")
    elapsed = time.monotonic() - t_start
    print(f"### VIP answered again {elapsed:.1f}s after the eviction")

    second = primary_of(others)
    assert second is not None, "no peer promoted after the eviction"
    # THE assertion of this test: the work actually left. An evict that no-oped would still
    # leave a healthy fleet serving a healthy VIP, and every other check here would pass.
    assert role(first) != "Primary", f"{first.name} is still Primary after evicting itself"
    print(f"### primary after the eviction: {second.name}")

    # Single-primary held throughout: an eviction must hand over, not fork.
    primaries = [m.name for m in machines if role(m) == "Primary"]
    assert len(primaries) == 1, f"expected exactly one primary, got {primaries}"

    # The data came with it (protocol C), same claim as the crash path -- a planned move must
    # be at least as safe as an unplanned one.
    t2 = int(json.loads(second.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    assert t2 >= t1 - 2, f"data lost across a PLANNED handover: t1={t1} t2={t2}"

    # ---- ACT 2: a plain evict does not exile the node ------------------------------------
    #
    # `evict` unmasks the target again on its way out, so the evicted node is immediately
    # eligible to hold the resource once more. That is what makes a hand-back possible at all
    # -- and it is exactly why the reboot path needs --keep-masked (act 3): without it, a node
    # rebooting for its own upgrade could take the work back before anyone verified it.
    first.succeed("systemctl show -p LoadState --value drbd-services@r0.target | grep -qv masked")

    # Hand it BACK, which is the second half of the roll (c2-iv-6 ends where it started).
    t_start = time.monotonic()
    second.succeed("drbd-reactorctl evict")
    first.wait_until_succeeds("curl -fsS --max-time 2 http://192.168.1.100:8080/healthz")
    print(f"### VIP answered again {time.monotonic() - t_start:.1f}s after the hand-back")
    assert role(second) != "Primary", f"{second.name} is still Primary after evicting itself"
    third = primary_of(machines)
    assert third is not None, "nobody holds the resource after the hand-back"
    print(f"### primary after the hand-back: {third.name}")

    # ---- ACT 3: --keep-masked keeps the node out until we say otherwise -------------------
    #
    # The reboot path's requirement. A runtime mask does NOT survive a reboot, which is the
    # trap: this proves the mask holds the node out while it is up, and `-u` releases it. What
    # a reboot does to it is the (c2-iv-6) sequencer's problem, not this mechanism's, and is
    # recorded rather than assumed here.
    holder = primary_of(machines)
    rest = [m for m in machines if m != holder]
    holder.succeed("drbd-reactorctl evict --keep-masked")
    rest[0].wait_until_succeeds("curl -fsS --max-time 2 http://192.168.1.100:8080/healthz")
    holder.succeed("systemctl show -p LoadState --value drbd-services@r0.target | grep -q masked")
    assert role(holder) != "Primary", f"{holder.name} kept the resource despite --keep-masked"

    # While masked it cannot take the work back even if the current holder leaves.
    now = primary_of([m for m in machines if m != holder])
    now.succeed("drbd-reactorctl evict")
    remaining = [m for m in machines if m not in (holder, now)]
    remaining[0].wait_until_succeeds("curl -fsS --max-time 2 http://192.168.1.100:8080/healthz")
    assert role(holder) != "Primary", f"masked node {holder.name} took the resource back"

    # ...and unmasking releases it, which is the deliberate hand-back after a verified upgrade.
    holder.succeed("drbd-reactorctl evict -u")
    holder.succeed("systemctl show -p LoadState --value drbd-services@r0.target | grep -qv masked")
    print("### REACTOR-EVICT PASS")
  '';
}
