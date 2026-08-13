# The SIMPLEST deterministic reproduction of the promote-vs-stop systemd deadlock,
# stripped of everything downstream (no broken payload, no snapshot/restore, no btrfs race).
#
# One node promotes r0 (so drbd-services@r0.target is active and drbd-reactor has written its
# `Before=drbd-reactor.service` drop-in), then we time a BARE stop of the daemon. On the buggy
# config the reactor, as it stops, tears down its own `drbdsetup events2` feed, restarts it,
# re-emits exists-events, and fires one last `systemctl start drbd-services@r0.target` — which
# systemd sequences BEHIND the reactor's own stop (the Before= ordering), so neither can proceed
# → ~90s SIGKILL. With the drop-in removed first → a few ms.
#
# WHO REMOVES IT IS THE POINT, and it changed. The defusal used to live in the `reactor.pause`
# verb, and this file mirrored the verb's commands; [B.85] moved it onto drbd-reactor.service's
# ExecStop, because the same deadlock hangs every stop the verb never sees — a shutdown, the
# deadman's reboot, a user rebooting the host. This file now disarms NOTHING by hand, so a green
# run is a statement about the shipped unit rather than about three lines copied into a test.
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

    # A BARE STOP, WITH NOTHING DISARMED FIRST — and that is the whole test now.
    #
    # This used to mirror `reactor.pause`'s three commands (rm the drop-in, reload, stop) because
    # the verb carried the defusal. [B.85] moved it onto drbd-reactor.service's own ExecStop,
    # since the identical deadlock hangs every OTHER stop too — a shutdown, the deadman's reboot,
    # a host reboot — none of which come through the verb. Mirroring the verb after that would
    # have been worse than useless: MEASURED, this file passed in 401ms with the two manual
    # commands deleted, i.e. it had stopped being able to tell whose defusal it was timing.
    #
    # So it asks the only question with an owner left: stop a promoted reactor the way everything
    # in the product actually stops it, and see whether the unit disarms itself. `reactor.pause`
    # is now literally this line (verbReactorPause), so driving the verb through a nested guest
    # would add a virtio-serial channel and prove nothing more.
    start = time.monotonic()
    node1.succeed("systemctl stop drbd-reactor.service")
    ms = int((time.monotonic() - start) * 1000)
    print(f"pause timing: REACTOR_PAUSE_MS={ms}")

    # A clean stop is milliseconds; the deadlock is the full ~90s TimeoutStopSec. 10s splits them
    # with a huge margin either way. This asserts the deadlock is GONE — and, since nothing above
    # disarms it by hand, that drbd-reactor.service's ExecStop is what removed it.
    assert ms < 10000, (
        f"stopping a promoted drbd-reactor took {ms}ms — the promote-vs-stop deadlock is present "
        f"(expect <10s); drbd-reactor.service's ExecStop is what should have defused it"
    )

    # And the pause was a PAUSE, not an outage: stop-services-on-exit defaults false, so the
    # promoted services and the DRBD Primary stay up while the daemon is down. Without this, a
    # deadlock "fixed" by tearing everything down would time in milliseconds and pass.
    node1.succeed("drbdadm role r0 | grep -q Primary")
    node1.succeed("systemctl is-active briard-data.service podman-briard-payload.service briard-vip.service")
    print(f"OK: reactor.pause completed in {ms}ms (no deadlock), services still up")
  '';
}
