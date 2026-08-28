# The dummy fixture as a CATALOGUED SERVICE — a digest-pinned manifest plus the image tarball it
# names — so a test can install it the way a user installs one, with no registry.
#
# WHY THIS EXISTS. The fixture has always reached a test by being BAKED into the guest's payload
# slot, which was a mechanism no user had: a shipped node installs services at
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
  # THE IMAGE, when the caller has one. Default: the dummy is built here. The HA tests pass the
  # pinned upstream image instead, which is what lets a REAL service be catalogued rather than only
  # the fixture ([V3b.3](e2) -- the baked slot was how HA reached a guest before).
  #
  # `imageFile` is a docker-archive derivation and `imageName` is the repo the manifest names.
  # MEASURED 2026-08-28 (podman 5.8.2, skopeo 1.22.2), because the digest question is not obvious
  # for a PULLED image: `podman load` of a `dockerTools.pullImage` archive records
  # `<repo>@<archive digest>` -- the digest of the archive's own manifest, NOT the registry digest
  # the image was pulled by -- and `podman image exists <repo>@<archive digest>` resolves against
  # it, which is what `Pull=never` needs. So the manifest written here pins the archive's digest,
  # and that ref is resolvable on any node the tarball was staged onto. It is deliberately NOT the
  # ref a registry would serve: nothing here pulls, and a catalogue published to real nodes would
  # pin the registry digest instead.
  imageFile ? null,
  imageName ? null,
  # FURTHER VERSIONS OF THE SAME SERVICE, one per attribute, for a harness whose subject is a
  # version CHANGE rather than an install: `{ v1 = { version = "1.0.0"; }; bad = { version =
  # "0.0.0-bad"; env = { BRIARD_BROKEN = "1"; }; }; }`. Each is emitted under
  # `$out/variants/<label>/`, complete with its own signed catalog, and every one of them is
  # published under the SAME service name — an upgrade is one name moving versions, so a variant
  # that renamed itself would be a different service and prove nothing.
  #
  # ONE KEYRING ACROSS ALL OF THEM (catalog-sign signs every catalog with one key): the node is
  # given its trust root once, out of band, before any of these exist.
  #
  # A variant's image carries a `briard.version` label, so the CODE differs too and not just the
  # document naming it — a rotation whose two versions share a digest would render the identical
  # units and could pass while doing nothing. `env` rides the MANIFEST rather than the image,
  # which is where a catalogued service says what its container gets.
  variants ? { },
}:
let
  inherit (pkgs) lib;
  mkImage =
    { tag, labels }:
    pkgs.dockerTools.buildImage {
      name = "briard-${name}";
      inherit tag;
      config = {
        Cmd = [ "${pkgs.dummy-service}/bin/dummy-service" ];
        Labels = labels;
      };
    };
  # One published version's tarball and the repo its ref names: the caller's image if it supplied
  # one, else the dummy built here.
  imageOf =
    { file, repo, tag, labels }:
    {
      tarball = if file != null then file else mkImage { inherit tag labels; };
      repo = if repo != null then repo else "localhost/briard-${name}";
    };
  base = imageOf {
    file = imageFile;
    repo = imageName;
    tag = "v0";
    labels = { };
  };
  # An explicit permissive policy rather than `--insecure-policy`: the flag's NAME reads like a TLS
  # bypass, and this call has no TLS in it at all — it inspects a local file. Spelling it out keeps
  # the security-shaped flag out of the tree, the same call service-install.nix made for skopeo.
  policy = pkgs.writeText "policy.json" (builtins.toJSON {
    default = [ { type = "insecureAcceptAnything"; } ];
  });
  envJSON = e: if e == { } then "" else '',"env":${builtins.toJSON e}'';
  variantImages = lib.mapAttrs (
    label: v:
    imageOf {
      file = v.imageFile or null;
      repo = v.imageName or null;
      tag = label;
      labels = { "briard.version" = v.version; };
    }
  ) variants;
  # The base fixture and every variant, in the one shape the builder loops over. The base lives at
  # $out; a variant lives under $out/variants/<label>, so a caller reaches either through the same
  # relative layout (image.tar, ref, manifest.json, catalog/).
  published = [
    {
      dir = ".";
      image = base;
      inherit version;
      env = env;
    }
  ]
  ++ lib.mapAttrsToList (label: v: {
    dir = "variants/${label}";
    image = variantImages.${label};
    inherit (v) version;
    env = v.env or { };
  }) variants;
  # One publishable version's tarball, pinned manifest and staged catalog directory. The digest is
  # computed from the very bytes that get staged (see (3) above), per version.
  emit = p: ''
    mkdir -p $out/${p.dir}
    ln -s ${p.image.tarball} $out/${p.dir}/image.tar
    digest=$(skopeo --policy ${policy} --tmpdir "$TMPDIR" inspect docker-archive:${p.image.tarball} --format '{{.Digest}}')
    ref="${p.image.repo}@$digest"
    printf '%s' "$ref" > $out/${p.dir}/ref
    cat > $out/${p.dir}/manifest.json <<EOF
    {"name":"${name}","version":"${p.version}","containers":[{"name":"${container}","image":"$ref","mount":"${mount}","primary":true,"port":${toString port},"healthPath":"${healthPath}"${envJSON p.env}}]}
    EOF
    # The heredoc above is indented for readability; the manifest's BYTES are its identity, so
    # strip the indentation rather than shipping it into the content hash.
    sed -i 's/^    //' $out/${p.dir}/manifest.json
  '';
in
pkgs.runCommand "briard-fixture-${name}"
  {
    nativeBuildInputs = [
      pkgs.skopeo
      (pkgs.callPackage ./catalog-sign/package.nix { })
    ];
    # Surfaced as passthru so a caller can stage the tarball without reaching into $out.
    # serviceName is the manifest slug a caller passes to `service install`; `name` is the same
    # string, exposed under both spellings because callers read as one or the other. `variants`
    # carries each extra version's IMAGE, which a guest disk has to stage before the node can be
    # rolled onto it (nothing pulls: `service.warm` starts an .image unit only when the image is
    # missing).
    passthru = {
      inherit
        container
        mount
        port
        healthPath
        ;
      name = name;
      serviceName = name;
      image = base.tarball;
      # Each extra version's IMAGE and the label it is published under. A guest disk stages these
      # so a node can be rolled onto one without a pull, and a hermetic test stages them the same
      # way through lib.nix's fixture wiring.
      variants = lib.mapAttrs (label: img: { image = img.tarball; }) variantImages;
      variantLabels = lib.attrNames variants;
    };
  }
  ''
    mkdir -p $out
    ${lib.concatMapStrings emit published}

    # A minimal SIGNED CATALOG beside each version, so a harness can install (and then roll) this
    # the way a user does rather than seeding the node-local cache -- which reproduces what an
    # install records but not what it does (no data subvolume, so the container cannot start;
    # measured on a fleet run 2026-08-27). `fetchManifest` fails closed with no keyring and
    # verifies before parsing, both deliberately, so the honest answer is a real catalog and not a
    # weakened path. Every catalog here is signed by ONE key, because a node holds one keyring:
    # publishing a new version must not need the node re-trusted.
    catalog-sign ${
      lib.concatMapStringsSep " " (p: "$out/${p.dir}/manifest.json $out/${p.dir}/catalog") published
    }
  ''
