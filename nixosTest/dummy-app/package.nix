# Briard dummy stateful app (test fixture), built from the repo's Go module. The binary is pure
# stdlib, but buildGoModule vendors the whole module, so it shares the module-wide vendorHash (one
# definition, vendor-hash.nix). It lives under nixosTest/: it is a fixture, nothing in the product
# tree refers to it, and the tests install it the way a user installs an app.
{ buildGoModule }:
buildGoModule {
  pname = "briard-dummy-app";
  version = "0.0.0";
  src = ../../.;
  vendorHash = import ../../vendor-hash.nix;
  subPackages = [ "nixosTest/dummy-app" ];
  meta.description = "Briard dummy stateful app (failover/upgrade fixture)";
}
