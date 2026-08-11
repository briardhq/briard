# The entrygate-eval CLI: runs the real S1 health-gate verdict
# (agent/guest/entrygate) so ha-upgrade-rollback.nix can judge HA's real config-entry
# states instead of re-implementing the gate in Python. Same Go module as the agent +
# driver, so it shares their vendorHash (regenerate together on go.mod/go.sum changes).
{ buildGoModule }:
buildGoModule {
  pname = "entrygate-eval";
  version = "0.0.0";
  src = ../.;
  vendorHash = "sha256-4d/F5wfaBgNfrt0bv6IuElUAR/wVr7yG8BYOX0dSq6c=";
  subPackages = [ "agent/guest/entrygate/cmd/entrygate-eval" ];
  meta.description = "Briard S1 health-gate verdict CLI (nixosTest)";
}
