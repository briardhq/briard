# The host-agent self-update PIVOT — a frozen, agent-independent commit/revert
# mechanism gated purely by systemd `Type=notify` — and, since [B.86a], the frozen UPDATE UNIT
# below the agent that feeds it. This test proves both hermetically (a single VM, NO nested
# guest): it installs the frozen briard-agent.service + briard-update.service and the three dumb
# wrapper scripts, then drives stand-in trial binaries (nixosTest/briard-selfupdate-stub) through
# them and asserts the commit-or-revert outcome.
#
# The layout is flat — two on-disk binaries, their manifests, and ephemeral tmpfs flags/messages:
#   /var/lib/briard/briard-agent        committed binary ExecStart runs (seeded on install)
#   /var/lib/briard/briard-agent.next   staged candidate on the SAME fs → commit = rename(2)
#   /var/lib/briard/manifest.json       the committed release's signed manifest (+ .next beside it)
#   /var/lib/briard/briard-net-wrap     the launch shim, cattle riding with the agent (+ .next)
#   /var/lib/briard/qemu -> qemu-<rel>/ a LINK to the committed qemu tree (+ qemu.next, a link)
#   /run/briard/update                  tmpfs flag: "an update is armed — trial .next this boot"
#   /run/briard/trial                   tmpfs marker: "this boot IS a trial — commit on success"
#   /run/briard/update-target|-result   the two messages between a trigger and the update unit
#
# Why `Type=notify` IS the whole gate: the trial binary sends READY=1 only when healthy, so
# systemd treats the start as "succeeded" only then → ExecStartPost/briard-commit runs only
# on success. A crash OR a silent up-but-unhealthy hang (which never enters `failed`, so
# OnFailure= alone can't catch it) trips TimeoutStartSec → start fails → no commit. Revert is
# implicit + timerless: the trigger is single-use, so the next start finds no flag and
# briard-exec falls back to the committed binary.
#
# Why the FETCH lives below the agent (scenarios 5-8): the updater must not be shipped by the
# thing it updates. briard-update.service pulls a FRESH agent from the channel's pointer and
# lets THAT binary do the Ed25519-verified fetch (`--fetch-update`), stage and arm — so a fetch
# bug in the committed binary cannot prevent its own replacement. Here the channel is the real
# tree ([B.86e]) served by the stub's http.FileServer, the manifests are written by the REAL
# `--stage-manifest`, the pointer's bootstrap is the REAL agent, and the artifact it stages is a
# stub candidate (so the pivot can still be driven through crash/hang). That one divergence —
# the pointer's briard-agent and the versioned briard-agent are different bytes — is what makes
# the mechanism testable without a nested guest; publish-release.sh verify refuses it on a real
# channel. install-macvtap.nix proves the SHIPPED scripts and units on a real install.
#
# The HOST BUNDLE ([B.86b], scenarios 5, 8, 9): a release is {agent, net-wrap, qemu}, staged as
# .next siblings and committed TOGETHER by briard-commit (qemu via `mv -T` of a link), with only
# the entries whose hash changed fetched. The stub candidates send READY without a smoke test,
# so this file proves the stage/commit/discard mechanics; the candidate's smoke test of a staged
# qemu on a real tree is install-macvtap.nix's, on the real agent.
#
# Hermetic (one VM, TCG-friendly), so it rides the default `.#all`. Run one:
#   nix build .#tests.agent-selfupdate -L
{ pkgs, stub, agent }:
let
  agentBin = "/var/lib/briard/briard-agent";
  nextBin = "/var/lib/briard/briard-agent.next";
  manifest = "/var/lib/briard/manifest.json";
  nextManifest = "/var/lib/briard/manifest.json.next";
  netWrap = "/var/lib/briard/briard-net-wrap";
  nextNetWrap = "/var/lib/briard/briard-net-wrap.next";
  qemuLink = "/var/lib/briard/qemu";
  nextQemu = "/var/lib/briard/qemu.next";
  updateFlag = "/run/briard/update";
  trialMarker = "/run/briard/trial";
  targetMsg = "/run/briard/update-target";
  resultMsg = "/run/briard/update-result";
  stubExe = "${stub}/bin/briard-selfupdate-stub";
  realAgent = "${agent}/bin/briard-agent";
  channel = "http://127.0.0.1:8099";
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
  readyV4 = candidate "ready" "v4"; # the artifact the channel's release ships
  evilCand = candidate "ready" "evil"; # different bytes than the signed manifest pins (tamper)
  crashCand = candidate "crash" ""; # exits 1 immediately → start fails → revert
  hangCand = candidate "hang" ""; # blocks without READY → TimeoutStartSec → revert
  # The three frozen wrappers — dumb shell, agent-INDEPENDENT (a bug in the volatile agent can
  # never wedge the update mechanism), verbatim the ones scripts/install.sh writes. Change one,
  # change both.
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
        if [ -e ${nextManifest} ]; then   # the candidate's manifest commits WITH it
            mv ${nextManifest} ${manifest}
        fi
        if [ -e ${nextNetWrap} ]; then    # the rest of the bundle, each existence-guarded ([B.86b])
            mv -T ${nextNetWrap} ${netWrap}
        fi
        if [ -L ${nextQemu} ]; then       # -T: rename the LINK, never move it into the old tree
            mv -T ${nextQemu} ${qemuLink}
        fi
        rm -f ${trialMarker}
    fi
  '';
  briardUpdate = pkgs.writeShellScript "briard-update" ''
    set -eu
    CHANNEL=${channel}
    KEYRING=/etc/briard/keyring.pem
    BASE=/var/lib/briard
    RUN=/run/briard
    GRACE=5400
    report() { printf '%s\n' "$*" | tee "$RUN/update-result"; }
    if [ -e "$RUN/update" ]; then
        age=$(( $(date +%s) - $(stat -c %Y "$RUN/update") ))
        if [ "$age" -ge "$GRACE" ]; then
            systemctl restart briard-agent.service
            report "forced the restart: an update had been armed for ''${age}s"
        else
            report "an update is already armed; the agent restarts itself at its next safe point (or now: systemctl restart briard-agent)"
        fi
        exit 0
    fi
    target=stable
    if [ -f "$RUN/update-target" ]; then
        target=$(head -n1 "$RUN/update-target")
        rm -f "$RUN/update-target"
    fi
    tmp=$(mktemp -d "$BASE/.update.XXXXXX"); trap 'rm -rf "$tmp"' EXIT
    url="$CHANNEL/host/$target/linux/briard-agent"
    if command -v curl >/dev/null 2>&1; then curl -fsSL "$url" -o "$tmp/briard-agent"
    elif command -v wget >/dev/null 2>&1; then wget -qO "$tmp/briard-agent" "$url"
    else report "need curl or wget to fetch $url"; exit 1; fi || { report "could not fetch a bootstrap agent from $url"; exit 1; }
    chmod +x "$tmp/briard-agent"
    set +e
    out=$(BRIARD_CHANNEL_URL="$CHANNEL" BRIARD_KEYRING="$KEYRING" UPDATE_BASE="$BASE" UPDATE_RUN_DIR="$RUN" \
        "$tmp/briard-agent" --fetch-update "$target" 2>&1)
    rc=$?
    set -e
    printf '%s\n' "$out" >&2
    report "$(printf '%s\n' "$out" | tail -n1)"
    exit $rc
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
      # curl: the frozen update script's bootstrap pull. tar + zstd: the test publishes a qemu
      # bundle the way the release script does, and the update verb unpacks it with tar(1).
      environment.systemPackages = [ pkgs.curl pkgs.gnutar pkgs.zstd ];

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
          # the health of the thing it supervises. Shortened further here so the up-but-unhealthy
          # HANG assertion resolves in seconds; the mechanism it exercises (timeout trips → start
          # fails → no commit) is identical at any value.
          TimeoutStartSec = 15;
          ExecStart = "${briardExec}"; # pick committed vs trial binary
          ExecStartPost = "${briardCommit}"; # runs ONLY after READY=1 → commit on success
          Restart = "always";
          RestartSec = 1;
          StartLimitIntervalSec = 0; # one failed trial then a revert must never latch as dead
        };
      };
      # The FROZEN update unit ([B.86a]): a oneshot, not templated — `systemctl start` blocks on
      # it and a start against a running job merges, so it is its own mutual exclusion. The
      # timer that fires it nightly in production is not under test here; every trigger is a
      # `systemctl start` of this unit, and that is what the script below receives.
      systemd.services.briard-update = {
        description = "briard update (the frozen unit below the agent)";
        wantedBy = [ ];
        # curl: the bootstrap pull; tar: the verb unpacks the qemu bundle with tar(1). install.sh's
        # unit sets an explicit PATH that reaches both for the same reason.
        path = [ pkgs.curl pkgs.gnutar ];
        serviceConfig = {
          Type = "oneshot";
          ExecStart = "${briardUpdate}";
        };
      };
    };

  testScript = ''
    machine.wait_for_unit("multi-user.target")

    def committed():
        return machine.succeed("cat ${agentBin}")

    def arm(candidate_store_path):
        # Stage a candidate + arm the trigger, exactly as the update verb does
        # (selfupdate.StageNext + Arm) — an atomic install then the tmpfs flag.
        machine.succeed(f"install -m755 {candidate_store_path} ${nextBin}")
        machine.succeed("touch ${updateFlag}")

    def invocation():
        return machine.succeed("systemctl show -p InvocationID --value briard-agent.service").strip()

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
    machine.succeed("rm -f ${nextBin}")
    print("4) power loss mid-arm ran committed v2, no commit, no revert code")

    # === 5) THE UPDATE UNIT BELOW THE AGENT ([B.86a]): with no target message (the timer's
    #        case) the unit follows `stable`, pulls the FRESH bootstrap from the pointer, and
    #        that binary verifies the manifest, fetches the artifact from the VERSIONED
    #        directory, stages it beside its manifest and ARMS — and does not restart anything.
    #        Then the forcing backstop: a young arm is left alone, an old one is forced. ===
    V4 = "v3.20260906.aaaaaaa"
    machine.succeed("mkdir -p /etc/briard /srv/host/latest/linux /srv/host/stable/linux")
    machine.succeed("${stubExe} keygen /root/release.key /etc/briard/keyring.pem")
    # The rest of the host bundle ([B.86b]): a launch shim, and a qemu "bundle" -- a tree with a
    # bin/qemu-system-x86_64 (a stand-in script; the stub candidates never run it) and a
    # PROVENANCE file, tarred and compressed exactly the way publish-release.sh does, so the same
    # bytes republish to the same hash (which is what the hash-skip in 8 and 9 rides on).
    machine.succeed("printf '#!/bin/sh\\nexec \"$@\"\\n' > /root/net-wrap && chmod 755 /root/net-wrap")
    def bundle(name, provenance):
        machine.succeed(f"mkdir -p /root/{name}/bin && printf '#!/bin/sh\\necho QEMU\\n' > /root/{name}/bin/qemu-system-x86_64 && chmod 755 /root/{name}/bin/qemu-system-x86_64")
        machine.succeed(f"printf '%s\\n' '{provenance}' > /root/{name}/PROVENANCE")
        machine.succeed(f"tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -cf - -C /root/{name} . | zstd -q -3 -o /root/{name}.tar.zst")
    bundle("bundle", "the first qemu")
    bundle("bundle2", "a second qemu")

    def publish(version, artifact, pointers=("latest", "stable"), qemu="bundle"):
        # One release of the host chain's linux arm, the way publish-release.sh lays it: the
        # artifacts under the versioned directory, a manifest written by the REAL writer, a
        # detached signature, and the pointers as byte-copies carrying the REAL agent as the
        # bootstrap (the one deliberate divergence, explained in the header).
        d = f"/srv/host/{version}/linux"
        machine.succeed(f"mkdir -p {d} && install -m755 {artifact} {d}/briard-agent")
        machine.succeed(f"install -m755 /root/net-wrap {d}/briard-net-wrap && install -m644 /root/{qemu}.tar.zst {d}/qemu-bundle.tar.zst")
        machine.succeed(f"${realAgent} --stage-manifest {d} --chain host --platform linux --release {version}")
        machine.succeed(f"${stubExe} sign /root/release.key {d}/manifest.json | base64 -d > {d}/manifest.json.sig")
        for p in pointers:
            machine.succeed(f"mkdir -p /srv/host/{p}/linux && cp {d}/manifest.json {d}/manifest.json.sig /srv/host/{p}/linux/")
            machine.succeed(f"install -m755 ${realAgent} /srv/host/{p}/linux/briard-agent")

    publish(V4, "${readyV4}")
    # The node's installed release: an OLDER date, so stable is ahead of it.
    machine.succeed(
        "printf '%s' '{\"chain\":\"host\",\"platform\":\"linux\",\"version\":\"v3.20260101.0000000\",\"artifacts\":[{\"name\":\"briard-agent\",\"sha256\":\"0\",\"size\":1}]}' > ${manifest}"
    )
    machine.succeed("systemd-run --unit=release-httpd --collect ${stubExe} serve 127.0.0.1:8099 /srv")
    machine.wait_until_succeeds("curl -sf ${channel}/host/stable/linux/manifest.json -o /dev/null", timeout=30)

    inv_before = invocation()
    machine.fail("test -e ${targetMsg}")
    machine.succeed("systemctl start briard-update.service")   # blocks: a oneshot
    result = machine.succeed("cat ${resultMsg}").strip()
    assert f"staged {V4} (agent, net-wrap, qemu), armed" in result, f"unexpected result: {result!r}"
    machine.succeed("test -e ${nextBin}")
    machine.succeed("cmp ${nextBin} ${readyV4}")             # the VERSIONED artifact, not the bootstrap
    machine.succeed(f"cmp ${nextManifest} /srv/host/{V4}/linux/manifest.json")  # its manifest beside it
    # The rest of the bundle ([B.86b]), staged beside them: the shim as a file, qemu as a
    # RELATIVE link to the tree the verb unpacked from the verified tarball.
    machine.succeed("cmp ${nextNetWrap} /root/net-wrap")
    assert machine.succeed("readlink ${nextQemu}").strip() == f"qemu-{V4}", "qemu.next does not link to this release's tree"
    machine.succeed(f"test -x /var/lib/briard/qemu-{V4}/bin/qemu-system-x86_64")
    machine.succeed(f"cmp /var/lib/briard/qemu-{V4}/PROVENANCE /root/bundle/PROVENANCE")
    machine.fail("test -e ${qemuLink}")                       # nothing committed yet
    machine.succeed("test -e ${updateFlag}")                  # armed…
    assert invocation() == inv_before, "the update unit restarted the agent itself; it must only arm"
    machine.succeed("test ! -e ${targetMsg}")
    machine.succeed("! ls -a /var/lib/briard | grep -q '^\\.update'")  # the bootstrap + unpack temp dirs are gone
    print(f"5a) the unit staged + armed the whole bundle of {V4} from stable without restarting anything")

    # A second run while armed, young: left to the agent's safe point; nothing restarted.
    machine.succeed("rm -f ${resultMsg}")
    machine.succeed("systemctl start briard-update.service")
    result = machine.succeed("cat ${resultMsg}").strip()
    assert "already armed" in result, f"unexpected result: {result!r}"
    assert invocation() == inv_before, "a young arm was forced"
    # Aged past the grace: forced. The stub agent has no safe point of its own, which is
    # exactly the case the backstop exists for.
    machine.succeed("touch -d '-2 hours' ${updateFlag}")
    machine.succeed("systemctl start briard-update.service")
    result = machine.succeed("cat ${resultMsg}").strip()
    assert "forced the restart" in result, f"unexpected result: {result!r}"
    machine.wait_for_unit("briard-agent.service")
    machine.wait_until_succeeds("grep -q ' v4' ${agentBin}", timeout=30)
    assert invocation() != inv_before, "the forced restart did not happen"
    machine.fail("test -e ${nextBin}")
    machine.fail("test -e ${nextManifest}")
    machine.succeed(f"cmp ${manifest} /srv/host/{V4}/linux/manifest.json")  # committed WITH its manifest
    assert " v4" in committed(), f"the fetched release did NOT commit, committed={committed()!r}"
    # ...and the bundle committed in the same burst: the shim moved onto its name, the qemu LINK
    # was renamed over (`mv -T`) rather than dropped inside a tree, and no .next survives.
    machine.fail("test -e ${nextNetWrap}")
    machine.succeed("cmp ${netWrap} /root/net-wrap")
    machine.fail("test -e ${nextQemu}")
    machine.succeed("test -L ${qemuLink}")
    assert machine.succeed("readlink ${qemuLink}").strip() == f"qemu-{V4}", "qemu was not committed as a link to the new tree"
    machine.succeed("test -x ${qemuLink}/bin/qemu-system-x86_64")  # the public path resolves through it
    print(f"5b) young arm left alone, old arm forced, {V4} committed with its manifest, shim and qemu link")

    # === 6) `briard update host` — the human trigger, on the REAL agent binary, through the
    #        same unit: a message in, the unit's verdict out, no admin socket. Up to date → a
    #        no-op that says so and bounces nothing. ===
    inv_before = invocation()
    out = machine.succeed("${realAgent} update host -base /var/lib/briard -run /run/briard").strip()
    assert f"already at {V4}" in out, f"unexpected CLI output: {out!r}"
    machine.fail("test -e ${resultMsg}")   # the CLI consumed its result
    machine.fail("test -e ${targetMsg}")   # and the unit consumed its target
    machine.fail("test -e ${updateFlag}")
    assert invocation() == inv_before, "an up-to-date `briard update host` bounced the agent"
    print("6) briard update host: already at the target, nothing armed, nothing restarted")

    # === 7) REFUSE-AND-STAY through the unit: a release whose served bytes differ from what
    #        its signed manifest pins is refused by the fresh bootstrap's hash check — nothing
    #        staged, nothing armed, the verdict names it, the CLI exits non-zero.
    #        [[verification-assertions-must-fail]] — the refusal must actually fire. ===
    V5 = "v3.20260907.bbbbbbb"
    publish(V5, "${readyV4}", pointers=("latest",))
    machine.succeed(f"install -m755 ${evilCand} /srv/host/{V5}/linux/briard-agent")  # tamper AFTER signing
    machine.fail("${realAgent} update host -base /var/lib/briard -run /run/briard")
    machine.succeed("journalctl -u briard-update | grep -q 'does not match the signed manifest'")
    machine.fail("test -e ${nextBin}")
    machine.fail("test -e ${updateFlag}")
    assert " v4" in committed(), f"a refused update changed the committed binary, committed={committed()!r}"
    # An UNSIGNED pointer (signature removed) is refused before anything is fetched.
    machine.succeed("rm /srv/host/latest/linux/manifest.json.sig")
    machine.fail("${realAgent} update host -base /var/lib/briard -run /run/briard")
    machine.fail("test -e ${nextBin}")
    assert " v4" in committed(), f"an unsigned manifest changed the committed binary, committed={committed()!r}"
    print("7) tampered artifact + unsigned manifest refused through the unit — committed v4 kept")

    # === 8) THE FLOOR: an exact pin OLDER than stable is refused loudly; the same pin is
    #        accepted once stable is moved down to it (the failable control). ===
    #        An EXACT target's bootstrap is pulled from its versioned directory (there is no
    #        pointer to duplicate it under), so this release's artifact must be the real agent:
    #        the stub divergence explained in the header only works through a pointer.
    OLD = "v3.20260201.ccccccc"
    publish(OLD, "${realAgent}", pointers=())
    machine.fail(f"${realAgent} update host -to {OLD} -base /var/lib/briard -run /run/briard")
    machine.succeed("journalctl -u briard-update | grep -q 'older than stable'")
    machine.fail("test -e ${nextBin}")
    machine.succeed(f"cp /srv/host/{OLD}/linux/manifest.json /srv/host/{OLD}/linux/manifest.json.sig /srv/host/stable/linux/")
    out = machine.succeed(f"${realAgent} update host -to {OLD} -base /var/lib/briard -run /run/briard").strip()
    # DOWNLOAD ONLY WHAT CHANGED ([B.86b]): this release ships the same shim and qemu bytes the
    # installed manifest ({V4}'s) pins, so neither is fetched or staged -- the agent alone moves.
    assert f"staged {OLD} (agent), armed" in out, f"a pin at the moved floor was refused, or fetched more than changed: {out!r}"
    machine.succeed("cmp ${nextBin} ${realAgent}")
    machine.fail("test -e ${nextNetWrap}")
    machine.fail("test -e ${nextQemu}")
    machine.fail(f"test -e /var/lib/briard/qemu-{OLD}")
    print("8) a pin below stable refused; accepted once stable moved to it (downgrade to the floor), and only the agent was fetched")

    # === 9) A FAILED TRIAL LEAVES THE BUNDLE STAGED AND INERT, AND THE NEXT RELEASE DROPS IT
    #        ([B.86b]): a release with a NEW qemu whose agent crashes reverts whole -- the committed
    #        qemu link never moves, qemu.next and its tree stay -- and a later release that does
    #        NOT change qemu discards that stale qemu.next before staging, so the commit that
    #        follows can never pair this agent with that qemu. [[verification-assertions-must-fail]]
    machine.succeed("rm -f ${nextBin} ${nextManifest} ${updateFlag}")  # un-arm scenario 8's pin
    V9 = "v3.20260908.ddddddd"
    publish(V9, "${crashCand}", pointers=("latest",), qemu="bundle2")
    out = machine.succeed("${realAgent} update host -to latest -base /var/lib/briard -run /run/briard").strip()
    assert f"staged {V9} (agent, qemu), armed" in out, f"unexpected: {out!r}"   # the shim is unchanged
    assert machine.succeed("readlink ${nextQemu}").strip() == f"qemu-{V9}"
    machine.succeed(f"cmp /var/lib/briard/qemu-{V9}/PROVENANCE /root/bundle2/PROVENANCE")
    machine.succeed("systemctl restart briard-agent.service || true")  # the trial crashes
    machine.wait_for_unit("briard-agent.service", timeout=60)
    machine.wait_until_succeeds("grep -q ' v4' ${agentBin}", timeout=30)
    assert " v4" in committed(), f"a crashing candidate committed: {committed()!r}"
    assert machine.succeed("readlink ${qemuLink}").strip() == f"qemu-{V4}", "the qemu link moved on a FAILED trial"
    assert machine.succeed("readlink ${nextQemu}").strip() == f"qemu-{V9}", "the refused qemu did not stay staged"
    machine.succeed(f"cmp ${manifest} /srv/host/{V4}/linux/manifest.json")
    machine.fail("test -e ${updateFlag}")
    print(f"9a) {V9}'s trial crashed: reverted whole, qemu link still {V4}'s, its qemu.next left inert")

    V10 = "v3.20260909.eeeeeee"
    publish(V10, "${readyV2}", pointers=("latest",))   # back on the FIRST bundle == the installed one
    out = machine.succeed("${realAgent} update host -to latest -base /var/lib/briard -run /run/briard").strip()
    assert f"staged {V10} (agent), armed" in out, f"unexpected: {out!r}"
    machine.fail("test -e ${nextQemu}")     # the stale link is GONE before this release is armed
    machine.succeed(f"test -d /var/lib/briard/qemu-{V9}")  # the tree is not the verb's to remove
    machine.succeed("systemctl restart briard-agent.service")
    machine.wait_for_unit("briard-agent.service")
    machine.wait_until_succeeds("grep -q ' v2' ${agentBin}", timeout=30)
    assert machine.succeed("readlink ${qemuLink}").strip() == f"qemu-{V4}", "the commit paired this agent with a qemu it did not ship"
    machine.succeed(f"cmp ${manifest} /srv/host/{V10}/linux/manifest.json")
    print(f"9b) {V10} discarded the stale qemu.next and committed on {V4}'s qemu, as its manifest says")

    print("the frozen Type=notify pivot commits good updates and reverts broken or lost ones; the frozen unit below the agent fetches, verifies, stages, arms, forces late, and refuses tampered, unsigned or below-floor releases; the host bundle stages and commits as one, fetching only what changed")
  '';
}
