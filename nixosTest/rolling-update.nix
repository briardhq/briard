# Cluster rolling-update safety. The acute hazard is a *successful*
# single-node upgrade: the primary now runs new code and writes new-format data, but the
# peers are code-stale — so a later failover onto an old-code peer breaks. This proves
# the fix end-to-end on the cluster: a real in-place primary upgrade to v1 AND a
# subsequent failover both land on v1, because the upgrade writes the code identity to
# the replicated volume and the promoting peer converges to it.
#
# The in-place upgrade here uses the guest primitives Manager.UpgradePayload drives
# (maintenance → pin → restart → health-gate); the Manager itself is unit-tested. This
# test proves the *cluster* consequence: no skew break after an upgrade.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  dummyV1 = pkgs.dockerTools.buildImage {
    name = "briard-dummy";
    tag = "v1";
    config = {
      Cmd = [ "${pkgs.dummy-service}/bin/dummy-service" ];
      Labels."briard.version" = "1";
    };
  };
  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  stageV1 = { system.extraDependencies = [ dummyV1 ]; };
  diskNode = { imports = [ (h.mkNode { inherit resource; }) stageV1 ]; };
  witnessNode = { imports = [ (h.mkNode { inherit resource; diskless = true; promoter = false; }) stageV1 ]; };
in
pkgs.testers.runNixOSTest {
  name = "rolling-update";
  skipTypeCheck = true;

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
        m.succeed("podman load -i ${dummyV1}") # pre-stage v1 on every node
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
    survivor = next(m for m in disk_nodes if m != primary)
    v1id = survivor.succeed("podman image inspect briard-dummy:v1 --format '{{.Id}}'").strip()
    assert primary.succeed("podman container inspect briard-payload --format '{{.Image}}'").strip() != v1id, "already v1?"
    print(f"primary={primary.name} survivor={survivor.name} v1={v1id}")

    # ---- In-place payload upgrade on the primary (what Manager.UpgradePayload drives) ----
    # Maintenance: hold the promoter so a planned payload cycle isn't treated as a failure.
    primary.succeed("systemctl stop drbd-reactor.service")
    # Pin v1: point the serve tag at it AND record it on the replicated volume (so a
    # failover converges to it). Then cycle the payload onto v1.
    primary.succeed("podman tag briard-dummy:v1 briard-payload:serve")
    primary.succeed("echo briard-dummy:v1 > /var/lib/briard/.payload-image")
    primary.succeed("sync")
    primary.succeed("systemctl restart podman-briard-payload.service")
    # Health-gate: the new payload must serve before we accept the upgrade.
    primary.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")
    primary.succeed(f"test $(podman container inspect briard-payload --format '{{{{.Image}}}}') = {v1id}")
    primary.succeed("systemctl start drbd-reactor.service") # resume
    print("primary upgraded in place to v1 and serves it")

    # ---- The cluster consequence: a failover must NOT regress to old code ----
    primary.crash()
    survivor.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")
    got = survivor.succeed("podman container inspect briard-payload --format '{{.Image}}'").strip()
    assert got == v1id, f"failover regressed: survivor serves {got}, want v1 {v1id}"
    print("failover after the upgrade converged the survivor to v1 — no skew break")
  '';
}
