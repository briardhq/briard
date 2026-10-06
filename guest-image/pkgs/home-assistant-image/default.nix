# Home Assistant as a hermetically-pinned OCI image.
#
# The service is a *pinned* upstream container, not something we build: HA is
# Python + hundreds of integrations, out of scope to package ourselves. So we
# pull the official image by content digest (never a mutable tag) and let Nix
# reproduce it bit-for-bit — the same closure-pin discipline the dummy gets from
# dockerTools.buildImage, applied to a third-party image.
#
# Pin provenance (refresh procedure): `stable`/`latest` currently resolve to HA
# 2026.9.4. The digests below are the *per-arch* image manifests under that
# release's multi-arch index (sha256:3e67…6b76); pinning the arch-specific
# manifest (not the index) keeps the pull unambiguous and fully reproducible.
#   ghcr.io/home-assistant/home-assistant:stable  ->  index sha256:3e6710a7…6b76
#     amd64  sha256:e47c978e1b801466e7f62f612fd552bc3a228e077b31a3f1c22c05cf63d754da
#     arm64  sha256:35e6df56a9ce632c9b15df869ac73a17af6cdd2cfb99830527ffac9cc5218ba2
# To bump: resolve the new digest (skopeo inspect / the registry API), swap it in
# here, and set sha256 to lib.fakeSha256 once so the build prints the real FOD hash.
{ dockerTools, lib, stdenv }:

let
  version = "2026.9.4";

  # V0 builds the guest for x86_64 only (guest-image/disk-image.nix). arm64 is
  # recorded above for when the Pi target lands (uniform-VM model) — add its
  # sha256 and select on stdenv.hostPlatform then.
  amd64 = {
    imageDigest = "sha256:e47c978e1b801466e7f62f612fd552bc3a228e077b31a3f1c22c05cf63d754da";
    sha256 = "sha256-45ES3e7FHFli+U2/ChyJc6HJBvWJiCaIQXBIwU0XKfU=";
  };
in
assert lib.assertMsg stdenv.hostPlatform.isx86_64
  "home-assistant-image is pinned for x86_64 only in v0; add the arm64 sha256 to build on aarch64";
dockerTools.pullImage {
  imageName = "ghcr.io/home-assistant/home-assistant";
  inherit (amd64) imageDigest sha256;
  finalImageName = "ghcr.io/home-assistant/home-assistant";
  finalImageTag = version;
  os = "linux";
  arch = "amd64";
}
