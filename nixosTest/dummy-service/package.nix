# Briard dummy stateful service (M0 test fixture), built from the repo's Go
# module. The binary itself is pure stdlib, but buildGoModule vendors the whole
# module, so this
# shares the module-wide vendorHash (one definition, vendor-hash.nix).
#
# It lives under nixosTest/: it is a fixture, and since the payload slot was deleted
# ([V3b.3](e2)) nothing in the product tree refers to it at all -- the tests install it the way a
# user installs a service.
{ buildGoModule }:
buildGoModule {
  pname = "briard-dummy-service";
  version = "0.0.0";
  src = ../../.;
  vendorHash = import ../../vendor-hash.nix;
  subPackages = [ "nixosTest/dummy-service" ];
  meta.description = "Briard dummy stateful service (failover/upgrade fixture)";
}
