# The quadlet-render TEST HELPER, built from the real agent/quadlet + shared/manifest
# packages so service-install.nix drives the actual renderer rather than a hand-written copy of
# its output. Same Go module as the agent, so it shares the module-wide vendorHash
# (one definition, vendor-hash.nix).
{ buildGoModule }:
buildGoModule {
  pname = "quadlet-render";
  version = "0.0.0";
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ "nixosTest/quadlet-render" ];
  meta.description = "Render a Briard service manifest to quadlet units (test helper)";
}
