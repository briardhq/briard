# The maintenance-mode contract suite. "stop drbd-reactor == maintenance mode" is an abuse that
# rides on properties nothing else enforces; a violation (the promote-vs-stop deadlock is one)
# otherwise surfaces only incidentally, as a flaky managed upgrade. Asserting the contract
# directly makes a violation fail a named gate. One node promotes r0, then one pause → poke →
# resume lifecycle checks each point:
#
#   #1 the pause completes promptly   — no promote-vs-stop deadlock (defused on drbd-reactor
#                                       ExecStop); not timed here, just the lifecycle entry.
#   #2 the pause is non-destructive   — still Primary+quorate, and the app the same process
#                                       (active-since unchanged: a pause is not a restart).
#   #3 the paused promoter is inert   — a deliberate app stop gets a settle window for a (buggy)
#                                       promoter to wrongly react: no demote, no failover; the
#                                       restarted app proves the mount survived (tick continuity).
#   #4 the resume is clean            — the daemon re-adopts the already-Primary, running app:
#                                       no demote, no bounce.
#   #5 the pause holds across a reload — a `systemctl daemon-reload` while paused must not
#                                       disturb the promotion or the data mount. Read from unit
#                                       timestamps, not the role (a torn-down chain is Primary
#                                       again in ~40ms). Crashing or restarting a chain member is
#                                       chain-member-contract.nix, not the bracket; see the check.
#
# Hermetic: the lifecycle runs the same systemctl commands as the guest agent reactor.*/app.*
# verbs, not the agent over virtio-serial, since every check reads DRBD or systemd state inside
# the node. What that costs is stated at the pause below.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # A single DRBD node (no shipped node runs this shape: a lone node runs no DRBD and its pause is
  # a no-op): the contract is about one node's promoter, and a peer would only add a failover
  # path to rule out.
  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  # The fixture container unit, as the renderer names it (checked below): the unit a pause must
  # not touch.
  appUnit = "briard-${fixture.name}-${fixture.container}.service";
  # The drop-in drbd-reactor.service's ExecStop removes (guest-image/configuration.nix) — see the
  # pause below.
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
        # The per-device quorum flag the agent reports upward, read without the agent.
        code, out = node1.execute("drbdsetup status --json r0")
        return '"quorum": true' in out

    def require_primary(when):
        # The no-demote / no-failover invariant, at every step of a pause.
        r, q = role(), quorum()
        assert r == "Primary" and q, f"CONTRACT VIOLATED ({when}): not Primary+quorate (role={r} quorum={q})"

    def active():
        return node1.execute("systemctl is-active ${appUnit}")[0] == 0

    def since():
        # The app process identity: a pause or resume that bounced it moves this.
        return node1.succeed(
            "systemctl show -p ActiveEnterTimestampMonotonic --value ${appUnit}"
        ).strip()

    def chain_since():
        # The identity of the promotion and of the data mount. A torn-down and rebuilt chain is
        # Primary again in well under a second (measured: 40ms), so a role check passes through
        # the demote; these timestamps move, so the round trip stays visible and the check can fail.
        return node1.succeed(
            "systemctl show -p ActiveEnterTimestampMonotonic --value drbd-promote@r0.service",
            "systemctl show -p ActiveEnterTimestampMonotonic --value briard-primary-storage.service",
        ).strip()

    def ticks():
        return int(json.loads(node1.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service") # the image, warm before promotion
    node1.succeed("modprobe drbd")
    node1.succeed("briard-test-storage --seed")
    node1.succeed("systemctl start drbd-reactor.service")

    # === Baseline: promoter running, app serving =============================================
    # Waited for, not read once: Primary is the head of the ordered chain and mount → app → VIP
    # follow it, so a bare read can fail the precondition milliseconds after promotion.
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    # Nothing is installed before promotion: an app lives on the volume, so that ordering is the
    # product's, not the harness's. The reverse proxy answers meanwhile.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)
    install_fixture(node1)
    assert "${appUnit}" in fixture_units(node1), "the renderer stopped producing the unit this test names"
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    require_primary("baseline")
    assert active(), "CONTRACT PRECOND: the app is not active at baseline"
    born = since()
    # Non-vacuity for the pause: the drop-in must be armed, or the stop could not have deadlocked
    # and a clean pause would prove nothing. Removing it is the job of drbd-reactor.service
    # ExecStop, not this script.
    node1.succeed("test -f ${beforeOverride}")

    # === #1 The pause ========================================================================
    # Exactly what the `reactor.pause` verb runs (agent/guestagent/guestagent.go,
    # verbReactorPause): one command with no decision in it, so run here rather than through a
    # nested guest and a host across virtio-serial. This test does not carry the verb plumbing
    # (the class of defect where the unit PATH lacks a binary). Nothing is disarmed first: the
    # drbd-reactor.service ExecStop removes the `Before=` drop-in that would deadlock
    # promote-vs-stop. The duration is not gated anywhere (an isolated harness could not fail with
    # the defusal removed); under test is that a pause is non-destructive, a claim about the state
    # the stop leaves, however it got there.
    # Keep this in step with the verb.
    node1.succeed("systemctl stop drbd-reactor.service")

    # === #2 Non-destructive ===================================================================
    require_primary("after-pause")
    assert since() == born, f"CONTRACT VIOLATED (#2): the pause bounced the app ({born} -> {since()})"
    assert active(), "CONTRACT VIOLATED (#2): the app is not active after the pause"
    # Let the counter climb before capturing it: the dummy ticks ~1/s after a 3s slow start, so a
    # bare read can be 0, indistinguishable from a mount-loss reload-to-0. 10 sits well above what
    # a reloaded-from-zero app could reach during the stop+restart window.
    node1.wait_until_succeeds(
        "[ $(curl -fsS http://192.168.1.100:8080/state | grep -oE '[0-9]+' | head -1) -ge 10 ]",
        timeout=30,
    )
    pre = ticks()
    print(f"### #2 pause non-destructive: still Primary, app unbounced, ticks={pre}")

    # === #3 The paused promoter is inert =====================================================
    # `--job-mode=ignore-dependencies` is the one real decision of the `app.stop` verb
    # (agent/guestagent/guestagent.go, verbAppStop: "a planned app quiesce can't cascade to the
    # promoter target / data mount / VIP"); a mirror without it does not test the product
    # quiesce. `app.start` is a plain start.
    node1.succeed("systemctl --job-mode=ignore-dependencies stop ${appUnit}")
    time.sleep(8)  # long enough for a promoter reaction to manifest, if any
    assert not active(), "CONTRACT PRECOND: the app is still active after a deliberate stop"
    require_primary("after-stop-while-paused")  # the core guarantee: the stop drew no failover
    print("### #3 promoter inert: a deliberate app stop drew no demote or failover")

    # ...and the mount survived: ticks continue from the pre-stop value, where a torn-down
    # /var/lib/briard would reload from 0.
    node1.succeed("systemctl start ${appUnit}")
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=60)
    time.sleep(2)  # let at least one post-ready tick land before comparing
    post = ticks()
    assert post >= pre, f"CONTRACT VIOLATED (#3): data mount lost — ticks reset {pre} -> {post}"
    reborn = since()
    print(f"### #3 app restarted with its mount intact: ticks {pre} -> {post}")

    # === #4 Clean resume =====================================================================
    # The daemon must re-adopt what runs; a demote and re-promote fails both checks below.
    node1.succeed("systemctl start drbd-reactor.service")
    time.sleep(8)
    require_primary("after-resume")
    assert since() == reborn, f"CONTRACT VIOLATED (#4): the resume bounced the app ({reborn} -> {since()})"
    assert active(), "CONTRACT VIOLATED (#4): the app is not active after the resume"

    # Belt-and-suspenders: green here ⇒ the promoter resumed onto a live, mounted node.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=30)
    node1.succeed("systemctl is-active briard-primary-storage.service ${appUnit} briard-vip.service")

    # === #5 The pause across a daemon-reload ================================================
    # #3 covers an app stop; app units are not chain members (briard-apps converges them from the
    # volume), so a crashed container cannot reach the promoter target ("an app error alerts,
    # never demotes"). Five units are members (lib.nix promoterSnippet), wanted by the target
    # (`Wants=`, target-as). A member crash (`systemctl kill -s KILL`) or `systemctl restart` is
    # the chain-member contract, asserted in chain-member-contract.nix with drbd-reactor running:
    # the pause does not decide it, so an assertion here would be filed against the wrong layer.
    #
    # Only the reload is asserted here: a bare `systemctl daemon-reload` while paused, not a
    # synthetic gesture, as the guest agent reloads whenever it writes units
    # (agent/guestagent/converge.go, units.go). A reload alone starts no stop job; this proves it
    # stays inert. The verdict reads timestamps, not the role: a rebuilt chain is Primary again in
    # ~40ms, so `drbdadm role` would pass straight through a full demote + unmount + re-promote.
    node1.succeed("systemctl stop drbd-reactor.service")
    require_primary("second-pause")

    # The edges the question turns on, recorded rather than assumed, and read while paused:
    # stopping drbd-reactor leaves them in place.
    print(node1.succeed(
        "systemctl show -p Requires -p Wants -p BindsTo -p ConsistsOf drbd-services@r0.target",
        "systemctl show -p PartOf -p RequiredBy -p WantedBy briard-reverse-proxy.service",
    ))

    chain = chain_since()
    node1.succeed("systemctl daemon-reload")
    time.sleep(8)  # the same settle window #3 gives a reaction to manifest
    print(node1.succeed("drbdadm role r0", "systemctl is-active drbd-services@r0.target || true"))
    assert chain_since() == chain, (
        "CONTRACT VIOLATED (#5): a daemon-reload tore the promoter chain down and rebuilt it while "
        f"PAUSED -- demote + data-volume unmount + re-promote ({chain} -> {chain_since()})"
    )
    require_primary("after-daemon-reload-while-paused")
    print("### #5 promoter inert: a daemon-reload while paused left the chain and the mount alone")

    # Resume: a green run leaves the same state #4 did.
    node1.succeed("systemctl start drbd-reactor.service")
    time.sleep(8)
    require_primary("after-second-resume")

    print(f"MAINTENANCE_CONTRACT_OK: ticks {pre} -> {post}, app serving")
  '';
}
