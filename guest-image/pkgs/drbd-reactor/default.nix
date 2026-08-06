{ lib, rustPlatform, fetchFromGitHub, drbd }:

# drbd-reactor is not in nixpkgs, so we package it ourselves.
# It is the failover orchestrator: its promoter plugin runs the ordered
# {DRBD-primary → workload → VIP} unit.
rustPlatform.buildRustPackage rec {
  pname = "drbd-reactor";
  version = "1.11.0";

  src = fetchFromGitHub {
    owner = "LINBIT";
    repo = "drbd-reactor";
    rev = "v${version}";
    hash = "sha256-eg9hRqGYVpXWjcp7anzUKleeDyygur/zaycXr0YQ2ME=";
  };

  cargoHash = "sha256-XoYRl5xRe3bPI3NWR3G5bPLqHD1MFfFYkNjfJm1KaSI=";

  # Pending-upstream fix: on a peer-less (single-node) resource, `drbdsetup show
  # --json` omits the `connections` key entirely, so the promoter's split-brain
  # detector fails to deserialize it and IGNOREs the resource on every event (its
  # reactive state goes stale — the single-node bug). Mark the field
  # #[serde(default)]. Submit upstream, then drop on the version bump that carries it.
  patches = [ ./0001-drbdstatus-default-connections-peerless.patch ];

  # drbd-reactor hardcodes the drbd-utils shim at the FHS path /lib/drbd in its
  # secondary-force drop-in templates; on Nix there is no such path. Substitute
  # the store path — the same fix nixpkgs' own drbd derivation applies to that
  # shim — so the drop-ins resolve hermetically, with no global /lib/drbd symlink.
  # (Upstream ought to make this path configurable rather than hardcoded.)
  postPatch = ''
    substituteInPlace src/plugin/promoter.rs \
      --replace-fail '/lib/drbd/scripts/drbd-service-shim.sh' \
                     '${drbd}/lib/drbd/scripts/drbd-service-shim.sh'
  '';

  meta = {
    description = "Daemon that reacts to DRBD state changes (promoter, prometheus, ...)";
    homepage = "https://github.com/LINBIT/drbd-reactor";
    license = lib.licenses.asl20;
    mainProgram = "drbd-reactor";
  };
}
