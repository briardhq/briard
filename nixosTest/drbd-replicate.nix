# A real DRBD 9 resource replicating across two nodes under emulation ("does DRBD work under
# emulation"). Steady state only: bring a replicated volume up, write to the primary, prove the
# write reached the peer. No drbd-reactor, no VIP, nothing killed: those are the promote and
# failover tests.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    resource = h.mkResource [
      { name = "node1"; id = 0; }
      { name = "node2"; id = 1; }
    ];
    promoter = false; # bare DRBD — no drbd-reactor in this test
  };
in
pkgs.testers.runNixOSTest {
  name = "drbd-replicate";

  nodes = {
    node1 = node;
    node2 = node;
  };

  testScript = ''
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
        # The product's storage bring-up. Its create-md uses --force only because its probe found
        # no DRBD metadata on the LV, so there is nothing to protect; that is not force-promotion,
        # which stays banned tree-wide (CONTRIBUTING.md invariant 3).
        m.succeed("briard-test-storage")

    # The two sides find each other on the private DRBD subnet.
    node1.wait_until_succeeds("drbdadm cstate r0 | grep -q Connected")

    # Skip the initial full sync: both backing disks are blank, so declare them in sync
    # (--clear-bitmap). node1 is then UpToDate and promotes without force-promotion, the one-time
    # bootstrap that would otherwise need the forbidden command. (r0/0: per-node volume form.)
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    node1.succeed("drbdadm primary r0")

    # Put a filesystem on the replicated device and write a marker.
    node1.succeed("mkfs.ext4 -F /dev/drbd0")
    node1.succeed("mkdir -p /mnt && mount /dev/drbd0 /mnt")
    node1.succeed("echo briard-m0.5 > /mnt/marker && sync")

    # Replication reached the peer: under protocol C, UpToDate/UpToDate means the
    # write is durably on node2's disk before the primary acked it.
    node1.wait_until_succeeds("drbdadm dstate r0 | grep -q UpToDate/UpToDate")

    # Single-primary holds: node1 is Primary, node2 is Secondary and may not write.
    node1.succeed("drbdadm role r0 | grep -q Primary")
    node2.succeed("drbdadm role r0 | grep -q Secondary")
  '';
}
