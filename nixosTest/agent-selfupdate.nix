# The host-agent self-update PIVOT — a frozen, agent-independent commit/revert
# mechanism gated purely by systemd `Type=notify`. This test proves the
# mechanism itself, hermetically (a single VM, NO nested guest): it installs the frozen
# briard-agent.service + the two dumb wrapper scripts, then drives stand-in trial binaries
# (nixosTest/briard-selfupdate-stub) through it and asserts the commit-or-revert outcome.
#
# The layout is flat — two on-disk binaries + ephemeral tmpfs decision flags:
#   /var/lib/briard/briard-agent        committed binary ExecStart runs (seeded on install)
#   /var/lib/briard/briard-agent.next   staged candidate on the SAME fs → commit = rename(2)
#   /run/briard/update                  tmpfs flag: "an update is armed — trial .next this boot"
#   /run/briard/trial                   tmpfs marker: "this boot IS a trial — commit on success"
#
# Why `Type=notify` IS the whole gate: the trial binary sends READY=1 only when healthy, so
# systemd treats the start as "succeeded" only then → ExecStartPost/briard-commit runs only
# on success. A crash OR a silent up-but-unhealthy hang (which never enters `failed`, so
# OnFailure= alone can't catch it) trips TimeoutStartSec → start fails → no commit. Revert is
# implicit + timerless: the trigger is single-use, so the next start finds no flag and
# briard-exec falls back to the committed binary.
#
# It also proves the signed-fetch path end-to-end (scenarios 5-6): a real
# agent/selfupdate.Fetcher pulls a signed artifact over HTTP, verifies it against an Ed25519
# keyring, and stages+arms it for the same pivot — a good signature commits, a tampered/unsigned
# one is refused with the committed binary kept. Still hermetic (the release host is a local
# http.FileServer in the stub; no nested guest — the guest-untouched half is the proof).
#
# Hermetic (one VM, TCG-friendly), so it rides the default `.#all`. Run one:
#   nix build .#tests.agent-selfupdate -L
{ pkgs, stub }:
let
  agentBin = "/var/lib/briard/briard-agent";
  nextBin = "/var/lib/briard/briard-agent.next";
  updateFlag = "/run/briard/update";
  trialMarker = "/run/briard/trial";

  stubExe = "${stub}/bin/briard-selfupdate-stub";
  # A "binary" on disk is a tiny script that execs the stub in a given mode/identity. The
  # frozen briard-exec just execs the path, so the mode has to live IN the file — and the
  # `exec` keeps the PID, so the stub's READY=1 comes from MAINPID (NotifyAccess=main, exactly
  # like the real agent). The identity string is what the test greps to prove which binary won.
  candidate = mode: id: pkgs.writeShellScript "briard-agent-${mode}${pkgs.lib.optionalString (id != "") "-${id}"}" ''
    exec ${stubExe} ${mode} ${id}
  '';
  readyV1 = candidate "ready" "v1"; # the initial committed binary
  readyV2 = candidate "ready" "v2"; # a good update
  readyV3 = candidate "ready" "v3"; # a good update used only in the power-loss case
  readyV4 = candidate "ready" "v4"; # the signed artifact fetched over HTTP
  evilCand = candidate "ready" "evil"; # different bytes, NOT signed by the release key (tamper)
  crashCand = candidate "crash" ""; # exits 1 immediately → start fails → revert
  hangCand = candidate "hang" ""; # blocks without READY → TimeoutStartSec → revert

  # The two frozen wrappers — dumb shell, agent-INDEPENDENT (a bug in the volatile agent can
  # never wedge the update mechanism), verbatim.
  briardExec = pkgs.writeShellScript "briard-exec" ''
    set -eu
    if [ -e ${updateFlag} ]; then
        mv ${updateFlag} ${trialMarker}   # consume SINGLE-USE (rename, not delete): a crash
        exec ${nextBin}                   #   can't re-trial forever, and briard-commit can
    else                                  #   still tell a trial boot from a normal one
        rm -f ${trialMarker}              # discard a failed trial's marker — this IS the revert
        exec ${agentBin}
    fi
  '';
  briardCommit = pkgs.writeShellScript "briard-commit" ''
    set -eu
    if [ -e ${trialMarker} ]; then
        mv ${nextBin} ${agentBin}         # atomic same-fs commit
        rm -f ${trialMarker}
    fi
  '';
