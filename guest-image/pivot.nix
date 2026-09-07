# THE GUEST'S FROZEN PIVOT ([B.86j]): how a briard binary pushed by the host takes over from the
# one baked into the image, and how it falls back.
#
# Every briard binary the guest runs rides the HOST bundle. The image bakes a FIRMWARE copy of
# each -- rebuilt only with the image ([B.86i]) -- and the host dresses the guest with the
# release's copies over the control channel (`bin.stage` / `bin.activate`, agent/guestagent/
# bin.go) at every bring-up: the overlay the guest boots on is disposable, so every boot starts
# as firmware and nothing pushed survives a restart. Restart is the ultimate revert.
#
# This module is the guest-side half of that, and it is the SAME shape as the host's own pivot
# (scripts/install.sh briard-exec / briard-commit, [B.84]):
#
#   <bin>/<name>.next    a pushed binary, verified by the agent (sha256 over the whole file)
#   <run>/<name>.update  "trial <name>.next on the next start" -- single-use: the picker renames
#                        it to .trial, so a crashing candidate cannot re-trial forever
#   <run>/<name>.trial   "this start IS a trial -- commit .next when the unit says READY"
#   <bin>/<name>         the PUSHED, committed binary; absent on a fresh boot
#   <baked>              the firmware, always present, always last
#
# ExecStart is the picker: trial -> pushed -> baked. ExecStartPost is the commit, and systemd runs
# it ONLY after READY=1 (Type=notify; both binaries say READY once they LISTEN) -- so a pushed
# binary that will not exec, panics, or cannot bind never reaches it, the start fails, the next
# start finds the flag consumed and runs what it ran before. No channel, no timer, no memory of
# what to undo. The host reads the outcome in the next handshake (the guest reports the bundle
# it runs, `RELEASE`, committed by the guest agent's own commit as the LAST binary activated).
#
# Frozen in the sense that matters: a pushed binary can change everything about itself except the
# picker it is started by and the three verbs it is reached through (handshake, bin.stage,
# bin.activate) -- those are the contract the firmware keeps, versioned additively.
{ lib, pkgs, ... }:
let
  # On the overlay ROOT, disposable by design -- and NOT under /var/lib/briard, which in the guest
  # is the replicated data volume: mounted only while promoted (files put there before vanish under
  # the mount) and unmountable while a binary runs from it (agent/guestagent/bin.go says how that
  # was measured).
  binDir = "/var/lib/briard-bin";
  runDir = "/run/briard-bin"; # tmpfs: the single-use flags
  # briard-bin-exec <name> <baked> [args...]
  exec = pkgs.writeShellScript "briard-bin-exec" ''
    set -eu
    name=$1; baked=$2; shift 2
    # Each choice is said on stderr (the journal, forwarded to the console): a rig that watches a
    # dress go wrong reads the pivot's own account rather than inferring it from a handshake.
    if [ -e ${runDir}/$name.update ]; then
      mv ${runDir}/$name.update ${runDir}/$name.trial   # consume SINGLE-USE
      echo "briard-bin-exec: $name: TRIAL of ${binDir}/$name.next" >&2
      exec ${binDir}/$name.next "$@"
    fi
    rm -f ${runDir}/$name.trial                          # a failed trial's marker: this IS the revert
    if [ -x ${binDir}/$name ]; then
      echo "briard-bin-exec: $name: pushed ${binDir}/$name" >&2
      exec ${binDir}/$name "$@"
    fi
    echo "briard-bin-exec: $name: baked $baked" >&2
    exec "$baked" "$@"
  '';
  # briard-bin-commit <name> [--release]: after READY. --release also commits RELEASE.next, which
  # only the LAST binary activated (the guest agent) passes, so a half-applied set reports the old
  # bundle in the handshake.
  commit = pkgs.writeShellScript "briard-bin-commit" ''
    set -eu
    name=$1
    if [ -e ${runDir}/$name.trial ]; then
      echo "briard-bin-commit: $name: committing ${binDir}/$name.next ''${2:+($2)}" >&2
      mv ${binDir}/$name.next ${binDir}/$name             # atomic same-fs commit
      if [ "''${2:-}" = --release ] && [ -e ${binDir}/RELEASE.next ]; then
        mv ${binDir}/RELEASE.next ${binDir}/RELEASE
      fi
      rm -f ${runDir}/$name.trial
    fi
  '';
in
{
  options.briard.pivot = {
    exec = lib.mkOption {
      type = lib.types.path;
      default = exec;
      readOnly = true;
      description = "The picker every dressed unit's ExecStart goes through: briard-bin-exec <name> <baked> [args].";
    };
    commit = lib.mkOption {
      type = lib.types.path;
      default = commit;
      readOnly = true;
      description = "The commit every dressed unit runs as ExecStartPost (after READY): briard-bin-commit <name> [--release].";
    };
    binDir = lib.mkOption {
      type = lib.types.str;
      default = binDir;
      readOnly = true;
    };
  };
  config = {
    # The dirs exist before either unit starts; the agent also creates binDir on the first push.
    systemd.tmpfiles.rules = [
      "d ${binDir} 0755 root root -"
      "d ${runDir} 0755 root root -"
    ];
  };
}
