# Witness loss under a WAN outage: the survivor must REFUSE to promote.
#
# The 2-node + cloud-witness tier leans on the cloud witness as the
# tiebreaker. A WAN outage *removes* it (here: block the witness node). With both
# disk nodes up that's still 2-of-3 = majority, so the cluster stays quorate and
# keeps serving — but a *further* node loss leaves the survivor at 1-of-3 = no
# quorum, and it must **refuse to promote**: no split-brain, no false failover.
# This is the safety backstop under the user-facing "2 nodes → failover only while
# your internet is up" claim — with the witness gone, a node death does NOT fail over.
#
# Complements the existing tests: drbd-witness (witness *present* → survivor+witness
# promote) and drbd-fence (isolated primary *self-fences* on quorum loss). The path
# neither covers is a *minority* survivor correctly refusing to become primary. Uses
# the same diskless witness VM — the DRBD quorum mechanism is identical whether the
# witness runs locally or in our cloud (only the deployment location is a v2 concern).
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  diskNode = h.mkNode { inherit resource; };
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
    import time

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

    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")

    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    primary = next(m for m in disk_nodes if role(m) == "Primary")
    secondary = next(m for m in disk_nodes if m != primary)
    print(f"primary={primary.name}")

    # WAN outage: the cloud witness becomes unreachable. Both disk nodes are still
    # up = 2-of-3 = majority, so the cluster stays quorate and keeps serving.
    witness.block()
    primary.succeed("drbdadm role r0 | grep -q Primary")
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")

    # Now lose the primary too. The survivor is alone: 1-of-3 (dead primary +
    # partitioned witness) = no quorum, so it must REFUSE to promote — the
    # split-brain backstop. No failover happens (the documented 2-node-offline limit).
    primary.crash()

    # The VIP dies with the crashed primary and does NOT move to the survivor.
    secondary.wait_until_fails("curl -fsS --max-time 3 http://192.168.1.100:8080/healthz")

    # Give drbd-reactor ample time to (wrongly) promote, then assert it did not:
    # the survivor stays Secondary, claims no VIP, runs no payload.
    time.sleep(20)
    secondary.succeed("drbdadm role r0 | grep -q Secondary")
    secondary.fail("ip -4 addr show dev eth1 | grep -q 192.168.1.100")
    secondary.fail("systemctl is-active podman-briard-payload.service")
    secondary.fail("curl -fsS --max-time 3 http://192.168.1.100:8080/healthz")
  '';
}
