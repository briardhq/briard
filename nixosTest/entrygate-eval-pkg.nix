# The entrygate-eval CLI: runs the real S1 health-gate verdict
# (agent/guest/entrygate) so ha-upgrade-rollback.nix can judge HA's real config-entry
# states instead of re-implementing the gate in Python. Same Go module as the agent +
# driver, so it shares their vendorHash (one definition, vendor-hash.nix).
{ buildGoModule }:
buildGoModule {
  pname = "entrygate-eval";
  version = "0.0.0";
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ "agent/guest/entrygate/cmd/entrygate-eval" ];
  meta.description = "Briard S1 health-gate verdict CLI (nixosTest)";
}
