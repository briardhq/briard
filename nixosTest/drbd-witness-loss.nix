# Witness loss under a WAN outage: the survivor must refuse to promote.
#
# The 2-node + cloud-witness tier uses the cloud witness as tiebreaker; a WAN outage removes it
# (here: block the witness node). Both disk nodes up are still 2-of-3, so the cluster stays
# quorate and serves; a further node loss leaves the survivor at 1-of-3, no quorum, and it must
# refuse to promote: no split-brain, no false failover. This is the safety backstop under the
# user-facing "2 nodes → failover only while your internet is up" claim.
#
# drbd-witness covers the witness present (survivor + witness promote) and drbd-fence the
# isolated primary self-fencing; neither covers a minority survivor refusing to become primary.
# The same diskless witness VM serves: the DRBD quorum mechanism is identical whether the witness
# runs locally or in our cloud (only the deployment location differs).
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  diskNode = h.mkNode { inherit resource fixture; };
  witnessNode = h.mkNode {
    inherit resource;
    diskless = true;
    promoter = false;
  };
in
pkgs.testers.runNixOSTest {
  name = "drbd-witness-loss";

  # block()/crash() are QemuMachine-only + dynamic primary selection.
  skipTypeCheck = true;

  nodes = {
    node1 = diskNode;
    node2 = diskNode;
    witness = witnessNode;
  };

  testScript = ''
    ${h.fixtureHelpers}
    import time

    disk_nodes = [node1, node2]
    machines = [node1, node2, witness]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
    for m in disk_nodes:
        m.wait_for_unit("briard-test-fixture-install.service") # the image, warm before any promotion
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")
    # The witness has no tier to build and still needs its `.res` and its attach --
    # briard-node-storage runs on every node, which is why one call covers both.
    witness.succeed("briard-test-storage")
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")

    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")

    # The reverse proxy first, with nothing installed -- the shipped state -- then the app goes
    # onto the volume from whichever node promoted.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    primary = next(m for m in disk_nodes if role(m) == "Primary")
    secondary = next(m for m in disk_nodes if m != primary)
    print(f"primary={primary.name}")
    install_fixture(primary)
    service_units = fixture_units(primary)
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)

    # The crash below must find an UpToDate replica, to keep the refusal honest rather than to
    # enable a promotion. The seed path syncs a joiner for real, so a crash mid-resync leaves a
    # SyncTarget, which cannot promote whatever quorum says: every assertion below would hold for
    # the wrong reason. Quorum is the claim under test, so the disk state of the survivor is
    # confirmed first, leaving quorum the only thing refusing it, and waited for while the
    # cluster is still whole (this act's precondition).
    secondary.wait_until_succeeds("drbdadm dstate r0 | grep -q '^UpToDate'", timeout=300)

    # WAN outage: the witness is unreachable; 2-of-3 stays quorate and keeps serving.
    witness.block()
    primary.succeed("drbdadm role r0 | grep -q Primary")
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")

    # Lose the primary too: the survivor at 1-of-3 (dead primary, partitioned witness) has no
    # quorum and must refuse to promote. No failover happens (the documented 2-node-offline limit).
    primary.crash()

    # The VIP dies with the crashed primary and does not move to the survivor.
    secondary.wait_until_fails("curl -fsS --max-time 3 http://192.168.1.100:8080/healthz")

    # Give drbd-reactor ample time to (wrongly) promote, then assert it did not:
    # the survivor stays Secondary, claims no VIP, runs no app.
    time.sleep(20)
    secondary.succeed("drbdadm role r0 | grep -q Secondary")
    secondary.fail("ip -4 addr show dev eth1 | grep -q 192.168.1.100")
    for unit in service_units:
        secondary.fail(f"systemctl is-active {unit}")
    secondary.fail("curl -fsS --max-time 3 http://192.168.1.100:8080/healthz")
  '';
}
