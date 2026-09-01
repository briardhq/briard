# The maintenance-mode CONTRACT suite. "stop drbd-reactor == maintenance mode" is
# an abuse that rides on properties nothing enforced; the promote-vs-stop deadlock was a violation of them caught only
# incidentally, through a flaky managed-upgrade. This asserts the contract DIRECTLY, so the next
# violation fails a named gate instead of hiding.
#
# One node promotes r0, then the script drives one pause → poke → resume lifecycle and checks
# each contract point in turn:
#
#   #1 the pause completes promptly            — no promote-vs-stop deadlock ([B.28]), defused on
#                                                drbd-reactor.service's ExecStop; here the pause
#                                                is just the lifecycle's entry.
#   #2 the pause is NON-DESTRUCTIVE            — still Primary+quorate, and the service is the
#                                                SAME process (active-since unchanged: a pause is
#                                                not a restart).
#   #3 the paused promoter is INERT            — stop the service deliberately, give a (buggy)
#                                                promoter a settle window to wrongly react, and
#                                                prove it does not: no demote, no failover. Then
#                                                restart the service and prove the MOUNT SURVIVED,
#                                                via tick continuity.
#   #4 the resume is CLEAN                     — the daemon re-ADOPTS the already-Primary,
#                                                already-running service: no demote, no bounce.
#
# HERMETIC. Driving it through a nested guest and the agent's verbs would add nothing: drive the lifecycle
# through the agent's reactor.*/service.* verbs over virtio-serial (the driver's PAUSE_ONLY hook).
# Every check above is a read of DRBD or systemd state INSIDE the node, so none of it needed a
# host on the far side of a channel; moving it onto lib.nix is what lets the fixture guest disk
# be deleted, which is (e4)'s point. What it costs is stated at the pause below.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # A single node: the contract is about one node's promoter being paused, so a peer would add a
  # failover path the test then has to rule out rather than a property worth proving.
  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  # The service's own unit, named by the RENDERER rather than restated here -- it is the fixture's
  # container unit, and what the contract is about is that a pause does not touch it.
  serviceUnit = "briard-${fixture.name}-${fixture.container}.service";
  # Mirrors `reactorBeforeOverride` in agent/guestagent/guestagent.go — see the pause below.
  beforeOverride = "/run/systemd/system/drbd-services@r0.target.d/reactor-50-before.conf";