in
pkgs.testers.runNixOSTest {
  name = "agent-selfupdate";
  skipTypeCheck = true; # dynamic asserts

  nodes.machine =
    { ... }:
    {
      # The tmpfs decision flags live under /run/briard (cleared on every boot — that is what
      # makes a power loss mid-trial revert to the committed binary for free); the two binaries
      # under /var/lib/briard persist. Neither is a systemd Runtime/StateDirectory: those would
      # be wiped/recreated across the unit's own stop→start and clobber an externally-armed flag.
      systemd.tmpfiles.rules = [
        "d /var/lib/briard 0755 root root -"
        "d /run/briard 0755 root root -"
      ];
      environment.systemPackages = [ pkgs.curl ]; # probe the release HTTP host is up

      # The FROZEN pivot: it does not self-update (changing it is a rare base-install update),
      # so bugs in the volatile agent can't touch the mechanism. Type=notify + ExecStartPost is
      # the entire gate. wantedBy=[] so the testScript controls start ordering after seeding.
      systemd.services.briard-agent = {
        description = "Briard host agent (self-update pivot)";
        wantedBy = [ ];
        serviceConfig = {
          Type = "notify";
          NotifyAccess = "main"; # the trial binary signals READY from MAINPID, as the agent does
          # Production uses TimeoutStartSec=30, and it bounds a CONFIG READ rather than a
          # convergence: V3.32 moved READY to loop entry, because a supervisor's readiness is not
          # the health of the thing it supervises. This comment used to say >=180 for exactly the
          # reason that changed — READY once waited for the node to be healthy. Shortened further
          # here so the up-but-unhealthy HANG assertion resolves in seconds; the mechanism it
          # exercises (timeout trips → start fails → no commit) is identical at any value.
          #
          # The wrappers below are the pair install.sh now writes (B.84). Change one, change both
          # — and install-macvtap.nix proves the SHIPPED pair, which this test structurally cannot.
          TimeoutStartSec = 15;
          ExecStart = "${briardExec}"; # pick committed vs trial binary
          ExecStartPost = "${briardCommit}"; # runs ONLY after READY=1 → commit on success
          Restart = "always";
          RestartSec = 1;
          StartLimitIntervalSec = 0; # one failed trial then a revert must never latch as dead
        };
      };
    };

  testScript = ''
    machine.wait_for_unit("multi-user.target")

    def committed():
        return machine.succeed("cat ${agentBin}")

    def arm(candidate_store_path):
        # Stage a candidate + arm the trigger, exactly as the proven old agent / helper does
        # (selfupdate.StageNext + Arm) — an atomic install then the tmpfs flag.
        machine.succeed(f"install -m755 {candidate_store_path} ${nextBin}")
        machine.succeed("touch ${updateFlag}")

    # Seed the committed binary (install-time) and start the frozen unit on it.
    machine.succeed("install -m755 ${readyV1} ${agentBin}")
    machine.systemctl("start briard-agent.service")
    machine.wait_for_unit("briard-agent.service")
    assert " v1" in committed(), f"seed failed, committed={committed()!r}"
    print("seeded + started on committed v1")

    # === 1) A GOOD update COMMITS: briard-agent.next → briard-agent, guest untouched. ===
    arm("${readyV2}")
    machine.succeed("systemctl restart briard-agent.service")
    machine.wait_for_unit("briard-agent.service")
    machine.wait_until_succeeds("grep -q ' v2' ${agentBin}", timeout=30)
    machine.succeed("journalctl -u briard-agent | grep -q 'mode=ready id=v2'")  # the trial actually ran
    machine.fail("test -e ${nextBin}")     # candidate was renamed away → committed
    machine.fail("test -e ${trialMarker}") # briard-commit cleared it
    assert " v2" in committed(), f"good update did NOT commit, committed={committed()!r}"
    print("1) good update committed v2")

    # === 2) A CRASH-looping candidate REVERTS: no READY → start fails → no commit. ===
    arm("${crashCand}")
    machine.succeed("systemctl restart briard-agent.service || true")  # the trial start fails
    # Restart=always revives the unit; with the flag consumed, briard-exec runs the committed v2.
    machine.wait_for_unit("briard-agent.service", timeout=60)
    machine.wait_until_succeeds("grep -q ' v2' ${agentBin}", timeout=30)
    machine.succeed("journalctl -u briard-agent | grep -q 'mode=crash'")  # the crash candidate ran
    machine.succeed("test -e ${nextBin}")  # NOT committed — still staged, inert
    assert " v2" in committed(), f"crash candidate was wrongly committed, committed={committed()!r}"
    assert "crash" not in committed(), f"crash candidate leaked into committed, committed={committed()!r}"
    print("2) crash candidate reverted to v2")

    # === 3) An up-but-UNHEALTHY HANG REVERTS: never READY → TimeoutStartSec trips (the case
    #        OnFailure= alone can't catch, because a hang never enters `failed`). ===
    arm("${hangCand}")
    machine.succeed("systemctl restart briard-agent.service || true")
    # Prove it was the TIMEOUT (not a crash) that failed the start, then that it reverted.
    machine.wait_until_succeeds("journalctl -u briard-agent | grep -qi 'timed out'", timeout=60)
    machine.succeed("journalctl -u briard-agent | grep -q 'mode=hang'")  # the hang candidate ran
    machine.wait_for_unit("briard-agent.service", timeout=60)
    machine.wait_until_succeeds("grep -q ' v2' ${agentBin}", timeout=30)
    machine.succeed("test -e ${nextBin}")  # NOT committed
    assert " v2" in committed(), f"hang candidate was wrongly committed, committed={committed()!r}"
    print("3) hang candidate reverted to v2 via TimeoutStartSec")

    # === 4) POWER LOSS mid-arm REVERTS, and no revert code runs: the tmpfs decision flag is
    #        gone at boot, so briard-exec simply runs the committed binary. Deterministic —
    #        armed but never trialed, so no race against a would-be-good candidate committing. ===
    machine.succeed("install -m755 ${readyV3} ${nextBin}")
    machine.succeed("touch ${updateFlag}")  # armed
    machine.succeed("systemctl stop briard-agent.service")
    machine.succeed("rm -rf /run/briard")   # a reboot clears tmpfs...
    machine.succeed("mkdir -p /run/briard") # ...and tmpfiles recreates it empty
    machine.systemctl("start briard-agent.service")
    machine.wait_for_unit("briard-agent.service")
    assert " v2" in committed(), f"power loss did not revert, committed={committed()!r}"
    machine.fail("grep -q ' v3' ${agentBin}")  # the armed-but-lost update never committed
    machine.succeed("test -e ${nextBin}")      # v3 stays inert on disk (safe direction)
    machine.fail("test -e ${trialMarker}")     # no trial marker → no revert code path ran
    print("4) power loss mid-arm ran committed v2, no commit, no revert code")

    # === 5) END-TO-END: a SIGNED artifact fetched over HTTP is verified, staged, trialed
    #        through the frozen pivot, and committed. The fetch+verify+stage+arm is the REAL
    #        agent/selfupdate code (not a manual install), driven against a live HTTP release host
    #        and a real Ed25519 keyring. ===
    machine.succeed("mkdir -p /srv /etc/briard")
    # A fresh release keypair; the public half becomes the node's trusted keyring.
    machine.succeed("${stubExe} keygen /root/release.key /etc/briard/keyring.pem")
    # Publish the new agent artifact (a ready-v4 launcher) and sign its EXACT served bytes.
    machine.succeed("cp ${readyV4} /srv/briard-agent")
    sig = machine.succeed("${stubExe} sign /root/release.key /srv/briard-agent").strip()
    machine.succeed("systemd-run --unit=release-httpd --collect ${stubExe} serve 127.0.0.1:8099 /srv")
    machine.wait_until_succeeds("curl -sf http://127.0.0.1:8099/briard-agent -o /dev/null", timeout=30)
    # The agent fetches → verifies → stages → arms (real selfupdate.Fetcher).
    machine.succeed(
        f"${stubExe} fetch http://127.0.0.1:8099/briard-agent '{sig}' /etc/briard/keyring.pem /var/lib/briard /run/briard"
    )
    machine.succeed("test -e ${nextBin}")    # verified → staged
    machine.succeed("test -e ${updateFlag}") # and armed
    # Trial it through the frozen pivot → commit.
    machine.succeed("systemctl restart briard-agent.service")
    machine.wait_for_unit("briard-agent.service")
    machine.wait_until_succeeds("grep -q ' v4' ${agentBin}", timeout=30)
    machine.fail("test -e ${nextBin}") # committed (renamed away)
    assert " v4" in committed(), f"signed fetch did NOT commit, committed={committed()!r}"
    print("5) signed artifact fetched over HTTP, verified, trialed, committed v4")

    # === 6) REFUSE-AND-STAY end-to-end: a TAMPERED artifact (valid-looking but not signed
    #        by the release key) and an UNSIGNED fetch are both refused — verify fails before any
    #        disk write, so nothing stages and the committed binary is kept.
    #        [[verification-assertions-must-fail]] — the refusal must actually fire. ===
    machine.succeed("cp ${evilCand} /srv/briard-agent-evil")  # different bytes; the v4 sig won't match them
    machine.fail(
        f"${stubExe} fetch http://127.0.0.1:8099/briard-agent-evil '{sig}' /etc/briard/keyring.pem /var/lib/briard /run/briard"
    )
    machine.fail("test -e ${nextBin}")    # tampered → nothing staged
    machine.fail("test -e ${updateFlag}") # nothing armed
    assert " v4" in committed(), f"a refused fetch changed the committed binary, committed={committed()!r}"
    # An UNSIGNED fetch (empty signature) is refused too. (Single-quoted Python string with a
    # shell "" empty arg, to avoid an empty single-quote pair the Nix string would read as a close.)
    machine.fail(
        '${stubExe} fetch http://127.0.0.1:8099/briard-agent "" /etc/briard/keyring.pem /var/lib/briard /run/briard'
    )
    machine.fail("test -e ${nextBin}")
    assert " v4" in committed(), f"an unsigned fetch changed the committed binary, committed={committed()!r}"
    print("6) tampered + unsigned artifacts refused over HTTP — committed v4 kept (refuse-and-stay)")

    print("the frozen Type=notify pivot commits good updates (incl. signed HTTP fetch) and reverts/refuses broken, lost, or unsigned ones")
  '';
}
