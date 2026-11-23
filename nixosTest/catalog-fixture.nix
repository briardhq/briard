# A catalogued app's fixture, read from the version the catalog's list names: the `catalog` flake
# input's catalog/<app>/<version>.json, as catalog/main.json names it, is the one place an
# app's name, version, networking, container, mounts, port, health path, memory and sizes are
# written, so a test runs what the catalog holds and a version bump touches nothing here.
#
# The edge (`edge = true`): the fixture is the app's published version (the one named by
# catalog/latest.json, the catalog's copy of the list nodes are served), with the listed one as
# its variant `next`, so a test can install one and upgrade to the other (app-edge.nix). The two
# files may differ only where an upstream release moves them (version, image, size,
# installedSize); anything else is a change somebody made, and evaluation refuses it.
#
# The image is pulled by the entry's own digest. The catalog pins the multi-arch index; pullImage
# copies with `--override-arch`, so the amd64 image under it is resolved, never stored. The hash
# nix demands of the fetch cannot be derived (pullImage repacks into a docker-archive whose hash
# no registry knows), so it lives in the catalog's catalog-images.json, keyed on the image ref,
# one per arch. Not a manifest field: a node never needs it, and it moves with nixpkgs' skopeo,
# which as identity would offer every household an update to the same app. A missing entry
# builds with the fake hash and fails printing the real one, which is what goes in the file.
#
# The fixture names the archive's digest, not the registry's (fixture-app.nix says why), so a
# test proves the entry's fields, not its exact bytes. A variant carries its own version, env and
# images but the base's sizes.
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
    (lib.importJSON "${catalog}/catalog/main.json").apps;
  entry = v: lib.importJSON "${catalog}/catalog/${app}/${v}.json";
  current = entry line.version;
  # The published version: what a household upgrading to the listed version runs now. Not the
  # next lower version file: an app whose listed version waits to be published has two lines of
  # versions in its directory, and the next lower file can be the other line's.
  previous =
    let
      was = lib.findFirst (a: a.name == app && a.available or false) null
        (lib.importJSON "${catalog}/catalog/latest.json").apps;
    in
    if was == null || was.version == line.version then
      throw "catalog-fixture: ${app} ${line.version} is the published version, or the app has none; its edge needs one to upgrade from"
    else
      entry was.version;
  m = if edge then previous else current;
  c = lib.head m.containers;
  hashes = lib.importJSON "${catalog}/catalog-images.json";
  repoOfC = k: lib.head (lib.splitString "@" k.image);
  repoOf = e: repoOfC (lib.head e.containers);
  # A version names one part per container, in their order, joined by `-`: `2.0.2-4.25.1` is
  # Files 2.0.2 with Samba 4.25.1. Each image is tagged with its own part and not the whole
  # version: the archive's hash covers the tag, and an image two versions share (the other part
  # moved) must keep the one hash its ref is recorded under.
  partsOf = e: lib.splitString "-" e.version;
  # Container `i`'s image, of version `e`.
  pullC =
    e: i: k:
    pkgs.dockerTools.pullImage {
      imageName = repoOfC k;
      imageDigest = lib.last (lib.splitString "@" k.image);
      sha256 = hashes.${k.image}.amd64 or lib.fakeHash;
      finalImageName = repoOfC k;
      finalImageTag = lib.elemAt (partsOf e) i;
      os = "linux";
      arch = "amd64";
    };
  pull = e: pullC e 0 (lib.head e.containers);
  # The containers after the first, as fixture-app.nix takes them: each pulled by its own digest,
  # its other fields as the entry says them.
  extraOf =
    e:
    lib.imap1 (
      i: k:
      removeAttrs k [ "image" ]
      // {
        imageFile = pullC e i k;
        imageName = repoOfC k;
      }
    ) (lib.tail e.containers);
  onePartEach = e: lib.length (partsOf e) == lib.length e.containers;
  # What an upstream release may move; the rest of the two files must be equal.
  upstreamless = e: removeAttrs e [ "version" "size" "installedSize" ] // {
    containers = map (k: removeAttrs k [ "image" ]) e.containers;
  };
in
assert lib.assertMsg (c.primary or false)
  "catalog-fixture: ${m.name}'s first container, ${c.name}, is not its primary; a fixture takes the primary first";
assert lib.assertMsg (lib.all (k: lib.length (lib.splitString "@" k.image) == 2) m.containers)
  "catalog-fixture: ${m.name} has a container not pinned by digest";
assert lib.assertMsg (onePartEach m && (!edge || onePartEach current))
  "catalog-fixture: ${app}'s version must name one part per container, joined by -";
assert lib.assertMsg (!edge || upstreamless previous == upstreamless current)
  "catalog-fixture: ${app} ${previous.version} -> ${current.version} differs in more than version, image, size and installedSize";
assert lib.assertMsg (!edge || map repoOfC previous.containers == map repoOfC current.containers)
  "catalog-fixture: ${app} ${current.version} pulls from ${toString (map repoOfC current.containers)}, where ${previous.version} pulled from ${toString (map repoOfC previous.containers)}";
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
  extraContainers = extraOf m;
  variants =
    variants
    // lib.optionalAttrs edge {
      next = {
        inherit (current) version;
        env = (lib.head current.containers).env or { };
        imageFile = pull current;
        imageName = repoOf current;
        extraContainers = extraOf current;
      };
    };
}