in
pkgs.testers.runNixOSTest {
  name = "maintenance-contract";
  nodes.node1 = node;

  testScript = ''
    ${h.fixtureHelpers}
    import json
    import time

    def role():
        return node1.execute("drbdadm role r0")[1].strip()

    def quorum():
        # `drbdsetup status --json` carries the per-device quorum flag; this is the same field
        # the agent reports upward, read here without the agent.
        code, out = node1.execute("drbdsetup status --json r0")
        return '"quorum": true' in out

    def require_primary(when):
        # The no-demote / no-failover invariant that must hold at EVERY step of a pause.
        r, q = role(), quorum()
        assert r == "Primary" and q, f"CONTRACT VIOLATED ({when}): not Primary+quorate (role={r} quorum={q})"

    def active():
        return node1.execute("systemctl is-active ${serviceUnit}")[0] == 0

    def since():
        # The service's process identity. A pause or a resume that BOUNCED it would move this;
        # comparing it is how "the same process is still running" becomes checkable.
        return node1.succeed(
            "systemctl show -p ActiveEnterTimestampMonotonic --value ${serviceUnit}"
        ).strip()

    def ticks():
        return int(json.loads(node1.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service") # the image, warm before promotion
    node1.succeed("modprobe drbd")
    node1.succeed("drbdadm create-md --force r0")
    node1.succeed("systemctl start drbd@r0.target")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    # Format the fresh volume the way the INSTALLER now does ([B.126]): the product stopped
    # formatting on the promotion path, so a harness that seeds a resource by hand states it.
    node1.succeed("drbdadm primary r0 && mkfs.btrfs -f $(drbdadm sh-dev r0/0) && drbdadm secondary r0")
    node1.succeed("systemctl start drbd-reactor.service")

    # === BASELINE: promoter running, service serving ==========================================
    # The service needs WAITING for, not one read: Primary is the HEAD of the promoter's ordered
    # chain and mount → service → VIP all follow it. A bare read here once lost that race and
    # failed the precondition 19ms after promotion (nightly 2026-07-27).
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    # Nothing is installed until the node has promoted -- the volume is where a service lives, so
    # that ordering is the product's, not the harness's. The front door answers meanwhile.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    install_fixture(node1)
    assert "${serviceUnit}" in fixture_units(node1), "the renderer stopped producing the unit this test names"
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    require_primary("baseline")
    assert active(), "CONTRACT PRECOND: the service is not active at baseline"
    born = since()
    # NON-VACUITY for the pause below: the drop-in must be armed, or the stop could not have
    # deadlocked and a clean pause would prove nothing. Its REMOVAL is now the unit's job
    # (drbd-reactor.service ExecStop, [B.85]) rather than this script's.
    node1.succeed("test -f ${beforeOverride}")

    # === #1 THE PAUSE =========================================================================
    # Exactly what the guest agent's `reactor.pause` verb runs (agent/guestagent/guestagent.go,
    # verbReactorPause): stop the daemon. One command with no decision in it, so it is run here
    # rather than driven through the agent, which would need a nested guest plus a host on the far
    # side of a virtio-serial channel. What that gives up is coverage of the verb's own plumbing —
    # the class of defect where the unit's PATH lacks a binary, which bit us once — and that is
    # carried by the lab fleet demos, which run the real agent.
    #
    # NOTHING IS DISARMED FIRST any more. The verb used to `rm` drbd-reactor's `Before=` drop-in
    # and reload before stopping, to dodge the promote-vs-stop deadlock, and this file mirrored
    # it; [B.85] moved that defusal onto drbd-reactor.service's ExecStop, so the bare stop below
    # is the whole of it. The DURATION is not gated anywhere now (the isolated harness could not
    # fail when the defusal was removed, so it was deleted) — what is
    # under test here is that a pause is non-destructive, a claim about the state the stop leaves
    # behind however it got there. Keep this in step with the verb.
    node1.succeed("systemctl stop drbd-reactor.service")

    # === #2 NON-DESTRUCTIVE ===================================================================
    require_primary("after-pause")
    assert since() == born, f"CONTRACT VIOLATED (#2): the pause bounced the service ({born} -> {since()})"
    assert active(), "CONTRACT VIOLATED (#2): the service is not active after the pause"
    # Let the counter climb clear of a fresh-start value before capturing it, so the continuity
    # check below has real margin: the dummy ticks ~1/s after a 3s slow start, so a bare read can
    # still be 0 — against which a mount-loss reload-to-0 would be indistinguishable. 10 sits well
    # above anything a reloaded-from-zero service could reach during the stop+restart window.
    node1.wait_until_succeeds(
        "[ $(curl -fsS http://192.168.1.100:8080/state | grep -oE '[0-9]+' | head -1) -ge 10 ]",
        timeout=30,
    )
    pre = ticks()
    print(f"### #2 pause non-destructive: still Primary, service unbounced, ticks={pre}")

    # === #3 THE PAUSED PROMOTER IS INERT ======================================================
    # `--job-mode=ignore-dependencies` is NOT decoration, and this test proved it the hard way:
    # written as a bare `systemctl stop` the job cascaded to drbd-services@r0.target, took the
    # promote unit with it, and the node was Secondary by the next check. That flag is the
    # `service.stop` verb's one real decision (agent/guestagent/guestagent.go, verbServiceStop:
    # "a planned service quiesce can't cascade to the promoter target / data mount / VIP"), so a
    # mirror that drops it is not testing the product's quiesce at all. `service.start` needs no
    # counterpart — it is a plain start.
    node1.succeed("systemctl --job-mode=ignore-dependencies stop ${serviceUnit}")
    time.sleep(8)  # long enough for a promoter reaction to manifest, if any
    assert not active(), "CONTRACT PRECOND: the service is still active after a deliberate stop"
    require_primary("after-stop-while-paused")  # the core guarantee: the stop drew no failover
    print("### #3 promoter inert: a deliberate service stop drew no demote or failover")

    # ...and the mount survived. /healthz recovers and the tick counter is continuous with the
    # pre-stop value; a torn-down /var/lib/briard would reload from tick 0.
    node1.succeed("systemctl start ${serviceUnit}")
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=60)
    time.sleep(2)  # let at least one post-ready tick land before comparing
    post = ticks()
    assert post >= pre, f"CONTRACT VIOLATED (#3): data mount lost — ticks reset {pre} -> {post}"
    reborn = since()
    print(f"### #3 service restarted with its mount intact: ticks {pre} -> {post}")

    # === #4 CLEAN RESUME ======================================================================
    # The daemon must re-ADOPT what is already running. A resume that demoted and re-promoted
    # would fail both checks below.
    node1.succeed("systemctl start drbd-reactor.service")
    time.sleep(8)
    require_primary("after-resume")
    assert since() == reborn, f"CONTRACT VIOLATED (#4): the resume bounced the service ({reborn} -> {since()})"
    assert active(), "CONTRACT VIOLATED (#4): the service is not active after the resume"

    # Belt-and-suspenders: green here ⇒ the promoter resumed onto a live, mounted node.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=30)
    node1.succeed("systemctl is-active briard-data.service ${serviceUnit} briard-vip.service")
    print(f"MAINTENANCE_CONTRACT_OK: ticks {pre} -> {post}, service serving")
  '';
}
