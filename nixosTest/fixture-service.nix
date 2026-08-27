# The dummy fixture as a CATALOGUED SERVICE — a digest-pinned manifest plus the image tarball it
# names — so a test can install it the way a user installs one, with no registry.
#
# WHY THIS EXISTS. The fixture has always reached a test by being BAKED into the guest's payload
# slot (dummy-payload.nix), which is a mechanism no user has: a shipped node installs services at
# runtime from a manifest ([V3.16]). Every test riding the baked slot therefore proves a path
# nobody ships. This module is the other half of retiring it ([V3b.3](e)) — the same fixture,
# delivered the shipped way.
#
# WHY NO REGISTRY, which is the part that was assumed impossible and turned out not to be. The
# manifest schema accepts only digest-pinned refs (a tag is mutable, so it is UNSAYABLE here), and
# service-install.nix gets its digest by pushing to a real registry inside the test and inspecting
# it — which read as "a registry node per test". Measured on 2026-08-27 (podman 5.8.2, skopeo
# 1.22.2) instead of assumed:
#
#   1. `podman load` of a docker-archive RECORDS a RepoDigest, for an image never pulled;
#   2. `podman image exists localhost/<name>@<digest>` resolves against it — what Pull=never needs;
#   3. `skopeo inspect docker-archive:` returns the IDENTICAL digest at BUILD time.
#
# (3) is what makes this a derivation rather than a runtime step: the manifest is written here,
# already pinned, and the tarball is staged into the guest's podman store at boot. `Container.Image`
# in shared/manifest already described this shape — "from upstream, from our mirror, or from a
# tarball without changing what this service means".
#
# service-install.nix KEEPS its registry, deliberately: proving a real digest-pinned TLS pull is
# that test's whole subject. This is for every other test, whose subject is something else and
# which only needs a service to exist.
{
  pkgs,
  # The manifest slug: the unit prefix, the subvolume name, the handle a user would type.
  name ? "dummy",
  # The single container's slug within the service, and its data subdirectory's name.
  container ? "app",
  # The fixture's data path is a CONST in its own source, so the container's mount point is
  # dictated by the fixture while the host-side directory is the renderer's to choose — the normal
  # asymmetry for a catalogued service.
  mount ? "/var/lib/briard/dummy",
  port ? 8080,
  healthPath ? "/healthz",
  env ? { },
  version ? "0.0.0",
}:
let
  image = pkgs.dockerTools.buildImage {
    name = "briard-${name}";
    tag = "v0";
    config.Cmd = [ "${pkgs.dummy-service}/bin/dummy-service" ];
  };
  # An explicit permissive policy rather than `--insecure-policy`: the flag's NAME reads like a TLS
  # bypass, and this call has no TLS in it at all — it inspects a local file. Spelling it out keeps
  # the security-shaped flag out of the tree, the same call service-install.nix made for skopeo.
  policy = pkgs.writeText "policy.json" (builtins.toJSON {
    default = [ { type = "insecureAcceptAnything"; } ];
  });
  envJSON = if env == { } then "" else '',"env":${builtins.toJSON env}'';
in
pkgs.runCommand "briard-fixture-${name}"
  {
    nativeBuildInputs = [ pkgs.skopeo (pkgs.callPackage ./catalog-sign/package.nix { }) ];
    # Surfaced as passthru so a caller can stage the tarball without reaching into $out.
    passthru = { inherit image name container mount port healthPath; };
  }
  ''
    mkdir -p $out
    ln -s ${image} $out/image.tar

    # The digest the manifest pins IS the digest podman will record for the loaded archive — that
    # equality is the whole mechanism, so compute it from the very bytes that get staged.
    # --tmpdir because containers/image stages the archive through a hardcoded /var/tmp, which the
    # build sandbox does not have and cannot create (its root is read-only). Measured: without it
    # the build dies on "creating temporary file: open /var/tmp/container_images_docker-tar…".
    digest=$(skopeo --policy ${policy} --tmpdir "$TMPDIR" inspect docker-archive:${image} --format '{{.Digest}}')
    ref="localhost/briard-${name}@$digest"
    printf '%s' "$ref" > $out/ref

    cat > $out/manifest.json <<EOF
    {"name":"${name}","version":"${version}","containers":[{"name":"${container}","image":"$ref","mount":"${mount}","primary":true,"port":${toString port},"healthPath":"${healthPath}"${envJSON}}]}
    EOF
    # The heredoc above is indented for readability; the manifest's BYTES are its identity, so
    # strip the indentation rather than shipping it into the content hash.
    sed -i 's/^    //' $out/manifest.json

    # A minimal SIGNED CATALOG beside it, so a harness can install this the way a user does
    # rather than seeding the node-local cache -- which reproduces what an install records but
    # not what it does (no data subvolume, so the container cannot start; measured on a fleet run
    # 2026-08-27). `fetchManifest` fails closed with no keyring and verifies before parsing, both
    # deliberately, so the honest answer is a real catalog and not a weakened path.
    catalog-sign $out/manifest.json $out/catalog
  ''
