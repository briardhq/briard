# The nixosTest host driver, built from the repo's Go module. Uses the
# same platform + guestagent code the agent uses; buildGoModule vendors the whole
# module, so it shares the agent's
# vendorHash. Regenerate together on go.mod/go.sum changes.
{ buildGoModule }:
buildGoModule {
  pname = "briard-test-driver";
  version = "0.0.0";
  src = ../../.;
  vendorHash = "sha256-4d/F5wfaBgNfrt0bv6IuElUAR/wVr7yG8BYOX0dSq6c=";
  subPackages = [ "nixosTest/driver" ];
  meta.description = "Briard nixosTest host driver";
}
