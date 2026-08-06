# The guest image with Home Assistant as the payload — the base unit image
# plus the HA payload selection. Used as `guestModule` by the HA tests and as the
# system for the HA disk-image variant, mirroring how the default guest composes.
{ ... }:
{
  imports = [
    ./configuration.nix
    ./ha-payload.nix
  ];
}
