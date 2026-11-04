# The app-probe TEST HELPER, built from the real agent/mosquitto package so apps-pair.nix
# drives the actual S1 probe rather than a shell pipeline that resembles it. Same Go module as the
# agent, so it shares the module-wide vendorHash (one definition, vendor-hash.nix).
{ buildGoModule }:
buildGoModule {
  pname = "app-probe";
  version = "0.0.0";
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ "nixosTest/app-probe" ];
  meta.description = "Run a catalogued service's S1 probe against a container (test helper)";
}
