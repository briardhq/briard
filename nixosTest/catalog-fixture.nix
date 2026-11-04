# A catalogued app's fixture, read from its published entry -- catalog/<name>.json is the one
# place an app's name, version, networking, container, mounts, port, health path, memory and sizes
# are written, so a test runs what the catalog ships and a version bump touches nothing here.
#
# THE IMAGE is pulled by the entry's own digest. The catalog pins the multi-arch INDEX; pullImage
# copies with `--override-arch`, so the amd64 image under it is resolved, never stored. What
# cannot be derived is the hash nix demands of the fetch: pullImage repacks the image into a
# docker-archive, and that archive's hash is no digest a registry knows. It lives in
# catalog-images.json, keyed on the entry's image ref, one per arch. It is not a manifest field: a
# node never needs it, and it moves when nixpkgs' skopeo does, which as identity would offer every
# household an update to the same app. An entry missing there builds with the fake hash and fails
# printing the real one, which is what goes in the file.
#
# The fixture still names the archive's digest rather than the registry's (fixture-service.nix
# says why), so a test proves the entry's fields, not its exact bytes.
{
  pkgs,
  entry,
  # Further versions of the same app, as fixture-service.nix takes them.
  variants ? { },
}:
let
  inherit (pkgs) lib;
  m = lib.importJSON entry;
  c = lib.head m.containers;
  ref = lib.splitString "@" c.image;
  repo = lib.head ref;
  hashes = lib.importJSON ./catalog-images.json;
in
assert lib.assertMsg (lib.length m.containers == 1)
  "catalog-fixture: ${m.name} has ${toString (lib.length m.containers)} containers; a fixture is one";
assert lib.assertMsg (lib.length ref == 2) "catalog-fixture: ${c.image} is not pinned by digest";
import ./fixture-service.nix {
  inherit pkgs variants;
  inherit (m) name version;
  container = c.name;
  inherit (c) mounts port healthPath;
  env = c.env or { };
  # The schema's default: silence means private.
  network = m.network or "private";
  ports = m.ports or [ ];
  minMemoryMB = m.minMemoryMB or 0;
  size = m.size or 0;
  installedSize = m.installedSize or 0;
  imageName = repo;
  imageFile = pkgs.dockerTools.pullImage {
    imageName = repo;
    imageDigest = lib.last ref;
    sha256 = hashes.${c.image}.amd64 or lib.fakeHash;
    finalImageName = repo;
    finalImageTag = m.version;
    os = "linux";
    arch = "amd64";
  };
}
