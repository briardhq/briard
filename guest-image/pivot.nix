# The guest's frozen pivot: how the set of briard binaries pushed by the host takes over from
# what the guest runs, and how it falls back.
#
# Every briard binary the guest runs rides the host bundle. The image bakes one binary,
# `briard-guest-firmware`: the push protocol and nothing else (the handshake, the three push
# verbs, os.poweroff), rebuilt only with the image; it receives the first push. At every
# bring-up the host dresses the guest with the release set over the control channel
# (`bin.stage` / `bin.test` / `bin.activate`, agent/guestfirmware/bin.go) into the boot scratch
# on the state disk, emptied at every boot, so every boot starts as firmware and nothing pushed
# survives a restart. The guest agent, reverse proxy and dashboard have no baked copy; before the
# first dress the web units have nothing to exec and say so, and the host dresses before rejoin,
# so nothing can promote an undressed node.
#
# This module is the guest-side half: the picker and the files it chooses between, the same idea
# as the host self-update (a .next beside the committed binary) minus the commit, which the agent
# does itself:
#
#   <bin>/<name>.next    a pushed binary, verified by the firmware (sha256 over the whole file)
#                        and proven by its own --test-launch before anything is armed
#   <run>/<name>.update  "trial <name>.next on the next start"; single-use: the picker deletes
#                        it as it execs, so a crashing candidate cannot re-trial forever
#   <run>/<name>.ran     what the picker exec'd on this start: trial | pushed | baked. One file
#                        answers both "is this start a trial" and "what is this unit actually
#                        running" (a failed start is invisible to `systemctl try-restart` once
#                        the systemd auto-restart has succeeded); two files carrying one fact can
#                        disagree
#   <bin>/<name>         the pushed, committed binary; absent on a fresh boot
#   <baked>              the firmware, for the agent unit alone; `-` for the web units (none)
#   <bin>/RELEASE        the host release id the committed set came from (the handshake Bundle)
#
# ExecStart is the picker: trial -> pushed -> baked. bin.activate arms every name's flag and
# restarts only the agent unit, whose trial start is the verdict on the set: it try-restarts each
# running web unit (their pickers, flags consumed, exec the staged files; Type=notify with a short
# start timeout, so the systemctl return is the outcome), opens the port only on a passing
# verdict, then commits every staged name plus RELEASE together, and only then says READY and
# serves its first verb.
#
# ⚠️ The commit is deliberately not a unit hook. An `ExecStartPost` (run only after READY=1) reads
# as the tighter gate and is not: the host, outside the guest, cannot see READY and waits on the
# port, opened before READY and before any ExecStartPost. In that window the host handshakes,
# judges the guest dressed and sends bring-up verbs, and `briard-node-storage.service` execs
# `<bin>/briard-guest-agent` directly, which such a commit has not created yet: 203/EXEC, by
# milliseconds, rolling back a healthy OS upgrade. agent/guestfirmware/bin.go BinCommit has the
# full account and what committing in the agent gives up.
#
# A web unit whose staged file fails has already reverted by then: its auto-restart (2 s, direct
# mode) finds the flag consumed and execs the committed file. That costs one failed start of the
# unit budget, and the promotion hold fires on start-limit exhaustion only
# (agent/guestagent/units.go, chainMemberFailure), so a failed upgrade never demotes. The trial
# agent exits 1; its picker brings back the committed agent (or the firmware, when the first
# dress failed and none is committed), and that start discards the staged set and puts both web
# units on the committed files (agent/guestfirmware/bin.go BinStartup), whichever of the three
# failed. No channel, no timer, no memory of what to undo beyond "staged files present".
#
# Frozen in the sense that matters: a pushed binary can change everything about itself except
# the picker that starts it, --test-launch, and the verbs it is reached through, the contract the
# firmware keeps, versioned additively. That contract is also the image change condition: the
# firmware import graph is the guest chain input hash, so the image moves when the protocol
# does, not when the agent does.
{ lib, pkgs, config, ... }:
let
  # In /var, the per-boot scratch on the state disk (disk-image.nix), not under /var/lib/briard,
  # which in the guest is the replicated data volume: mounted only while promoted (files put
  # there before vanish under the mount) and unmountable while a binary runs from it
  # (agent/guestfirmware/bin.go).
  binDir = "/var/lib/briard-bin";
  runDir = "/run/briard-bin"; # tmpfs: the single-use flags and the .ran markers
  # ⚠️ The set is not listed here: the agent commits it, so guestfirmware/bin.go BinNames is the
  # only place the names and their order live. This module handles one name at a time, whichever
  # its picker was handed.
  # briard-bin-exec <name> <baked|-> [args...]
  # A package, not a bare script: the pushed agent writes the units that go through the picker
  # and can only name a fixed path, so the picker joins the image tool profile under its own name
  # (configuration.nix, guestTools) instead of being a store path interpolated into a baked unit.
  exec = pkgs.writeShellScriptBin "briard-bin-exec" ''
    set -eu
    name=$1; baked=$2; shift 2
    # Each choice is said on stderr (the journal, forwarded to the console), so a rig watching a
    # dress go wrong reads the pivot's own account instead of inferring it from a handshake.
    # What this start ran is written on every branch below, the only record of both "this start
    # is a trial" (the commit needs it) and "this unit runs the staged copy" (the trial agent's
    # verdict needs it: a staged copy that exits 1 fails its start, systemd's own auto-restart
    # brings the unit back on the committed binary seconds later and the restart job succeeds,
    # so `systemctl try-restart` reports nothing wrong, and without the marker the verdict
    # passes and commits a web unit that has already reverted). Overwriting it is the revert:
    # a failed trial's auto-restart lands below and the marker stops saying trial.
    if [ -e ${runDir}/$name.update ]; then
      rm -f ${runDir}/$name.update                       # consume, single-use: a crashing candidate cannot re-trial
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
      type = lib.types.package;
      default = exec;
      readOnly = true;
      description = "The picker every dressed unit's ExecStart goes through, as a package providing bin/briard-bin-exec <name> <baked|-> [args].";
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
