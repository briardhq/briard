# Boot the unit image and assert the 9.x DRBD module loads at runtime,
# and that the drbd-reactor daemon does NOT come up on its own.
#
# This is the harness skeleton M0 builds on: it boots N nodes (here 2) running
# the real unit config on a shared L2 segment. It does NOT configure DRBD
# resources or a promoter snippet, so the payload never starts here —
# replication is covered by drbd-replicate and the promoter-driven failover
# unit by drbd-promote.
#
# THE PROMOTER-GATE ASSERTION LIVES HERE ([V3b.16a]). This is the only test that boots the guest
# module with nothing declared over it, so it is the only place the PRODUCT's own default is
# observable — every other node either forces the reactor off (lib.nix) or runs a real agent. It
# used to `wait_for_unit("drbd-reactor.service")`: the daemon ran from boot, idle until a snippet
# appeared. That boot start is what raced the host agent's net.configure on every reboot of every
# install, and on the reboot it won it claimed the service VIP on the DRBD NIC under a second DHCP
# identity ([V3b.16]). The agent arms the promoter now, so an unconfigured guest must sit inert.
#
# `pkgs` must already carry the drbd-reactor overlay; runNixOSTest makes the
# nodes inherit it (their nixpkgs.pkgs is read-only and derived from this pkgs).
{ pkgs, guestModule }:

pkgs.testers.runNixOSTest {
  name = "guest-drbd9";

  nodes = {
    node1.imports = [ guestModule ];
    node2.imports = [ guestModule ];
  };

  testScript = ''
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        # The out-of-tree 9.x module must load — never an in-tree 8.x, which
        # lacks the quorum our safety model needs.
        m.succeed("modprobe drbd")
        out = m.succeed("cat /proc/drbd")
        assert "version: 9." in out, f"expected DRBD 9.x on {m.name}, got:\n{out}"
        # drbd-reactor is on PATH...
        m.succeed("command -v drbd-reactor")
        # ...and its daemon is INACTIVE, having reached multi-user.target without anything
        # pulling it in. `is-active` exits non-zero on inactive, which is what we want to see.
        m.fail("systemctl is-active drbd-reactor.service")
        # Not masked, not broken, not missing its dependencies — just unwanted by any target.
        # Asserting the mechanism rather than the symptom: a typo'd unit name would also read as
        # "inactive", and would pass an assertion that stopped at the line above.
        loaded = m.succeed("systemctl show -p LoadState --value drbd-reactor.service").strip()
        assert loaded == "loaded", f"expected drbd-reactor loaded-but-unstarted on {m.name}, got {loaded}"
        wanted = m.succeed("systemctl show -p WantedBy --value drbd-reactor.service").strip()
        assert wanted == "", f"expected nothing to want drbd-reactor on {m.name}, got {wanted!r}"

    # The agent's half of the gate, stood in for: bring-up ends in exactly this command
    # (guestagent's drbd.reactor.start verb), and it must still be the way the daemon comes up.
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_for_unit("drbd-reactor.service")
  '';
}
