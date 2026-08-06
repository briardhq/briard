# The briard-backup CLI: a thin wrapper over shared/backup (the same code the
# guest agent's backup.save/backup.restore verbs run) so ha-backup.nix can exercise the
# encrypted .storage backup + restore in a lib.nix rig (which can't run the virtio-serial
# Manager). Same Go module as the agent, so it shares the module-wide vendorHash
# (regenerate together on go.mod/go.sum changes).
{ buildGoModule }:
buildGoModule {
  pname = "briard-backup";
  version = "0.0.0";
  src = ../.;
  vendorHash = "sha256-5qbgoM8+XGR9gxMHkq+WqNO9EqTTk+1s9bZ6poMBYQc=";
  subPackages = [ "shared/backup/cmd/briard-backup" ];
  meta.description = "Briard encrypted .storage backup CLI";
}
