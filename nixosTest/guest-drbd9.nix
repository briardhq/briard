# Boot the unit image on N nodes (here 2, shared L2 segment) and assert the 9.x DRBD module loads
# at runtime and the drbd-reactor daemon does not come up on its own. No DRBD resources or
# promoter snippet, so no app starts here: replication is drbd-replicate, promoter-driven failover
# drbd-promote.
#
# The promoter-gate assertion lives here: the only test booting the guest module with nothing
# declared over it, so the only place the product default is observable (every other node is
# declared over by lib.nix, which starts the reactor by hand as the product does, or runs a real
# agent). A daemon running from boot, idle until a snippet appeared, would race the host agent's
# net.configure on every reboot of every install, and on a reboot it won it would claim the
# service VIP on the DRBD NIC under a second DHCP identity. The agent arms the promoter, so an
# unconfigured guest must sit inert.
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
        # drbd-reactor is on PATH, and its daemon is inactive at multi-user.target, nothing having
        # pulled it in.
        m.succeed("command -v drbd-reactor")
        m.fail("systemctl is-active drbd-reactor.service")
        # Not masked, broken or missing dependencies, just wanted by no target. The mechanism, not
        # the symptom: a typo'd unit name would also read "inactive" and pass the line above.
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
