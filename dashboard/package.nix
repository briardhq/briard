# The household dashboard ([V3b.31b]), built from the repo's Go module. Pure stdlib + the module's
# own packages, but buildGoModule vendors the whole module, so it shares the module-wide
# vendorHash (one definition, vendor-hash.nix). Runs in the guest, promoter-owned, on loopback
# behind the front door.
{ buildGoModule }:
buildGoModule {
  pname = "briard-dashboard";
  version = "0.0.0";
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ "dashboard" ];
  meta.description = "Briard household dashboard (behind the front door at the VIP)";
}
