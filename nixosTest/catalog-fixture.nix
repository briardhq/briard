# A catalogued app's fixture, read from the version the catalog's list names for it -- the
# `catalog` flake input's catalog/<app>/<version>.json, as catalog/latest.json names it, is the one
# place an app's name, version, networking, container, mounts, port, health path, memory and sizes
# are written, so a test runs what the catalog ships and a version bump touches nothing here.
#
# THE EDGE (`edge = true`): the fixture is the app's PREVIOUS version -- the next lower version
# file beside the listed one -- with the listed one as its variant `next`, so a test can install
# the one and upgrade to the other (app-edge.nix). The two files must differ only where a new
# upstream release moves them: version, image, size and installedSize. Anything else that differs
# is a change somebody made, not an upstream release, and evaluation refuses it.
#
# THE IMAGE is pulled by the entry's own digest. The catalog pins the multi-arch INDEX; pullImage
# copies with `--override-arch`, so the amd64 image under it is resolved, never stored. What
# cannot be derived is the hash nix demands of the fetch: pullImage repacks the image into a
# docker-archive, and that archive's hash is no digest a registry knows. It lives in
# the catalog's catalog-images.json, keyed on the entry's image ref, one per arch. It is not a manifest field: a
# node never needs it, and it moves when nixpkgs' skopeo does, which as identity would offer every
# household an update to the same app. An entry missing there builds with the fake hash and fails
# printing the real one, which is what goes in the file.
#
# The fixture still names the archive's digest rather than the registry's (fixture-app.nix
# says why), so a test proves the entry's fields, not its exact bytes. A variant carries its own
# version and image but the base's sizes.
{
  pkgs,
  # The catalog (the flake input) and the app in it.
  catalog,
  app,
  # Further versions of the same app, as fixture-app.nix takes them.
  variants ? { },
  edge ? false,
}:
let
  inherit (pkgs) lib;
  line = lib.findFirst (a: a.name == app && a.available or false)
    (throw "catalog-fixture: the catalog lists no installable version of ${app}")
    (lib.importJSON "${catalog}/catalog/latest.json").apps;
  entry = v: lib.importJSON "${catalog}/catalog/${app}/${v}.json";
  current = entry line.version;
  # The next lower version file: what a household upgrading to the listed version runs now.
  previous =
    let
      lower = lib.filter (v: builtins.compareVersions v line.version < 0) (
        map (lib.removeSuffix ".json") (lib.attrNames (builtins.readDir "${catalog}/catalog/${app}"))
      );
    in
    if lower == [ ] then
      throw "catalog-fixture: ${app} has no version below ${line.version}; its edge needs a previous"
    else
      entry (lib.last (lib.sort (a: b: builtins.compareVersions a b < 0) lower));
  m = if edge then previous else current;
  c = lib.head m.containers;
  hashes = lib.importJSON "${catalog}/catalog-images.json";
  repoOf = e: lib.head (lib.splitString "@" (lib.head e.containers).image);
  pull =
    e:
    let
      img = (lib.head e.containers).image;
    in
    pkgs.dockerTools.pullImage {
      imageName = repoOf e;
      imageDigest = lib.last (lib.splitString "@" img);
      sha256 = hashes.${img}.amd64 or lib.fakeHash;
      finalImageName = repoOf e;
      finalImageTag = e.version;
      os = "linux";
      arch = "amd64";
    };
  # What an upstream release may move; the rest of the two files must be equal.
  upstreamless = e: removeAttrs e [ "version" "size" "installedSize" ] // {
    containers = map (k: removeAttrs k [ "image" ]) e.containers;
  };
in
assert lib.assertMsg (lib.length m.containers == 1)
  "catalog-fixture: ${m.name} has ${toString (lib.length m.containers)} containers; a fixture is one";
assert lib.assertMsg (lib.length (lib.splitString "@" c.image) == 2) "catalog-fixture: ${c.image} is not pinned by digest";
assert lib.assertMsg (!edge || upstreamless previous == upstreamless current)
  "catalog-fixture: ${app} ${previous.version} -> ${current.version} differs in more than version, image, size and installedSize";
assert lib.assertMsg (!edge || repoOf previous == repoOf current)
  "catalog-fixture: ${app} ${current.version} moved its image from ${repoOf previous} to ${repoOf current}";
import ./fixture-app.nix {
  inherit pkgs;
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
  imageName = repoOf m;
  imageFile = pull m;
  variants =
    variants
    // lib.optionalAttrs edge {
      next = {
        inherit (current) version;
        env = (lib.head current.containers).env or { };
        imageFile = pull current;
        imageName = repoOf current;
      };
    };
}
