# The hass-nudge TEST HELPER, built from the real agent/hass package so hass-payload.nix drives the
# actual push half of the control channel rather than a curl that resembles it ([B.131]). Same Go
# module as the agent, so it shares the module-wide vendorHash (one definition, vendor-hash.nix).
{ buildGoModule }:
buildGoModule {
  pname = "hass-nudge";
  version = "0.0.0";
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ "nixosTest/hass-nudge" ];
  meta.description = "Fire briard's reconsider event at the node's Home Assistant (test helper)";
}
