# Converge-and-serve, per-service OCI form — the PRIMARY converge case
#. The payload writes the service data, so its OCI image is the
# Data-format identity. The data carries the pinned image ref — here an IMMUTABLE
# CONTENT DIGEST (the image id), proving the pin is by content, not a mutable tag — and a
# promoting node re-creates the container from *that* image (pre-staged on every node —
# so it's a local retag, never a build/pull on the failover path), or refuses.
#
# Unlike the whole-system-closure switch (which is per-node in nixosTest because the
# hostname is baked in), OCI digests are node-independent — so this converge-and-serve
# is provable hermetically. Companion to converge-at-promotion.nix (the refuse half).
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # A distinct "v1" payload image (a label makes it a different digest from the default
  # v0), pre-staged on every node — the warm-standby production does.
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
  stageV1 = { system.extraDependencies = [ dummyV1 ]; }; # v1 present in every node's store
  diskNode = { imports = [ (h.mkNode { inherit resource; }) stageV1 ]; };
  witnessNode = { imports = [ (h.mkNode { inherit resource; diskless = true; promoter = false; }) stageV1 ]; };
in
pkgs.testers.runNixOSTest {
  name = "converge-payload";
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
        m.succeed("podman load -i ${dummyV1}") # pre-stage v1 (warm-standby stand-in)
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

    # Baseline: the primary serves the DEFAULT payload image, not v1.
    base = primary.succeed("podman container inspect briard-payload --format '{{.Image}}'").strip()
    assert base != v1id, f"baseline already on v1? base={base}"
    print(f"primary={primary.name} serving default; survivor={survivor.name}; v1={v1id}")

    # Pin the data to v1 by its IMMUTABLE CONTENT DIGEST (the image id), not the mutable
    # briard-dummy:v1 tag — the "code identity as content" form, and proof the pin
    # mechanism is ref-agnostic (`podman image exists`/`tag` accept a bare content id, so
    # the registry-pull future's @sha256:digest drops in unchanged). The tag form is
    # covered by rolling-update.nix + the fleet demos. The pin file replicates on the DRBD
    # volume, so the survivor reads the same digest.
    primary.succeed(f"echo {v1id} > /var/lib/briard/.payload-image")
    primary.succeed("sync")
    primary.crash()

    # CONVERGE-AND-SERVE: the survivor booted the default payload, but the data demands
    # the v1 digest. converge re-points the serve tag at the pre-staged image with that
    # id, and the survivor re-creates the container from it and serves — matching code.
    survivor.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")
    got = survivor.succeed("podman container inspect briard-payload --format '{{.Image}}'").strip()
    assert got == v1id, f"survivor serves image {got}, expected the pinned v1 digest {v1id}"
    print("survivor re-created the payload from the data's pinned digest and served — converge-and-serve by immutable id")
  '';
}
