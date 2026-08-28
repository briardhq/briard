# Zero services is the SHIPPED state. This boots the guest exactly as the downloadable
# artifact is built (guest-image/configuration.nix, no payload module) and asserts that a node
# with nothing installed is a working node: the volume mounts, the promoter chain completes,
# the VIP comes up, and the front door answers there.
#
# It is the counterpart to every other test in this directory, which select the dummy fixture
# into the payload slot. That fixture is never the default, so the shipped disk
# carried a tick counter and nothing anywhere booted the shape a stranger actually installs —
# the gap this test closes and then keeps closed.
#
# The failure it guards against is specific and was real: with the payload unit unconditionally
# in the promoter chain, an empty slot means the chain names a unit that does not exist, drbd-
# reactor fails the whole ordered unit, and the VIP never comes up. A node that installs
# cleanly and then serves nothing at all.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # Three nodes for the same reason every other failover test uses three: a crashed primary
  # leaves the survivor at 1/2 without a witness, and quorum correctly refuses to promote.
  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  node = h.mkNode {
    inherit resource;
    payload = false; # the shipped chain: data -> vip, with the front door riding the VIP
  };
  witnessNode = h.mkNode { inherit resource; diskless = true; promoter = false; };
in
pkgs.testers.runNixOSTest {
  name = "zero-service";
  skipTypeCheck = true;

  nodes = {
    node1 = node;
    node2 = node;
    witness = witnessNode;
  };

  testScript = ''
    disk_nodes = [node1, node2]
    machines = [node1, node2, witness]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
    for m in disk_nodes:
        m.succeed("drbdadm create-md --force r0")
    for m in machines:
        m.succeed("systemctl start drbd@r0.target")
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")

    # No service is installed, so the unit that would run one must not exist at all — not
    # merely be stopped. This is what makes the shipped closure free of a workload rather
    # than carrying a dormant one.
    for m in disk_nodes:
        m.fail("systemctl cat podman-briard-payload.service")

    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")

    # THE ASSERTION: the promoter chain completes and the VIP answers, with nothing installed.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=60)

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    primary = next(m for m in disk_nodes if role(m) == "Primary")
    survivor = next(m for m in disk_nodes if m != primary)
    print(f"primary={primary.name} survivor={survivor.name}")

    # CONVERGE RAN AND CONVERGED TO NOTHING ([V3b.3](f)). briard-services is a chain member on
    # every data node now, so a failure there would take the VIP down on the one node shape every
    # stranger installs — precisely the failure this test exists to guard, arriving via a new unit.
    # Succeeding against an empty volume is the shipped state, not a degraded one.
    primary.succeed("systemctl is-active briard-services.service")
    left = primary.succeed("ls -A /run/containers/systemd 2>/dev/null || true").strip()
    assert left == "", f"converge wrote service units on a node with nothing installed: {left!r}"

    # The node reports itself READY, not sick. A fresh install used to sit unhealthy forever
    # because the health probe pointed at a payload port nobody was listening on.
    health = primary.succeed("curl -fsS http://192.168.1.100/healthz")
    assert "no backend configured" in health, f"/healthz on an empty node said: {health!r}"

    # And a human who opens the VIP sees Briard, not a connection refused.
    page = primary.succeed("curl -fsS http://192.168.1.100/")
    assert "Briard" in page and "Nothing is routed to this address" in page, f"the VIP served: {page!r}"

    # The data volume is mounted and genuinely empty of service data — the substrate is up,
    # waiting for something to run. (.snapshots is the ladder's own directory, not a payload.)
    primary.succeed("mountpoint -q /var/lib/briard")
    primary.fail("test -e /var/lib/briard/payload")

    # Failover still works with nothing installed: the whole point is that the node is
    # replicating and able to take over BEFORE it has a workload, so that installing one
    # later lands on a substrate already proven.
    primary.crash()
    survivor.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=90)
    print("an empty node fails over and still answers at the VIP — the substrate is the product")
  '';
}
