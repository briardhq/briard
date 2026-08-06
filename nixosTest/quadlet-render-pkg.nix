# The quadlet-render TEST HELPER, built from the real agent/quadlet + shared/manifest
# packages so service-install.nix drives the actual renderer rather than a hand-written copy of
# its output. Same Go module as the agent, so it shares the module-wide vendorHash (regenerate
# together on go.mod/go.sum changes).
{ buildGoModule }:
buildGoModule {
  pname = "quadlet-render";
  version = "0.0.0";
  src = ../.;
  vendorHash = "sha256-5qbgoM8+XGR9gxMHkq+WqNO9EqTTk+1s9bZ6poMBYQc=";
  subPackages = [ "nixosTest/quadlet-render" ];
  meta.description = "Render a Briard service manifest to quadlet units (test helper)";
}
