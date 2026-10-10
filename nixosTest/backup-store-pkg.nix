# The backup-store TEST HELPER, built from the real agent/host package so backup.nix backs up into
# the store that ships rather than a stand-in REST server. Same Go module as the agent, so it shares
# the module-wide vendorHash (one definition, vendor-hash.nix).
{ buildGoModule }:
buildGoModule {
  pname = "backup-store";
  version = "0.0.0";
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ "nixosTest/backup-store" ];
  meta.description = "Serve the host's backup store with no agent (test helper)";
}
