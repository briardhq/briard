# Boot the unit image and assert the 9.x DRBD module loads at runtime,
# and that the drbd-reactor daemon comes up.
#
# This is the harness skeleton M0 builds on: it boots N nodes (here 2) running
# the real unit config on a shared L2 segment. It does NOT configure DRBD
# resources or a promoter snippet, so drbd-reactor stays idle and the payload
# never starts here — replication is covered by drbd-replicate and the
# promoter-driven failover unit by drbd-promote.
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
        # drbd-reactor is on PATH and its daemon runs from boot (idle until a
        # promoter snippet for a resource exists — see the drbd-promote test).
        m.succeed("command -v drbd-reactor")
        m.wait_for_unit("drbd-reactor.service")
  '';
}
