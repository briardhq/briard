# The host-agent SELF-UPDATE, end to end on one VM (no nested guest): the frozen update unit
# below the agent pulls a fresh agent from the channel's pointer, and THAT binary's `--update`
# does the whole procedure -- verify, stage, smoke-test a staged qemu, stop the agent through its
# ordinary stop path, swap by rename, start it and watch it reach READY, then commit, or revert.
# Stand-in candidates (nixosTest/briard-selfupdate-stub) decide whether the started agent
# reaches READY and how it stops; everything else is the real thing.
#
# The layout is flat, every move a rename on one filesystem:
#   /var/lib/briard/briard-agent        the committed binary the service runs
#   /var/lib/briard/manifest.json       the committed release's signed manifest
#   /var/lib/briard/briard-net-wrap     the launch shim, cattle riding with the agent
#   /var/lib/briard/qemu -> qemu-<rel>/ a LINK to the committed qemu tree
#   /var/lib/briard/<entry>.next|.prev  staged, or displaced and not yet committed
#   /run/briard/update-target|-result   the target message in, the run's verdict out
#
# The update unit runs the SHIPPED scripts/agent/briard-update with one line changed (its base
# directory: this rig runs nothing at /opt/briard), so there is no second copy of its logic to
# drift. The channel is the real tree served by the stub's http.FileServer, the manifests are
# written by the REAL `--stage-manifest`, the pointer's bootstrap is the REAL agent, and the
# artifact it swaps in is a stub candidate -- that one divergence (the pointer's briard-agent and
# the versioned briard-agent are different bytes) is what makes the procedure drivable without a
# nested guest; publish-release.sh verify refuses it on a real channel. install-macvtap.nix
# proves the shipped units and script on a real install.
#
# The qemu bundle a release ships is a tree whose bin/qemu-system-x86_64 execs a real qemu, so
# the updater's smoke test boots a real scratch machine (TCG); a bundle whose qemu only prints
# a word is the release that does not run on this host.
#
# Hermetic (one VM, TCG-friendly), so it rides the tier. Run one:
#   nix build .#tests.agent-selfupdate -L
{ pkgs, stub, agent }:
let
  base = "/var/lib/briard";
  agentBin = "${base}/briard-agent";
  manifest = "${base}/manifest.json";
  netWrap = "${base}/briard-net-wrap";
  qemuLink = "${base}/qemu";
  resultMsg = "/run/briard/update-result";
  failureMsg = "/run/briard/update-failure";
  stubExe = "${stub}/bin/briard-selfupdate-stub";
  realAgent = "${agent}/bin/briard-agent";
  qemu = pkgs.qemu_test;
  channel = "http://127.0.0.1:8099";
  # A "binary" on disk is a tiny script that execs the stub in a given mode/identity. The unit
  # just execs the path, so the mode has to live IN the file -- and the `exec` keeps the PID, so
  # the stub's READY=1 comes from MAINPID (NotifyAccess=main, exactly like the real agent). The
  # identity string is what the test greps to prove which binary ran.
  candidate = mode: id: pkgs.writeShellScript "briard-agent-${mode}${pkgs.lib.optionalString (id != "") "-${id}"}" ''
    exec ${stubExe} ${mode} ${id}
  '';
  slowV1 = candidate "slowstop" "v1"; # the committed agent at install: it takes 8s to stop
  readyV2 = candidate "ready" "v2"; # a good agent, run directly in the interrupted-run case
  readyV4 = candidate "ready" "v4"; # the artifact the channel's stable release ships
  evilCand = candidate "ready" "evil"; # different bytes than the signed manifest pins (tamper)
  crashCand = candidate "crash" ""; # exits 1 at once: the start fails, the updater reverts
  hangCand = candidate "hang" ""; # never READY: TimeoutStartSec fails the start, the updater reverts
  # The SHIPPED update script, its base directory moved to this rig's. The grep makes the
  # substitution fail the build rather than leave the rig driving /opt/briard.
  briardUpdate = pkgs.runCommand "briard-update" { } ''
    sed 's|^BASE=/opt/briard/agent$|BASE=${base}|' ${../scripts/agent/briard-update} > $out
    grep -q '^BASE=${base}$' $out
    chmod +x $out
  '';
