# Mosquitto as a hermetically-pinned OCI image, in the two versions the upgrade tests need
# ([V3b.4]).
#
# Same discipline as ./home-assistant-image: pull the official container by per-arch content
# digest, never a mutable tag, so Nix reproduces it bit-for-bit. A pair rather than a single pin
# because an upgrade test needs a real version change on both sides of the switch — the identity
# a node upgrades to is the manifest's content hash, and a manifest naming the same image is not
# an upgrade at all.
#
# ── Why THIS pair (measured 2026-08-30, not reasoned) ────────────────────────────────────
#   from = 2.1.1
#   to   = 2.1.2   (what the published catalog entry pins; `:2` resolves here today)
#
# Both were run under Briard's own config before the pair was written, and both answer
# `/api/v1/listeners` on the loopback management listener. That matters: the health floor probes
# the same endpoint on both sides of the switch, so an upgrade test that failed would be reporting
# on the PIPELINE rather than on a broker that changed its API mid-pair.
#
# Nothing about the broker's storage format changes across it, and that is deliberate too. The
# recorder-schema straddle the HA pair exists for (./home-assistant-image-pair) is HA's problem;
# what the broker's upgrade has to prove is that retained state written before the switch is still
# there after it, and rolls back with the snapshot if the switch is reverted.
#
# ── Pin provenance / refresh procedure ───────────────────────────────────────────────────
# Per-arch amd64 manifest digests under each release's multi-arch index, resolved 2026-08-30 from
# the registry (`docker manifest inspect eclipse-mosquitto:<tag>`). v0 is x86_64-only (see
# ./home-assistant-image); arm64 waits for the Pi target. To (re)compute the FOD `sha256`: leave
# it `lib.fakeSha256`, run `nix build .#mosquitto-image-2_1_2` (and `..._2_1_1`), and paste the
# hash the build prints back here.
{ dockerTools, lib, stdenv }:

let
  imageName = "docker.io/library/eclipse-mosquitto";
  mk =
    { version, imageDigest, sha256 }:
    assert lib.assertMsg stdenv.hostPlatform.isx86_64
      "mosquitto-image is pinned for x86_64 only in v0; add the arm64 sha256 to build on aarch64";
    dockerTools.pullImage {
      inherit imageName imageDigest sha256;
      finalImageName = imageName;
      finalImageTag = version;
      os = "linux";
      arch = "amd64";
    };
in
{
  from = mk {
    version = "2.1.1";
    imageDigest = "sha256:d373dffb3185c95a34e0a693667fbdab7d2dc4b676dd72e8a0a6891d50be0c64";
    sha256 = "sha256-lP7XAy0lYDAUgTFTLeOli3uhKWz0cClRxKgpBn4/SvY=";
  };
  to = mk {
    version = "2.1.2";
    imageDigest = "sha256:6dba0f1b2795ddcbe0d41bdfb8b8d56a423acca23ccde4342a4652be54639b11";
    sha256 = "sha256-D85jVUinf8xZIuYYA9PaIbMpqOv95SYwPdPgEcvttlc=";
  };
}
