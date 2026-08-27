# The catalog-sign LAB HELPER, built from the real shared/manifest so a signed lab catalog is
# signed over the same bytes the product parses. Same Go module as the agent, so it shares the
# module-wide vendorHash (one definition, vendor-hash.nix).
{ buildGoModule }:
buildGoModule {
  pname = "catalog-sign";
  version = "0.0.0";
  src = ../..;
  vendorHash = import ../../vendor-hash.nix;
  subPackages = [ "nixosTest/catalog-sign" ];
  meta.description = "Sign a Briard service manifest into a minimal catalog (lab helper)";
}
