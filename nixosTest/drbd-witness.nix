# 2 disk nodes + 1 diskless witness (M0b): DRBD-9 diskless quorum.
#
# The product's primary topology: two real nodes plus a cloud
# tiebreaker. The witness runs DRBD *diskless* (a quorum voter with no data copy)
# and does NOT run drbd-reactor — it never holds the service. Kill the primary
# disk node and the lone survivor still fails over, because survivor + witness =
# 2 of 3 = quorum; a bare 2-node cluster (1 of 2) could not. That is the whole
# point of the diskless tiebreaker, and what makes 2-node homes genuinely HA.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  diskNode = h.mkNode { inherit resource fixture; };
  # The witness: no backing disk, no promoter — a pure diskless quorum voter.
  witnessNode = h.mkNode {
    inherit resource;
    diskless = true;
    promoter = false;
  };
in
pkgs.testers.runNixOSTest {
  name = "drbd-witness";

  # crash() is QemuMachine-only + dynamic primary selection — see drbd-failover.
  skipTypeCheck = true;

  nodes = {
    node1 = diskNode;
    node2 = diskNode;
    witness = witnessNode;
  };

  testScript = ''
    ${h.fixtureHelpers}
    import json

    disk_nodes = [node1, node2]
    machines = [node1, node2, witness]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
    # Only the disk nodes have metadata to create; the witness is diskless.
    for m in disk_nodes:
        # The image is warmed on both disk nodes before anything promotes: the survivor renders
        # from the volume when it takes over and must not need a pull to do it.
        m.wait_for_unit("briard-test-fixture-install.service")
        m.succeed("drbdadm create-md --force r0")
    for m in machines:
        m.succeed("systemctl start drbd@r0.target")

    # All three connected (node1 ↔ node2 + the diskless witness), then skip the
    # initial sync. The per-node volume form needs the volume id (r0/0).
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    # Arm the one-time format the way BRING-UP does ([B.126]): the product no longer formats on
    # the promotion path, so a harness that seeds a resource by hand leaves the same marker.
    node1.succeed("mkdir -p /run/briard && touch /run/briard/data.format")

    # Only the disk nodes run the promoter; the witness just votes.
    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")

    # The front door answers first with nothing installed -- the shipped state -- and the service
    # is put on the volume by whoever promoted.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    primary = next(m for m in disk_nodes if role(m) == "Primary")
    dataroot = install_fixture(primary)
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    primary.wait_until_succeeds(f"[ $(grep -oE '[0-9]+' {dataroot}/app/state.json) -ge 3 ]")
    t1 = int(json.loads(primary.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    print(f"primary={primary.name} tick={t1}")

    # Kill the primary disk node. Survivor + diskless witness = 2 of 3 = quorum,
    # so the survivor promotes and serves — the diskless-quorum win (a lone disk
    # node would be 1 of 2 and self-fence).
    primary.crash()
    survivor = next(m for m in disk_nodes if m != primary)

    survivor.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")
    t2 = int(json.loads(survivor.succeed("curl -fsS http://192.168.1.100:8080/state"))["ticks"])
    assert t2 >= t1 - 2, f"data lost across witness-backed failover: t1={t1} t2={t2}"
    survivor.succeed("drbdadm role r0 | grep -q Primary")
  '';
}
