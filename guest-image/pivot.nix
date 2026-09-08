# THE GUEST'S FROZEN PIVOT ([B.86j], re-cut by [B.138]): how the set of briard binaries pushed by
# the host takes over from what the guest runs, and how it falls back.
#
# Every briard binary the guest runs rides the HOST bundle. The image bakes ONE firmware binary,
# the guest agent -- rebuilt only with the image ([B.86i]), and the thing that receives the first
# push -- and the host dresses the guest with the release's set over the control channel
# (`bin.stage` / `bin.test` / `bin.activate`, agent/guestagent/bin.go) at every bring-up: the
# overlay the guest boots on is disposable, so every boot starts as firmware and nothing pushed
# survives a restart. The front door and the dashboard have NO firmware copy: before the first
# dress their units have nothing to exec and say so; the host dresses before rejoin, so nothing
# can promote a node that has not been dressed.
#
# This module is the guest-side half of that, the SAME shape as the host's own pivot
# (scripts/install.sh briard-exec / briard-commit, [B.84]), with ONE commit for the whole set:
#
#   <bin>/<name>.next    a pushed binary, verified by the agent (sha256 over the whole file) and
#                        proven by its own --test-launch before anything is armed
#   <run>/<name>.update  "trial <name>.next on the next start" -- single-use: the picker renames
#                        it to .trial, so a crashing candidate cannot re-trial forever
#   <run>/<name>.trial   "this start IS a trial"
#   <run>/<name>.ran     what the picker exec'd on this start: trial | pushed | baked -- the
#                        trial agent's verdict reads it, because a failed start is invisible to
#                        `systemctl try-restart` once systemd's auto-restart has succeeded
#   <bin>/<name>         the PUSHED, committed binary; absent on a fresh boot
#   <baked>              the firmware, the guest agent only; `-` for the doors (none)
#   <bin>/RELEASE        the host release id the committed set came from (the handshake's Bundle)
#
# ExecStart is the picker: trial -> pushed -> baked. bin.activate arms EVERY name's flag and
# restarts ONLY the agent's unit; the trial agent's start is the verdict on the set: it
# try-restarts each door that is running (their pickers, flags consumed, exec the staged files;
# Type=notify with a short start timeout, so systemctl's return IS the outcome), and only a
# passing verdict opens the port and says READY. The agent's ExecStartPost is the ONE commit --
# systemd runs it only after READY=1 -- and it moves every staged name plus RELEASE together.
#
# A door whose staged file fails has already reverted by then: its auto-restart (2 s, direct
# mode) finds the flag consumed and execs the committed file. One failed start out of the unit's
# budget -- the promotion hold fires on start-limit exhaustion only (configuration.nix,
# chainMemberFailure), so a failed upgrade NEVER demotes. The trial agent exits 1, its own picker
# brings the committed agent back, and that agent's start discards the staged set and puts both
# doors on the committed files (bin.go BinStartup), whichever of the three actually failed.
# No channel, no timer, no memory of what to undo beyond "staged files present".
#
# Frozen in the sense that matters: a pushed binary can change everything about itself except the
# picker it is started by, --test-launch, and the three verbs it is reached through -- those are
# the contract the firmware keeps, versioned additively.
{ lib, pkgs, config, ... }:
let
  # On the overlay ROOT, disposable by design -- and NOT under /var/lib/briard, which in the guest
  # is the replicated data volume: mounted only while promoted (files put there before vanish under
  # the mount) and unmountable while a binary runs from it (agent/guestagent/bin.go says how that
  # was measured).
  binDir = "/var/lib/briard-bin";
  runDir = "/run/briard-bin"; # tmpfs: the single-use flags
  # The set, in commit order -- the same names as bin.go BinNames, and the units the doors run
  # under. The agent's own name is the one whose trial flag its picker consumes.
  names = [ "briard-dashboard" "briard-reverse-proxy" "briard-guest-agent" ];
  doorUnits = "briard-dashboard.service briard-reverse-proxy.service";
  # briard-bin-exec <name> <baked|-> [args...]
  exec = pkgs.writeShellScript "briard-bin-exec" ''
    set -eu
    name=$1; baked=$2; shift 2
    # Each choice is said on stderr (the journal, forwarded to the console): a rig that watches a
    # dress go wrong reads the pivot's own account rather than inferring it from a handshake.
    if [ -e ${runDir}/$name.update ]; then
      mv ${runDir}/$name.update ${runDir}/$name.trial   # consume SINGLE-USE
      echo "briard-bin-exec: $name: TRIAL of ${binDir}/$name.next" >&2
      echo trial > ${runDir}/$name.ran
      exec ${binDir}/$name.next "$@"
    fi
    rm -f ${runDir}/$name.trial                          # a failed trial's marker: this IS the revert
    if [ -x ${binDir}/$name ]; then
      echo "briard-bin-exec: $name: pushed ${binDir}/$name" >&2
      # WHAT THIS START ACTUALLY RAN ([B.138]), for the trial agent's verdict. `systemctl
      # try-restart` alone is NOT enough: a staged copy that exits 1 fails its start, systemd's
      # own auto-restart brings the unit back on the COMMITTED binary seconds later, and the
      # restart JOB then succeeds -- so systemctl reports nothing wrong (measured on the first
      # install-macvtap run of this item: the verdict passed and committed a door that had
      # already reverted). The picker is the only thing that knows which file it exec'd.
      echo pushed > ${runDir}/$name.ran
      exec ${binDir}/$name "$@"
    fi
    if [ "$baked" = - ]; then
      echo "briard-bin-exec: $name: NOT DRESSED YET -- no committed binary and no firmware; the host dresses this guest before it can serve" >&2
      exit 1
    fi
    echo "briard-bin-exec: $name: baked $baked" >&2
    echo baked > ${runDir}/$name.ran
    exec "$baked" "$@"
  '';
  # briard-bin-commit: the agent unit's ExecStartPost, after READY. Only a TRIAL start commits
  # (the aftermath rule has already discarded a stale set before a non-trial start says READY),
  # and it commits the WHOLE staged set plus RELEASE; every flag is cleared, and the doors get
  # their start budget back -- the trial spent one of it where a door was running.
  commit = pkgs.writeShellScript "briard-bin-commit" ''
    set -eu
    if [ -e ${runDir}/briard-guest-agent.trial ]; then
      set=""
      for name in ${lib.concatStringsSep " " names}; do
        if [ -e ${binDir}/$name.next ]; then
          mv ${binDir}/$name.next ${binDir}/$name           # atomic same-fs commit
          set="$set $name"
        fi
      done
      if [ -e ${binDir}/RELEASE.next ]; then
        mv ${binDir}/RELEASE.next ${binDir}/RELEASE
      fi
      echo "briard-bin-commit: committed $(cat ${binDir}/RELEASE 2>/dev/null || echo '?'):$set" >&2
      systemctl reset-failed ${doorUnits} || true
    fi
    for name in ${lib.concatStringsSep " " names}; do
      rm -f ${runDir}/$name.trial ${runDir}/$name.update ${runDir}/$name.ran
    done
  '';
  cfg = config.briard.pivot;