in
pkgs.testers.runNixOSTest {
  name = "agent-selfupdate";
  skipTypeCheck = true; # dynamic asserts

  nodes.machine =
    { ... }:
    {
      virtualisation.memorySize = 1536; # the smoke test boots a 128 MB scratch machine
      systemd.tmpfiles.rules = [
        "d ${base} 0755 root root -"
        "d /run/briard 0755 root root -"
      ];
      # curl: the update script's bootstrap pull. tar + zstd: the test publishes a qemu bundle the
      # way the release script does, and the update verb unpacks it with tar(1). qemu: what the
      # good bundle's qemu execs, so it must be in this VM's store.
      environment.systemPackages = [ pkgs.curl pkgs.gnutar pkgs.zstd qemu ];

      # The agent's unit, as shipped in every line the update depends on: Type=notify, READY from
      # the main process, the start bound, a restart on failure only (a clean exit on SIGTERM must
      # not bring a stopped agent back), and no start limiter (a failed candidate followed by its
      # revert is a burst of starts). TimeoutStopSec is SHORTER than the slowstop agent takes to
      # stop: a stop that reached it would kill the agent, so a clean stop proves the updater's
      # own patience. TimeoutStartSec is short so the hang case resolves in seconds; the
      # mechanism (the start fails, the updater reverts) is the same at any value.
      systemd.services.briard-agent = {
        description = "Briard host agent (self-update rig)";
        wantedBy = [ ];
        startLimitIntervalSec = 0;
        serviceConfig = {
          Type = "notify";
          NotifyAccess = "main";
          TimeoutStartSec = 15;
          TimeoutStopSec = 5;
          ExecStart = agentBin;
          Restart = "on-failure";
          RestartSec = 1;
        };
      };
      # The update unit: a oneshot, not templated -- `systemctl start` blocks on it and a start
      # against a running job merges, so it is its own mutual exclusion. The timer that fires it
      # in production is not under test here; every trigger is a `systemctl start` of this unit.
      # CHANNEL_URL and UPDATE_KEYRING are what config.env delivers on a real node; ACCEL is the
      # node's own setting, which the smoke test boots its scratch machine with.
      systemd.services.briard-update = {
        description = "briard update (the frozen unit below the agent)";
        wantedBy = [ ];
        path = [ pkgs.curl pkgs.gnutar ];
        environment = {
          CHANNEL_URL = channel;
          UPDATE_KEYRING = "/etc/briard/keyring.pem";
          ACCEL = "tcg";
        };
        serviceConfig = {
          Type = "oneshot";
          ExecStart = "${briardUpdate}";
        };
      };
    };

  testScript = ''
    machine.wait_for_unit("multi-user.target")

    def invocation():
        return machine.succeed("systemctl show -p InvocationID --value briard-agent.service").strip()

    def verdict():
        return machine.succeed("cat ${resultMsg}").strip()

    def leftovers():
        return machine.succeed("ls -a ${base} | grep -E '[.](next|prev)$' || true").strip()

    def last_id():
        # The identity of the agent that started most recently, from its own announcement.
        return machine.succeed("journalctl -u briard-agent -o cat | grep 'briard-selfupdate-stub mode=' | tail -1").strip()

    # Seed the committed binary (install-time) and start the unit on it.
    machine.succeed("install -m755 ${slowV1} ${agentBin}")
    machine.systemctl("start briard-agent.service")
    machine.wait_for_unit("briard-agent.service")

    # The channel: a keyring, a launch shim, two qemu bundles tarred the way publish-release.sh
    # does it (so the same bytes republish to the same hash, which the hash-skip rides on). The
    # good one's qemu is a real one; the bad one's prints a word and exits.
    machine.succeed("mkdir -p /etc/briard /srv/briard")
    machine.succeed("${stubExe} keygen /root/release.key /etc/briard/keyring.pem")
    machine.succeed("printf '#!/bin/sh\\nexec \"$@\"\\n' > /root/net-wrap && chmod 755 /root/net-wrap")
    def bundle(name, qemu_bin, provenance):
        machine.succeed(f"mkdir -p /root/{name}/bin /root/{name}/share && printf '%s\\n' '{provenance}' > /root/{name}/PROVENANCE")
        machine.succeed(f"printf '#!/bin/sh\\n%s\\n' '{qemu_bin}' > /root/{name}/bin/qemu-system-x86_64 && chmod 755 /root/{name}/bin/qemu-system-x86_64")
        machine.succeed(f"ln -sfn ${qemu}/share/qemu /root/{name}/share/qemu")
        machine.succeed(f"tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -cf - -C /root/{name} . | zstd -q -3 -o /root/{name}.tar.zst")
    bundle("good", 'exec ${qemu}/bin/qemu-system-x86_64 "$@"', "a qemu that runs here")
    bundle("bad", "echo QEMU", "a qemu that does not")

    def publish(version, artifact, pointers=("latest", "stable"), qemu="good"):
        # One release of the host chain's linux arm, the way publish-release.sh lays it: the
        # artifacts under the versioned directory, a manifest written by the REAL writer, a
        # detached signature, and the pointers as byte-copies carrying the REAL agent as the
        # bootstrap (the one deliberate divergence, explained in the header).
        d = f"/srv/briard/{version}/linux-amd64"
        machine.succeed(f"mkdir -p {d} && install -m755 {artifact} {d}/briard-agent")
        machine.succeed(f"install -m755 /root/net-wrap {d}/briard-net-wrap && install -m644 /root/{qemu}.tar.zst {d}/qemu-bundle.tar.zst")
        machine.succeed(f"${realAgent} --stage-manifest {d} --chain briard --platform linux-amd64 --release {version}")
        machine.succeed(f"${stubExe} sign /root/release.key {d}/manifest.json | base64 -d > {d}/manifest.json.sig")
        for p in pointers:
            machine.succeed(f"mkdir -p /srv/briard/{p}/linux-amd64 && cp {d}/manifest.json {d}/manifest.json.sig /srv/briard/{p}/linux-amd64/")
            machine.succeed(f"install -m755 ${realAgent} /srv/briard/{p}/linux-amd64/briard-agent")

    V4 = "v3.99090600.aaaaaaa"
    publish(V4, "${readyV4}")
    # The node's installed release: an OLDER date, so stable is ahead of it.
    machine.succeed(
        "printf '%s' '{\"chain\":\"briard\",\"platform\":\"linux-amd64\",\"version\":\"v3.99010100.0000000\",\"artifacts\":[{\"name\":\"briard-agent\",\"sha256\":\"0\",\"size\":1}]}' > ${manifest}"
    )
    machine.succeed("systemd-run --unit=release-httpd --collect ${stubExe} serve 127.0.0.1:8099 /srv")
    machine.wait_until_succeeds("curl -sf ${channel}/briard/stable/linux-amd64/manifest.json -o /dev/null", timeout=30)

    # === 1) THE TIMER'S PATH, A GOOD RELEASE: with no target message the unit follows stable;
    #        the fresh bootstrap verifies and stages the whole bundle, smoke-tests the staged qemu,
    #        stops the agent -- WAITING for it to finish, past the unit's own TimeoutStopSec --
    #        swaps, starts the new agent, sees READY and commits. One blocking `systemctl start`. ===
    inv = invocation()
    machine.succeed("systemctl start briard-update.service")
    assert verdict() == f"ok updated to {V4} (agent, net-wrap, qemu)", f"unexpected verdict: {verdict()!r}"
    machine.succeed("cmp ${agentBin} ${readyV4}")                     # the VERSIONED artifact, not the bootstrap
    machine.succeed(f"cmp ${manifest} /srv/briard/{V4}/linux-amd64/manifest.json")
    machine.succeed("cmp ${netWrap} /root/net-wrap")
    assert machine.succeed("readlink ${qemuLink}").strip() == f"qemu-{V4}", "qemu was not swapped to this release's tree"
    machine.succeed(f"cmp ${base}/qemu-{V4}/PROVENANCE /root/good/PROVENANCE")
    assert leftovers() == "", f"a committed update left {leftovers()!r}"
    machine.succeed("journalctl -u briard-update | grep -q 'qemu smoke test: scratch machine running'")
    # THE STOP WAS THE AGENT'S OWN: it finished its act (the file it leaves on the way out) and
    # was never killed, though it took longer than the unit's TimeoutStopSec would have allowed.
    machine.succeed("test -e /run/briard/drained-v1")
    machine.succeed("journalctl -u briard-agent -o cat | grep -q 'id=v1: stopped cleanly'")
    machine.fail("journalctl -u briard-agent | grep -qiE 'timed out|SIGKILL'")
    assert "mode=ready id=v4" in last_id(), f"the started agent is not v4: {last_id()!r}"
    machine.succeed("systemctl is-active briard-agent.service")
    assert invocation() != inv, "the agent was never restarted"
    machine.succeed("! ls -a ${base} | grep -q '^[.]update'")         # the bootstrap + unpack temp dirs are gone
    print(f"1) stable {V4}: staged, smoke-tested, stopped with patience, swapped, started, committed")

    # === 2) `briard update` -- the human trigger, on the REAL agent binary, through the same
    #        unit. Up to date: a no-op that says so and touches nothing. The verdict stays on
    #        disk: the alerter reads it too. ===
    inv = invocation()
    out = machine.succeed("${realAgent} update -base ${base} -run /run/briard").strip()
    assert f"already at {V4}" in out, f"unexpected CLI output: {out!r}"
    assert verdict().startswith(f"ok already at {V4}"), f"the verdict was not left: {verdict()!r}"
    machine.fail("test -e /run/briard/update-target")                  # the unit consumed its target
    assert invocation() == inv, "an up-to-date `briard update` bounced the agent"
    print("2) briard update: already at the target, nothing stopped, the verdict left for the alerter")

    # === 3) REFUSE-AND-STAY: a release whose served bytes differ from what its signed manifest
    #        pins is refused by the fresh bootstrap's hash check, and an unsigned pointer before
    #        anything is fetched -- nothing staged, nothing stopped, the verdict says failed. ===
    V5 = "v3.99090700.bbbbbbb"
    publish(V5, "${readyV4}", pointers=("latest",))
    machine.succeed(f"install -m755 ${evilCand} /srv/briard/{V5}/linux-amd64/briard-agent")  # tamper AFTER signing
    # ⚠️ THE DEFAULT IS `stable`: V5 sits at `latest` alone, so the bare verb must not reach it.
    out = machine.succeed("${realAgent} update -base ${base} -run /run/briard").strip()
    assert f"already at {V4}" in out, f"the bare verb did not take stable: {out!r}"
    machine.fail("${realAgent} update -to latest -base ${base} -run /run/briard")
    machine.succeed("journalctl -u briard-update | grep -q 'does not match the signed manifest'")
    assert verdict().startswith("failed "), f"a refusal left {verdict()!r}"
    machine.succeed("rm /srv/briard/latest/linux-amd64/manifest.json.sig")
    machine.fail("${realAgent} update -to latest -base ${base} -run /run/briard")
    machine.succeed("cmp ${agentBin} ${readyV4}")
    assert leftovers() == "", f"a refused update left {leftovers()!r}"
    assert invocation() == inv, "a refused update stopped the agent"
    print("3) tampered artifact + unsigned manifest at `latest` refused; nothing staged, nothing stopped")

    # === 4) A QEMU THAT DOES NOT RUN HERE refuses its release BEFORE ANYTHING STOPS: the smoke
    #        test is the updater's, at stage time, so the agent never notices. ===
    V6 = "v3.99090800.ccccccc"
    publish(V6, "${readyV2}", pointers=("latest",), qemu="bad")
    out = machine.fail("${realAgent} update -to latest -base ${base} -run /run/briard 2>&1")
    assert "refused on this host: qemu smoke test" in out, f"the release failed, but not on its qemu: {out!r}"
    assert invocation() == inv, "a release refused at stage time stopped the agent"
    assert machine.succeed("readlink ${qemuLink}").strip() == f"qemu-{V4}", "a refused qemu was swapped in"
    machine.succeed("cmp ${agentBin} ${readyV4}")
    assert leftovers() == "", f"a refused release left {leftovers()!r}"
    print(f"4) {V6}'s qemu failed its smoke test: refused with nothing touched")

    # === 5) A CANDIDATE THAT CRASHES is reverted whole: the start fails, the committed release
    #        goes back and starts, and the reason waits for the agent that came back. ===
    V7 = "v3.99090900.ddddddd"
    publish(V7, "${crashCand}", pointers=("latest",))
    out = machine.fail("${realAgent} update -to latest -base ${base} -run /run/briard 2>&1")
    assert "did not start on this host and was reverted" in out, f"unexpected: {out!r}"
    machine.succeed("journalctl -u briard-agent -o cat | grep -q 'mode=crash'")   # the candidate really ran
    machine.succeed(f"journalctl -u briard-update | grep -q 'update: staged {V7} (agent)'")  # shim and qemu unchanged: not fetched
    machine.succeed("cmp ${agentBin} ${readyV4}")
    machine.succeed(f"cmp ${manifest} /srv/briard/{V4}/linux-amd64/manifest.json")
    assert leftovers() == "", f"a reverted update left {leftovers()!r}"
    machine.succeed(f"grep -q '^{V7} ' ${failureMsg}")                 # left for the agent to escalate
    assert "mode=ready id=v4" in last_id(), f"the agent after the revert is not v4: {last_id()!r}"
    machine.succeed("systemctl is-active briard-agent.service")
    print(f"5) {V7} crashed at start: reverted to {V4}, which is running")

    # === 6) A CANDIDATE THAT HANGS before READY: TimeoutStartSec fails the start (the case a crash
    #        detector alone would miss), and it is reverted the same way. ===
    V8 = "v3.99091000.eeeeeee"
    publish(V8, "${hangCand}", pointers=("latest",))
    out = machine.fail("${realAgent} update -to latest -base ${base} -run /run/briard 2>&1")
    assert "did not start on this host and was reverted" in out, f"unexpected: {out!r}"
    machine.succeed("journalctl -u briard-agent -o cat | grep -q 'mode=hang'")
    machine.succeed("journalctl -u briard-agent | grep -qi 'timed out'")
    machine.succeed("cmp ${agentBin} ${readyV4}")
    assert leftovers() == "", f"a reverted update left {leftovers()!r}"
    assert "mode=ready id=v4" in last_id(), f"the agent after the revert is not v4: {last_id()!r}"
    print(f"6) {V8} hung before READY: reverted to {V4}")

    # === 7) A RUN KILLED BETWEEN ITS STOP AND ITS COMMIT -- a power cut, a reboot -- leaves the
    #        candidate in place and the committed agent beside it as .prev. The next run reverts
    #        that before it decides anything. Here the node then runs the uncommitted candidate,
    #        as it would after the boot that follows a cut. ===
    machine.succeed("systemctl stop briard-agent.service")
    machine.succeed("mv ${agentBin} ${agentBin}.prev && install -m755 ${readyV2} ${agentBin}")
    machine.succeed("systemctl start briard-agent.service")
    assert "mode=ready id=v2" in last_id(), "the interrupted state is not what the test set up"
    machine.succeed("systemctl start briard-update.service")           # the timer's path: stable is V4
    assert verdict().startswith(f"ok already at {V4}"), f"unexpected verdict: {verdict()!r}"
    machine.succeed("journalctl -u briard-update | grep -q 'stopped between its swap and its verdict; reverting it'")
    machine.succeed("cmp ${agentBin} ${readyV4}")
    assert leftovers() == "", f"the reconcile left {leftovers()!r}"
    assert "mode=ready id=v4" in last_id(), f"the agent after the reconcile is not v4: {last_id()!r}"
    print("7) a run cut between swap and commit was reverted by the next run")

    # === 8) THE FLOOR: an exact pin OLDER than stable is refused loudly, nothing stopped; the
    #        same pin, once stable is moved down to it, goes past the decision (the failable
    #        control). An exact target's bootstrap is pulled from its versioned directory, so its
    #        artifact is the real agent -- which, started bare, prints its help and never sends
    #        READY, so the run that follows is one more revert. ===
    OLD = "v3.99020100.fffffff"
    publish(OLD, "${realAgent}", pointers=())
    inv = invocation()
    out = machine.fail(f"${realAgent} update -to {OLD} -base ${base} -run /run/briard 2>&1")
    assert "older than stable" in out, f"the pin failed, but not on the stable floor: {out!r}"
    assert invocation() == inv, "a pin below the floor stopped the agent"
    machine.succeed(f"cp /srv/briard/{OLD}/linux-amd64/manifest.json /srv/briard/{OLD}/linux-amd64/manifest.json.sig /srv/briard/stable/linux-amd64/")
    out = machine.fail(f"${realAgent} update -to {OLD} -base ${base} -run /run/briard 2>&1")
    assert "older than stable" not in out and "reverted" in out, f"a pin at the moved floor was refused: {out!r}"
    machine.succeed(f"journalctl -u briard-update | grep -q 'update: staged {OLD} (agent)'")
    machine.succeed("cmp ${agentBin} ${readyV4}")
    machine.succeed("systemctl is-active briard-agent.service")
    print("8) a pin below stable refused; at the moved floor it went past the decision")

    print("the update unit below the agent fetches, verifies, stages, smoke-tests, stops with patience, swaps, starts and commits a good release; it reverts a candidate that crashes or hangs and a run cut mid-swap, and refuses a tampered, unsigned, below-floor or not-runnable-here release with nothing stopped")
  '';
}
