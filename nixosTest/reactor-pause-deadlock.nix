# The SIMPLEST deterministic reproduction of the promote-vs-stop systemd deadlock,
# stripped of everything downstream (no broken payload, no snapshot/restore, no btrfs race).
#
# One node promotes r0 (so drbd-services@r0.target is active and drbd-reactor has written its
# `Before=drbd-reactor.service` drop-in), then we time a BARE maintenance pause. On the buggy
# config the reactor, as it stops, tears down its own `drbdsetup events2` feed, restarts it,
# re-emits exists-events, and fires one last `systemctl start drbd-services@r0.target` — which
# systemd sequences BEHIND the reactor's own stop (the Before= ordering), so neither can proceed
# → ~90s SIGKILL. With the drop-in removed first → a few ms.
#
# Unlike an upgrade test this asserts on the DEADLOCK itself (pause duration), which is
# deterministic (the 90s hang was 16/16 across earlier runs; only the downstream unmount was
# the flaky 1-in-8). So this test is a clean red/green gate for the fix. Debug tag; NOT nightly.
#
# HERMETIC. Driving it through a nested guest and the agent's verbs would add nothing: drive the pause through
# the agent's `reactor.pause` verb over virtio-serial. Nothing here needed that: what is under
# test is a systemd/drbd-reactor ordering interaction INSIDE the node, and the driver contributed
# a stopwatch. Moving it onto lib.nix — the scaffolding single-node-promoter and reactor-evict
# already use — is one of the two steps that let the fixture guest disk be deleted, which is
# (e4)'s whole point. What it costs is stated at the pause below.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # A single node: mesh-of-one, the shape single-node-promoter proves. The deadlock is a property
  # of one node's promoter stopping, so a peer would only add a variable.
  node = h.mkNode {
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  # The drop-in drbd-reactor writes over its own promoter target, and the thing whose removal
  # defuses the deadlock. Mirrors `reactorBeforeOverride` in agent/guestagent/guestagent.go, and
  # deliberately a literal rather than a shared constant: a test that imported it could not
  # notice the product changing it.
  beforeOverride = "/run/systemd/system/drbd-services@r0.target.d/reactor-50-before.conf";
in
pkgs.testers.runNixOSTest {
  name = "reactor-pause-deadlock";
  nodes.node1 = node;

  testScript = ''
    import time

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.succeed("modprobe drbd")
    node1.succeed("drbdadm create-md --force r0")
    node1.succeed("systemctl start drbd@r0.target")
    # Make the peer-less resource UpToDate so the promoter can promote without --force.
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")

    # Hand off to the promoter and let the WHOLE ordered chain converge — promote → mount →
    # payload → VIP. The deadlock is a property of stopping a promoter that has services up, so
    # pausing before the chain finished would be timing something else.
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    node1.succeed("systemctl is-active drbd-services@r0.target")

    # NON-VACUITY: the drop-in must actually be there. It is what creates the ordering cycle, so
    # if drbd-reactor ever stopped writing it this test would keep passing while timing a stop
    # that could not have deadlocked at all — green for the wrong reason. Asserted, not assumed.
    node1.succeed("test -f ${beforeOverride}")

    # THE PAUSE — exactly what the guest agent's `reactor.pause` verb runs
    # (agent/guestagent/guestagent.go, verbReactorPause): remove the ordering drop-in, reload,
    # stop the daemon. Mirrored here rather than driven through the agent because the verb is
    # three commands with no decision in it, while reaching it needs a nested guest plus a host
    # on the far side of a virtio-serial channel. What that gives up is coverage of the verb's
    # own plumbing — the class of defect where the unit's PATH lacks a binary, which bit us once
    # — and that is carried by the lab fleet demos, which run the real agent.
    # Keep this sequence in step with the verb.
    start = time.monotonic()
    node1.succeed("rm -f ${beforeOverride}")
    node1.succeed("systemctl daemon-reload")
    node1.succeed("systemctl stop drbd-reactor.service")
    ms = int((time.monotonic() - start) * 1000)
    print(f"pause timing: REACTOR_PAUSE_MS={ms}")

    # A clean stop is milliseconds; the deadlock is the full ~90s TimeoutStopSec. 10s splits them
    # with a huge margin either way. This asserts the deadlock is GONE.
    assert ms < 10000, f"reactor.pause took {ms}ms — the promote-vs-stop deadlock is present (expect <10s)"

    # And the pause was a PAUSE, not an outage: stop-services-on-exit defaults false, so the
    # promoted services and the DRBD Primary stay up while the daemon is down. Without this, a
    # deadlock "fixed" by tearing everything down would time in milliseconds and pass.
    node1.succeed("drbdadm role r0 | grep -q Primary")
    node1.succeed("systemctl is-active briard-data.service podman-briard-payload.service briard-vip.service")
    print(f"OK: reactor.pause completed in {ms}ms (no deadlock), services still up")
  '';
}
