# Selects Home Assistant as the unit payload.
#
# Imported alongside configuration.nix (see ./ha-guest.nix) by the HA guest
# variant and the HA tests. The default guest stays on the dummy, so the DRBD /
# upgrade *mechanism* tests keep running on the deterministic fixture; HA rides
# the identical promoter/VIP/snapshot slot, only its image + data location differ.
#
# Data placement: HA's /config — `.storage` + the recorder
# SQLite + YAML — lives on the DRBD btrfs subvolume, so it replicates and
# snapshots as one unit. HA's image sets WorkingDir /config and Entrypoint /init
# (s6-overlay), so the slot needs no cmd override.
{ pkgs, ... }:
{
  briard.payload = {
    image = "ghcr.io/home-assistant/home-assistant:2026.7.1";
    imageFile = pkgs.home-assistant-image;
    dataDir = "/var/lib/briard/ha"; # the HA data subvolume on the DRBD volume
    mountPath = "/config"; # HA's data dir inside the container
    port = 8123; # HA's listen port — what the front door proxies to
    # HA has no /healthz. /manifest.json is static and unauthenticated, so a 200 means HA's
    # HTTP stack is up — the same readiness signal ha-payload.nix waits on, rather than `/`,
    # which redirects into onboarding/auth depending on how far along the instance is.
    healthPath = "/manifest.json";
  };
}
