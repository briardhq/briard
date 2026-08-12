# The nixosTest host driver, built from the repo's Go module. Uses the
# same platform + guestagent code the agent uses; buildGoModule vendors the whole
# module, so it shares the module-wide
# vendorHash (one definition, vendor-hash.nix).
{ buildGoModule }:
buildGoModule {
  pname = "briard-test-driver";
  version = "0.0.0";
  src = ../../.;
  vendorHash = import ../../vendor-hash.nix;
  subPackages = [ "nixosTest/driver" ];
  meta.description = "Briard nixosTest host driver";
}
