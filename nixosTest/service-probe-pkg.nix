# The service-probe TEST HELPER, built from the real agent/mosquitto package so services-pair.nix
# drives the actual S1 probe rather than a shell pipeline that resembles it. Same Go module as the
# agent, so it shares the module-wide vendorHash (one definition, vendor-hash.nix).
{ buildGoModule }:
buildGoModule {
  pname = "service-probe";
  version = "0.0.0";
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ "nixosTest/service-probe" ];
  meta.description = "Run a catalogued service's S1 probe against a container (test helper)";
}
