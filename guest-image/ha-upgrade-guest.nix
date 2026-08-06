# The HA upgrade-pair guest: HA boots on the `from` image (2025.11.0,
# recorder schema 51) with the `to` image (2025.12.0, schema 53) warm-staged as the
# upgrade target, so ha-upgrade.nix can drive the payload-upgrade primitives and
# prove the REAL recorder migration (v52 — statistics_meta.unit_class) survives with
# its data intact. Same payload slot as ha-guest.nix; only the image pair (from the
# fixture, guest-image/pkgs/home-assistant-image-pair) + the staged target differ.
{ pkgs, ... }:
{
  imports = [ ./configuration.nix ];
  briard.payload = {
    image = "ghcr.io/home-assistant/home-assistant:2025.11.0";
    imageFile = pkgs.home-assistant-upgrade-pair.from;
    dataDir = "/var/lib/briard/ha"; # HA data subvolume on the DRBD volume
    mountPath = "/config"; # HA's data dir inside the container
    # The upgrade target, resident and ready to pin — a pull on the upgrade path would
    # defeat the warm-standby model. `podman load`ed at boot by briard-payload-stage.
    stagedImages = [ pkgs.home-assistant-upgrade-pair.to ];
  };
}