in
{
  options.briard.pivot = {
    exec = lib.mkOption {
      type = lib.types.path;
      default = exec;
      readOnly = true;
      description = "The picker every dressed unit's ExecStart goes through: briard-bin-exec <name> <baked|-> [args].";
    };
    commit = lib.mkOption {
      type = lib.types.path;
      default = commit;
      readOnly = true;
      description = "The ONE commit, the guest agent unit's ExecStartPost (after READY): every staged name plus RELEASE.";
    };
    binDir = lib.mkOption {
      type = lib.types.str;
      default = binDir;
      readOnly = true;
    };
    preDressed = lib.mkOption {
      type = lib.types.attrsOf lib.types.path;
      default = { };
      description = ''
        TEST NODES ONLY: binaries linked into the pushed directory at boot as if a host had
        dressed this guest, name -> store path. The shipped image sets none: its doors exist
        only once pushed, and nixosTest machines built from configuration.nix alone have no host
        to push them (nixosTest/lib.nix).
      '';
    };
  };
  config = {
    # The dirs exist before either unit starts; the agent also creates binDir on the first push.
    systemd.tmpfiles.rules = [
      "d ${binDir} 0755 root root -"
      "d ${runDir} 0755 root root -"
    ] ++ lib.mapAttrsToList (name: path: "L+ ${binDir}/${name} - - - - ${path}") cfg.preDressed;
  };
}
