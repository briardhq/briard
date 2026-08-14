# Selects the dummy stateful fixture as the unit payload — the test-side sibling of
# guest-image/hass-payload.nix.
#
# The payload slot defaults to nothing, so this fixture has to be selected — and being selected
# only from nixosTest/ is what makes "the shipped artifact runs no service" a property the tree
# enforces rather than a claim.
#
# The image is built locally (no registry) and baked into whatever disk imports this, which is
# how a cold standby avoids paying a `podman load` on the failover path. A runtime service
# install pulls a digest instead, which is what will eventually retire this module.
{ pkgs, ... }:
{
  briard.payload = {
    image = "briard-dummy:v0";
    imageFile = pkgs.dockerTools.buildImage {
      name = "briard-dummy";
      tag = "v0";
      config.Cmd = [ "${pkgs.dummy-service}/bin/dummy-service" ];
    };
    dataDir = "/var/lib/briard/dummy"; # the fixture's data subvolume on the DRBD volume
    # Port + healthPath keep their defaults (:8080, /healthz) — the dummy is what they describe.
  };
}
