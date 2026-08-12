# Briard dummy stateful service (M0 test fixture), built from the repo's Go
# module. The binary itself is pure stdlib, but buildGoModule vendors the whole
# module, so this
# shares the module-wide vendorHash (one definition, vendor-hash.nix).
#
# It lives under nixosTest/: it is a fixture, and once the payload slot
# became optional nothing in the product tree refers to it any more.
{ buildGoModule }:
buildGoModule {
  pname = "briard-dummy-service";
  version = "0.0.0";
  src = ../../.;
  vendorHash = import ../../vendor-hash.nix;
  subPackages = [ "nixosTest/dummy-service" ];
  meta.description = "Briard dummy stateful service (failover/upgrade fixture)";
}
