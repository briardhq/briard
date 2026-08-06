# The nixosTest host driver, built from the repo's Go module. Uses the
# same platform + guestagent code the agent uses; buildGoModule vendors the whole
# module, so it shares the agent's
# vendorHash. Regenerate together on go.mod/go.sum changes.
{ buildGoModule }:
buildGoModule {
  pname = "briard-test-driver";
  version = "0.0.0";
  src = ../../.;
  vendorHash = "sha256-5qbgoM8+XGR9gxMHkq+WqNO9EqTTk+1s9bZ6poMBYQc=";
  subPackages = [ "nixosTest/driver" ];
  meta.description = "Briard nixosTest host driver";
}
