# Home Assistant as a hermetically-pinned OCI image.
#
# The service is a *pinned* upstream container, not something we build: HA is
# Python + hundreds of integrations, out of scope to package ourselves. So we
# pull the official image by content digest (never a mutable tag) and let Nix
# reproduce it bit-for-bit — the same closure-pin discipline the dummy gets from
# dockerTools.buildImage, applied to a third-party image.
#
# Pin provenance (refresh procedure): `stable`/`latest` currently resolve to HA
# 2026.7.1. The digests below are the *per-arch* image manifests under that
# release's multi-arch index (sha256:f735…ff3cb); pinning the arch-specific
# manifest (not the index) keeps the pull unambiguous and fully reproducible.
#   ghcr.io/home-assistant/home-assistant:stable  ->  index sha256:f73512ba…ff3cb
#     amd64  sha256:21e0d1bae299819d8cf4ef8aa197593205a5fae51c69031c13bfd1eac8c56204
#     arm64  sha256:480eaf9b62d28f45c30cfb69f37b49573945937294529c2a59a67d5b7406fc5c
# To bump: resolve the new digest (skopeo inspect / the registry API), swap it in
# here, and set sha256 to lib.fakeSha256 once so the build prints the real FOD hash.
{ dockerTools, lib, stdenv }:

let
  version = "2026.7.1";

  # V0 builds the guest for x86_64 only (guest-image/disk-image.nix). arm64 is
  # recorded above for when the Pi target lands (uniform-VM model) — add its
  # sha256 and select on stdenv.hostPlatform then.
  amd64 = {
    imageDigest = "sha256:21e0d1bae299819d8cf4ef8aa197593205a5fae51c69031c13bfd1eac8c56204";
    sha256 = "sha256-MbYqE3XBieKDCquv+VpyIAGJZUhvzZ6cTF/GG+hxlRA=";
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
