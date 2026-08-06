# Converge-at-promotion + code-identity-as-replicated-data.
#
# The cluster hazard: code is per-node but the DRBD volume replicates. A single-node
# upgrade leaves peers code-stale with the new data, so a failover onto an old-code
# peer meets new-format data and forward-only migrations break. The fix: the data
# carries the *code identity* it was written by, and a promoting node reconciles to it
# before serving.
#
# This proves the fail-safe half — "the real prize": a node that cannot satisfy the
# identity its data demands REFUSES to serve rather than serve stale code against new-format
# data, turning a silent skew-break into a safe deferral. The refusal is terminal here: there
# is no host agent in this hermetic test to resolve it, which is exactly the safe deferral.
# (The converge-and-serve half is `converge-payload`.)
#
# The identity it exercises is the PAYLOAD IMAGE, never the whole-OS closure. A system closure
# is a property of the NODE, not of the data, so refusing
# to serve over one deferred a node for no data-safety reason — and did it at promotion, i.e.
# during a failover. What the data genuinely pins is per-service, and the payload image is it:
# the payload is what writes the data, so a promoting node that does not hold the pinned image
# cannot serve that data safely. Same gate, same fail-safe, aimed at a real property.
#
# Baseline first: with no identity pinned, promotion is unchanged (converge is a
# no-op) — the older path still works.
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
  name = "converge-at-promotion";
  skipTypeCheck = true; # crash() + dynamic primary selection, as in drbd-failover

  nodes = {
    node1 = diskNode;
    node2 = diskNode;
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
    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")

    # BASELINE: no code identity pinned => briard-converge is a no-op, promotion is
    # unchanged. The primary serves (proves the older path is intact).
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    primary = next(m for m in disk_nodes if role(m) == "Primary")
    survivor = next(m for m in disk_nodes if m != primary)
    print(f"primary={primary.name} survivor={survivor.name}")
    primary.succeed("systemctl is-active briard-converge.service") # ran, succeeded (no-op)

    # Arm the hazard: pin the data to a payload image no node has staged, simulating an
    # upgrade whose image the survivor never warm-loaded. It's a file on the DRBD volume, so
    # it replicates (sync flushes it under protocol C) to the survivor.
    primary.succeed("echo briard-dummy:never-staged > /var/lib/briard/.payload-image")
    primary.succeed("sync")

    # Kill the primary. Survivor + diskless witness = 2 of 3 = quorum, so the survivor
    # *tries* to promote and serve.
    primary.crash()

    # REFUSE: the gate reads an image the node does not hold and refuses. It never fetches —
    # the failover path must not wait on a registry — so the survivor defers instead
    # of serving old code against data written by code it cannot run.
    survivor.wait_until_succeeds("journalctl -u briard-converge.service | grep -q 'refusing to promote'")
    survivor.fail("systemctl is-active podman-briard-payload.service") # payload never came up
    survivor.fail("curl -fsS --max-time 3 http://192.168.1.100:8080/healthz") # VIP never served
    print("survivor refused to serve against an unstaged payload image — safe deferral")
  '';
}
