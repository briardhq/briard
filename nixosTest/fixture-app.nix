# The dummy fixture as a catalogued app (a digest-pinned manifest plus the image tarball it names),
# so a test installs it the way a user installs one: at runtime from a manifest. The guest image
# has no build-time app slot, and a fixture baked into the guest would prove a path nobody ships.
#
# No registry. The schema accepts only digest-pinned refs (a mutable tag is unsayable), and
# app-install.nix gets its digest by pushing to a real registry, which reads as "a registry node
# per test". Measured (podman 5.8.2, skopeo 1.22.2):
#
#   1. `podman load` of a docker-archive records a RepoDigest, for an image never pulled;
#   2. `podman image exists localhost/<name>@<digest>` resolves against it — what Pull=never needs;
#   3. `skopeo inspect docker-archive:` returns the identical digest at build time.
#
# (3) makes this a derivation, not a runtime step: the manifest is written here already pinned,
# and the tarball is loaded into the podman store the node mounts (imageStore below), the shape
# `Container.Image` in shared/manifest describes ("from upstream, from our mirror, or from a
# tarball without changing what this app means"). app-install.nix deliberately keeps its
# registry, since a real digest-pinned TLS pull is its whole subject; this is for every other
# test, which only needs an app to exist.
{
  pkgs,
  # The manifest slug: the unit prefix, the subvolume name, the handle a user would type.
  name ? "dummy",
  # The single container's slug within the app, and its unit's.
  container ? "app",
  # The container mounts, as the manifest says them. The data path is a const in the fixture
  # source, so the fixture dictates the mount point and the renderer chooses the host-side
  # directory, the normal asymmetry for a catalogued app.
  mounts ? [
    {
      tier = "data";
      dir = container;
      path = "/var/lib/briard/dummy";
    }
  ],
  port ? 8080,
  healthPath ? "/healthz",
  env ? { },
  version ? "0.0.0",
  # The pod networking, host by default rather than the schema default: this fixture is the
  # substrate of the DRBD, promoter and converge tests, and a new networking mode under tests
  # about something else makes every failure ambiguous. The private path is proven where it is the
  # subject: by mosquitto (private in the real catalog) and a fixture that deliberately asks
  # for it.
  network ? "host",
  # Published ports, meaningful only on a private pod: a host-networked one already holds the
  # guest ports, and the schema refuses the combination.
  ports ? [ ],
  # The declared minimum memory (manifest minMemoryMB), and with it the container memory limit.
  # Zero, the default, declares none.
  minMemoryMB ? 0,
  # The declared sizes (manifest size / installedSize, bytes): what the download stages and the
  # image store holds once pulled, which the host grows the guest disk for and gates the install
  # on. Declared, not measured from the tarball: the host reads the numbers. Zero declares none.
  size ? 0,
  installedSize ? 0,
  # The image, when the caller has one (default: the dummy, built here). The HA tests pass the
  # pinned upstream image, which lets a real app be catalogued, not only the fixture. `imageFile`
  # is a docker-archive derivation, `imageName` the repo the manifest names.
  #
  # Measured (podman 5.8.2, skopeo 1.22.2), since the digest is not obvious for a pulled image:
  # `podman load` of a `dockerTools.pullImage` archive records `<repo>@<archive digest>`, the
  # digest of the archive manifest, not the registry digest it was pulled by, and
  # `podman image exists` resolves that ref, as `Pull=never` needs. So the manifest pins the
  # archive digest, resolvable on any node the tarball was staged onto, and deliberately not the
  # ref a registry would serve: nothing here pulls, and a catalogue published to real nodes would
  # pin the registry digest.
  imageFile ? null,
  imageName ? null,
  # Further versions of the same app, one per attribute, for a harness whose subject is a
  # version change rather than an install: `{ v1 = { version = "1.0.0"; }; bad = { version =
  # "0.0.0-bad"; env = { BRIARD_BROKEN = "1"; }; }; }`. Each is emitted under
  # `$out/variants/<label>/` with its own signed catalog, all under the same app name: an upgrade
  # is one name moving versions, and a renamed variant would be a different app, proving nothing.
  # One key signs every catalog (catalog-sign): the node gets its trust root once, out of band,
  # before any of these exist. A variant image carries a `briard.version` label so the code
  # differs, not just the document: two versions sharing a digest would render identical units
  # and could pass while doing nothing. `env` rides the manifest, where a catalogued app says
  # what its container gets.
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
        Cmd = [ "${pkgs.dummy-app}/bin/dummy-app" ];
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
  # An explicit permissive policy rather than `--insecure-policy`, whose name reads like a TLS
  # bypass though this call only inspects a local file: it keeps the security-shaped flag out of
  # the tree, as app-install.nix does for skopeo.
  policy = pkgs.writeText "policy.json" (builtins.toJSON {
    default = [ { type = "insecureAcceptAnything"; } ];
  });
  envJSON = e: if e == { } then "" else '',"env":${builtins.toJSON e}'';
  # Published ports, omitted when empty so a host-networked fixture keeps its exact manifest
  # bytes; the schema refuses `ports` alongside host networking.
  portsJSON = ps: if ps == [ ] then "" else '',"ports":${builtins.toJSON ps}'';
  # The declared minimum memory, omitted when zero so every fixture that asks for no limit keeps
  # its exact bytes (and with them its identity).
  minMemJSON = if minMemoryMB == 0 then "" else '',"minMemoryMB":${toString minMemoryMB}'';
  sizeJSON = if installedSize == 0 then "" else '',"size":${toString size},"installedSize":${toString installedSize}'';
  variantImages = lib.mapAttrs (
    label: v:
    imageOf {
      file = v.imageFile or null;
      repo = v.imageName or null;
      tag = label;
      labels = { "briard.version" = v.version; };
    }
  ) variants;
  # The base (at $out) and every variant (under $out/variants/<label>), in the one shape the
  # builder loops over, so a caller reaches either through the same relative layout (image.tar,
  # ref, manifest.json, catalog/).
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
  # The podman store, prebuilt: every archive is `podman load`ed once, here, and the store ships
  # as a read-only disk the node mounts as an additional image store. Loading on every node at
  # every boot costs, for the 2.4 GB HA archive, ~8 GB of host disk writes and ~100 s per node,
  # for a pure-harness step (a shipped node pulls by digest, image.ensure, and never loads an
  # archive). The proof is the same: the load records the same RepoDigest, and
  # `podman image exists <ref>` at boot (lib.nix) asserts the digest resolves through the store.
  #
  # A disk image, not a store directory: a loaded layer carries whiteouts as character devices,
  # which a store path cannot hold. erofs is made for a read-only overlay lower (device nodes and
  # xattrs, the opaque-directory markers, intact; written in one stream). The load needs overlayfs,
  # so it runs in a build VM (the tool make-disk-image uses) with a scratch disk for podman
  # temporary copies: the VM root is tmpfs, where a 2.4 GB archive copied twice would be memory.
  imageStore =
    let
      vm = pkgs.vmTools.override {
        rootModules = [
          "virtio_pci"
          "virtio_mmio"
          "virtio_blk"
          "virtio_balloon"
          "virtio_rng"
          "ext4"
          "virtiofs"
          "crc32c"
          "overlay"
        ];
      };
      podman = "podman --root /mnt/storage --runroot /run/podman --storage-driver overlay --cgroup-manager cgroupfs --events-backend none";
    in
    vm.runInLinuxVM (
      pkgs.runCommand "briard-fixture-${name}-store"
        {
          nativeBuildInputs = [
            pkgs.podman
            pkgs.e2fsprogs
            pkgs.erofs-utils
            pkgs.util-linux
          ];
          memSize = 2048;
          preVM = vm.createEmptyImage {
            size = 16384;
            fullName = "scratch";
            destination = "./scratch";
          };
        }
        ''
          mkfs.ext4 -q /dev/vda
          mkdir -p /mnt /etc/containers /sys/fs/cgroup
          mount /dev/vda /mnt
          mount -t cgroup2 none /sys/fs/cgroup
          mkdir -p /mnt/storage /mnt/tmp
          export TMPDIR=/mnt/tmp
          cp ${policy} /etc/containers/policy.json
          ${lib.concatMapStrings (p: ''
            ${podman} load -i ${p.image.tarball}
          '') published}
          ${podman} images --digests
          mkdir -p $out
          mkfs.erofs --quiet $out/store.erofs /mnt/storage
        ''
    );
  # One publishable version's tarball, pinned manifest and staged catalog directory. The digest is
  # computed from the very bytes that get staged (see (3) above), per version.
  emit = p: ''
    mkdir -p $out/${p.dir}
    ln -s ${p.image.tarball} $out/${p.dir}/image.tar
    digest=$(skopeo --policy ${policy} --tmpdir "$TMPDIR" inspect docker-archive:${p.image.tarball} --format '{{.Digest}}')
    ref="${p.image.repo}@$digest"
    printf '%s' "$ref" > $out/${p.dir}/ref
    cat > $out/${p.dir}/manifest.json <<EOF
    {"name":"${name}","version":"${p.version}","network":"${network}"${portsJSON ports},"containers":[{"name":"${container}","image":"$ref","mounts":${builtins.toJSON mounts},"primary":true,"port":${toString port},"healthPath":"${healthPath}"${envJSON p.env}}]${minMemJSON}${sizeJSON}}
    EOF
    # The heredoc is indented for readability; the manifest bytes are its identity, so strip the
    # indentation rather than ship it into the content hash.
    sed -i 's/^    //' $out/${p.dir}/manifest.json
  '';
in
pkgs.runCommand "briard-fixture-${name}"
  {
    nativeBuildInputs = [
      pkgs.skopeo
      (pkgs.callPackage ./catalog-sign/package.nix { })
    ];
    # Passthru, so a caller can stage the tarball without reaching into $out. appName is the slug
    # a caller passes to `app install`; `name` is the same string, as callers read one or the
    # other. `variants` carries each extra version image, which a guest disk must stage before the
    # node can be rolled onto it (nothing pulls: `app.warm` starts an .image unit only when the
    # image is missing).
    passthru = {
      inherit
        container
        mounts
        port
        healthPath
        ;
      # The data mount's directory in the app's data area: where a test finds what the app keeps.
      dataDir = (pkgs.lib.findFirst (m: m.tier == "data") null mounts).dir;
      name = name;
      appName = name;
      image = base.tarball;
      inherit installedSize;
      # Each extra version image and its published label, staged by a guest disk (or, in a
      # hermetic test, by the lib.nix fixture wiring) so a node rolls onto one without a pull.
      variants = lib.mapAttrs (label: img: { image = img.tarball; }) variantImages;
      variantLabels = lib.attrNames variants;
    };
  }
  ''
    mkdir -p $out
    ${lib.concatMapStrings emit published}
    # The prebuilt store holding every version above, for the node's additional image store.
    ln -s ${imageStore}/store.erofs $out/store.erofs

    # A minimal signed catalog beside each version, so a harness installs (and rolls) this the way
    # a user does. Seeding the node-local cache reproduces what an install records, not what it
    # does (no data subvolume, so the container cannot start). `fetchManifest` deliberately fails
    # closed with no keyring and verifies before parsing, so the honest answer is a real catalog,
    # not a weakened path. One key signs every catalog, as a node holds one keyring: a new version
    # must not need the node re-trusted.
    catalog-sign ${
      lib.concatMapStringsSep " " (p: "$out/${p.dir}/manifest.json $out/${p.dir}/catalog") published
    }
  ''
