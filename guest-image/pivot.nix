# THE GUEST'S FROZEN PIVOT ([B.86j], re-cut by [B.138] and [B.139]): how the set of briard
# binaries pushed by the host takes over from what the guest runs, and how it falls back.
#
# Every briard binary the guest runs rides the HOST bundle. The image bakes ONE binary,
# `briard-guest-firmware` -- the push protocol and nothing else ([B.139]: the handshake, the three
# push verbs, os.poweroff), rebuilt only with the image ([B.86i]), and the thing that receives the
# first push. The host dresses the guest with the release's set over the control channel
# (`bin.stage` / `bin.test` / `bin.activate`, agent/guestfirmware/bin.go) at every bring-up: the
# overlay the guest boots on is disposable, so every boot starts as firmware and nothing pushed
# survives a restart. The guest AGENT, the front door and the dashboard have no baked copy at all;
# before the first dress the doors' units have nothing to exec and say so, and the host dresses
# before rejoin, so nothing can promote a node that has not been dressed.
#
# This module is the guest-side half of that -- the PICKER, and the files it chooses between. It
# is the same shape as the host's own pivot (scripts/install.sh briard-exec / briard-commit,
# [B.84]) minus the commit, which since [B.148] the agent does itself (see below):
#
#   <bin>/<name>.next    a pushed binary, verified by the firmware (sha256 over the whole file)
#                        and proven by its own --test-launch before anything is armed
#   <run>/<name>.update  "trial <name>.next on the next start" -- single-use: the picker DELETES
#                        it as it execs, so a crashing candidate cannot re-trial forever
#   <run>/<name>.ran     what the picker exec'd on this start: trial | pushed | baked. It answers
#                        both questions the rest of this asks -- "is this start a trial" (ran =
#                        trial) and "what is this unit actually running" -- the second because a
#                        failed start is invisible to `systemctl try-restart` once systemd's
#                        auto-restart has succeeded. There used to be a separate `.trial` flag
#                        saying the first; two files carrying one fact are two files that can
#                        disagree, so the marker absorbed it (owner, 2026-09-08)
#   <bin>/<name>         the PUSHED, committed binary; absent on a fresh boot
#   <baked>              the firmware, for the agent's unit alone; `-` for the doors (none)
#   <bin>/RELEASE        the host release id the committed set came from (the handshake's Bundle)
#
# ExecStart is the picker: trial -> pushed -> baked. bin.activate arms EVERY name's flag and
# restarts ONLY the agent's unit; the trial agent's start is the verdict on the set: it
# try-restarts each door that is running (their pickers, flags consumed, exec the staged files;
# Type=notify with a short start timeout, so systemctl's return IS the outcome), and only a
# passing verdict opens the port. It then COMMITS -- every staged name plus RELEASE, together --
# and only after that says READY and serves its first verb.
#
# ⚠️ THE COMMIT IS NOT A UNIT HOOK, and that is the fix of [B.148]. It was an `ExecStartPost`
# here (briard-bin-commit, a shell script systemd ran only after READY=1), which read as the
# tighter gate and was not: the host is outside the guest and cannot see READY, so what it
# actually waits on is the PORT -- opened before READY, and before systemd schedules any
# ExecStartPost. In that window the host handshakes, judges the guest dressed and sends bring-up
# verbs, and `briard-node-storage.service` execs `<bin>/briard-guest-agent` directly, which the
# commit has not created yet: measured 203/EXEC on the fleet tier 2026-09-11 (os-reboot.sh, run
# 34576181211, lost by 12 ms), rolling back a healthy OS upgrade. agent/guestfirmware/bin.go
# BinCommit carries the full account and what the move gives up.
#
# A door whose staged file fails has already reverted by then: its auto-restart (2 s, direct
# mode) finds the flag consumed and execs the committed file. One failed start out of the unit's
# budget -- the promotion hold fires on start-limit exhaustion only (configuration.nix,
# chainMemberFailure), so a failed upgrade NEVER demotes. The trial agent exits 1, its own picker
# brings the committed agent back -- or, when the FIRST dress is what failed and there is no
# committed agent yet, the firmware -- and that start discards the staged set and puts both doors
# on the committed files (agent/guestfirmware/bin.go BinStartup), whichever of the three actually
# failed. No channel, no timer, no memory of what to undo beyond "staged files present".
#
# Frozen in the sense that matters: a pushed binary can change everything about itself except the
# picker it is started by, --test-launch, and the verbs it is reached through -- those are the
# contract the firmware keeps, versioned additively. That contract is now also the image's
# CHANGE CONDITION: the firmware's import graph is the guest chain's input hash ([B.139]), so the
# guest image moves when the protocol does and not when the agent does.
{ lib, pkgs, config, ... }:
let
  # On the overlay ROOT, disposable by design -- and NOT under /var/lib/briard, which in the guest
  # is the replicated data volume: mounted only while promoted (files put there before vanish under
  # the mount) and unmountable while a binary runs from it (agent/guestfirmware/bin.go says how that
  # was measured).
  binDir = "/var/lib/briard-bin";
  runDir = "/run/briard-bin"; # tmpfs: the single-use flags
  # ⚠️ The SET is not listed here any more ([B.148]): the commit that used to walk it moved into
  # the agent, so guestfirmware/bin.go BinNames is now the only place the names and their order
  # live. This module cares about one name at a time -- whichever its picker was handed.
  # briard-bin-exec <name> <baked|-> [args...]
  exec = pkgs.writeShellScript "briard-bin-exec" ''
    set -eu
    name=$1; baked=$2; shift 2
    # Each choice is said on stderr (the journal, forwarded to the console): a rig that watches a
    # dress go wrong reads the pivot's own account rather than inferring it from a handshake.
    # WHAT THIS START RAN, written on every branch below ([B.138]). It is the ONLY record: it says
    # both "this start is a trial" (which the commit needs) and "this unit is running the staged
    # copy" (which the trial agent's verdict needs, because `systemctl try-restart` alone is not
    # enough -- a staged copy that exits 1 fails its start, systemd's own auto-restart brings the
    # unit back on the COMMITTED binary seconds later, and the restart JOB then succeeds, so
    # systemctl reports nothing wrong; measured on the first install-macvtap run of this item, where
    # the verdict passed and committed a door that had already reverted). Overwriting it IS the
    # revert: a failed trial's auto-restart lands below and the marker stops saying trial.
    if [ -e ${runDir}/$name.update ]; then
      rm -f ${runDir}/$name.update                       # consume SINGLE-USE: a crashing candidate cannot re-trial
      echo "briard-bin-exec: $name: TRIAL of ${binDir}/$name.next" >&2
      echo trial > ${runDir}/$name.ran
      exec ${binDir}/$name.next "$@"
    fi
    if [ -x ${binDir}/$name ]; then
      echo "briard-bin-exec: $name: pushed ${binDir}/$name" >&2
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
        dressed this guest, name -> store path. The shipped image sets none: only the firmware is
        baked, and nixosTest machines built from configuration.nix alone have no host to push
        them the rest (nixosTest/lib.nix).
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
