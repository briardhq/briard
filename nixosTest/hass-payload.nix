# Home Assistant installed as an app, data on the DRBD volume.
#
# The path drbd-promote proves for the dummy, with the real HA image (digest-pinned in a signed
# manifest): bring r0 up, let drbd-reactor promote and start the ordered chain, install HA onto
# the volume, and assert HA boots and serves and that its recorder SQLite + `.storage` landed on
# the DRBD btrfs subvolume. Single node: this validates the app wiring, not failover (that is
# hass-failover), and HA is heavy enough that one instance is the right cost.
#
# It also carries zeroconf claim (b), the one measurement needing real HA and a real announcement
# in one place: whether HA's own zeroconf library, inside its container, sees a record the guest
# published. The broker is installed for that and for the claims wiring HA to it, (c) and (d).
# HA is also asserted reachable through the reverse proxy under its own name ("Through the
# reverse proxy", below), keyed on the app's name in the routing table rather than on there
# being exactly one thing to forward to.
{ pkgs, guestModule, fixture, mosquitto }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # HA's address and readiness path come from its fixture's manifest, never restated here.
  ha = "http://127.0.0.1:${toString fixture.port}";
  haVip = "http://192.168.1.100:${toString fixture.port}";
  # The zeroconf probe, as its own file rather than a heredoc inside the test script: Nix's
  # indented strings strip the smallest indentation in the string, so a Python body at column 0
  # stops any stripping and silently de-indents the whole test script around it, a driver syntax
  # error a long way from its cause.
  zeroconfProbe = pkgs.writeText "zeroconf-probe.py" ''
    """Browse _mqtt._tcp with HA's own zeroconf, print what resolved, exit 1 if nothing did."""
    import sys
    import time

    from zeroconf import ServiceBrowser, Zeroconf

    found = {}


    class Listener:
        def add_service(self, zc, type_, name):
            info = zc.get_service_info(type_, name, timeout=5000)
            if info:
                found[name] = (sorted(info.parsed_addresses()), info.port, info.server)

        def update_service(self, *a):
            pass

        def remove_service(self, *a):
            pass


    zc = Zeroconf()
    ServiceBrowser(zc, "_mqtt._tcp.local.", Listener())
    for _ in range(45):
        if found:
            break
        time.sleep(1)
    zc.close()
    for name, value in found.items():
        print(name, value)
    sys.exit(0 if found else 1)
  '';

  # The Repairs probe: Briard's issues and the issue strings HA loaded for the domain, as one JSON
  # object. Repairs has no REST listing, so this speaks HA's websocket, on HA's own aiohttp.
  repairsProbe = pkgs.writeText "repairs-probe.py" ''
    import asyncio
    import json
    import os

    import aiohttp

    async def main():
        async with aiohttp.ClientSession() as s:
            async with s.ws_connect("${ha}/api/websocket") as ws:
                await ws.receive_json()
                await ws.send_json({"type": "auth", "access_token": os.environ["TOKEN"]})
                assert (await ws.receive_json())["type"] == "auth_ok"
                await ws.send_json({"id": 1, "type": "repairs/list_issues"})
                issues = (await ws.receive_json())["result"]["issues"]
                await ws.send_json({"id": 2, "type": "frontend/get_translations", "language": "en", "category": "issues", "integration": ["briard"]})
                strings = (await ws.receive_json())["result"]["resources"]
        print(json.dumps({"issues": [i for i in issues if i["domain"] == "briard"], "strings": strings}))

    asyncio.run(main())
  '';

  # The updates probe: Briard's update entities as HA's state machine holds them, as one JSON list,
  # over the same websocket.
  updatesProbe = pkgs.writeText "updates-probe.py" ''
    import asyncio
    import json
    import os

    import aiohttp

    async def main():
        async with aiohttp.ClientSession() as s:
            async with s.ws_connect("${ha}/api/websocket") as ws:
                await ws.receive_json()
                await ws.send_json({"type": "auth", "access_token": os.environ["TOKEN"]})
                assert (await ws.receive_json())["type"] == "auth_ok"
                await ws.send_json({"id": 1, "type": "get_states"})
                states = (await ws.receive_json())["result"]
        print(json.dumps([s for s in states if s["entity_id"].startswith("update.briard_")]))

    asyncio.run(main())
  '';

  node = h.mkNode {
    # The broker rides along for claim (b): whether HA's own zeroconf stack sees what the guest
    # announces. It needs a real service record on the LAN, and the product publishes exactly
    # one. Claims (c) and (d) then wire HA to it.
    fixtures = [
      fixture
      mosquitto
    ];
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
in
pkgs.testers.runNixOSTest {
  name = "hass-payload";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      # hass-nudge is the real push half of the control channel as a command (agent/hass's Nudge),
      # so claim (d) drives the shipping code against the real Home Assistant rather than a curl
      # that resembles it, the split app-probe makes in apps-pair.nix.
      environment.systemPackages = [ (pkgs.callPackage ./hass-nudge-pkg.nix { }) ];
      # HA needs real memory + disk headroom: the 2.4 GB image is read from its prebuilt store (an
      # erofs disk, not RAM), and HA's Python stack wants ~1 GB live. 2048 is measured: a guest
      # grows page cache into whatever it is given (3166 MB resident of 3072 declared, and 4192 of
      # 4096 would follow); at 2048 it is 2143 MB resident and the same 129s.
      virtualisation.memorySize = 2048;
      virtualisation.diskSize = 10240;
    };

  # HA boot is slow and the promoter selects the primary dynamically.
  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}
    node1.start()
    node1.wait_for_unit("multi-user.target")
    # The 2.4 GB HA image is resident before anything promotes: an install must not fetch it, and
    # on the failover path a pull would be fatal.
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=600)
    node1.succeed("modprobe drbd")
    node1.succeed("briard-test-storage --seed")
    # Single node: no peer to connect to — just make it UpToDate so it's promotable.

    # Give the node a flock name before anything converges, the way the agent does at bring-up:
    # the per-app names are composed from it, so a node with none routes nothing.
    name_the_flock(node1)

    # Hand off to the promoter: it has quorum alone, promotes, and runs the ordered chain. Nothing
    # is installed yet, so the reverse proxy comes up answering for itself.
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-primary-storage.service", timeout=120)
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)

    # ⚠️ The inbound socket must exist before any container starts. The rendered unit binds it into
    # the container, and podman creates a missing bind source as a root-owned directory, which
    # poisons the path for every later attempt (a socket cannot go where a directory sits). In the
    # product the listener starts with the agent, before anything converges; here the harness unit
    # supplies it, so wait for the same precondition rather than racing it.
    node1.wait_until_succeeds("test -S /run/briard/agent.sock", timeout=60)

    # The install: the HA manifest onto the volume, then the product converge renders it, warms it
    # (already resident) and starts it.
    dataroot = install_fixture(node1)
    assert dataroot == "/var/lib/briard/data/home-assistant", f"the renderer chose {dataroot}"
    # The broker, for the zeroconf claim at the bottom. Installed through the same converge as HA.
    install_fixture(node1, app="${mosquitto.name}")

    # The DRBD device is mounted, and the HA /config subvolume lives on it. The real mount, not
    # the subvolume path: a fresh btrfs subvolume is not reliably seen as a mountpoint until
    # traversed; the data-file checks below confirm the state lands on the subvolume.
    node1.succeed("mountpoint -q /var/lib/briard")

    # HA serves at the VIP. /manifest.json is static + unauthenticated, so a 200 means the HTTP
    # stack is up, the readiness signal the health-gate will use. Generous timeout: first boot
    # initializes the recorder DB + onboarding store.
    node1.wait_until_succeeds("curl -fsS -o /dev/null ${haVip}${fixture.healthPath}", timeout=300)

    # The unit that runs HA came from the renderer, not from this file.
    for unit in fixture_units(node1):
        node1.succeed(f"systemctl is-active {unit}")

    # The claim: the HA state (recorder SQLite and the `.storage` config) landed on the DRBD
    # subvolume, so they replicate + snapshot as one unit.
    node1.wait_until_succeeds(f"test -f {dataroot}/${fixture.dataDir}/home-assistant_v2.db", timeout=60)
    node1.succeed(f"test -d {dataroot}/${fixture.dataDir}/.storage")
    node1.succeed(f"test -f {dataroot}/${fixture.dataDir}/configuration.yaml")
    # Its backups apart, in the unversioned tier, mounted over /config/backups: an area of its own
    # with the nested mount on top of its parent, so what is put in the area is what HA sees there,
    # and none of it is in the data area.
    unversioned = "/var/lib/briard/unversioned/home-assistant"
    node1.succeed(f"btrfs subvolume show {unversioned} && echo briard > {unversioned}/backups/probe")
    node1.succeed("podman exec briard-home-assistant-app grep -q briard /config/backups/probe")
    node1.fail(f"test -e {dataroot}/${fixture.dataDir}/backups/probe")

    # ── The control channel (agent/hass) ─────────────────────────────────────────────
    #
    # This is the catalog gate. Briard shadow-mounts its own script over the s6 `run` the image
    # owns and hands over to that image's own `run`, extracted byte-for-byte; the compatibility
    # risk is deliberately relocated here: an upstream layout change (s6 v2 -> v3, a moved
    # service directory) must fail a blessed image on its way into the catalog, never a fleet of
    # homes at once. Everything below runs against the image the manifest pins.
    def exchange(refresh):
        """Refresh -> access, the documented exchange, client_id omitted."""
        return node1.succeed(
            "curl -fsS -X POST ${ha}/auth/token "
            f"-d grant_type=refresh_token -d refresh_token={refresh} "
            "| sed 's/.*\"access_token\":\"\\([^\"]*\\)\".*/\\1/'"
        ).strip()

    # The value is ours and chosen first, on tmpfs, 0600, so a consumer knows the token from t=0,
    # with no return channel out of the container and no startup race.
    node1.succeed("test -f /run/briard/home-assistant/token")
    perms = node1.succeed("stat -c %a /run/briard/home-assistant/token").strip()
    assert perms == "600", f"the token is mode {perms}; it is a credential for the whole HA API"
    token = node1.succeed("cat /run/briard/home-assistant/token").strip()

    # ---- The inbound channel, and the ring member Home Assistant's own start took ----
    # The only place the channel is reachable end to end: the rigs that run Home Assistant are
    # agent-less, so the harness starts the product listener (nixosTest/lib.nix), and inside the
    # container the product `run` wrapper calls the product notify.py. The member is the proof,
    # not the socket: a socket proves a bind; a member with a readable sidecar proves the request
    # crossed the container boundary, resolved to an app by its token, and reached btrfs on the
    # replicated volume.
    node1.succeed("test -S /run/briard/agent.sock")
    node1.succeed("test -f /run/briard/home-assistant/inbound.token")
    tok_perms = node1.succeed("stat -c %a /run/briard/home-assistant/inbound.token").strip()
    assert tok_perms == "600", f"the inbound token is mode {tok_perms}; it is what identifies a caller"

    members = node1.succeed(
        "ls -1 /var/lib/briard/data/.snapshots | grep '^home-assistant-start-' || true"
    ).split()
    assert members, (
        "Home Assistant started and no ring member was taken:\n"
        + node1.succeed("ls -la /var/lib/briard/data/.snapshots || true")
        + node1.succeed("systemctl status briard-test-inbound --no-pager -l || true")
    )
    member = members[0]
    # A real read-only subvolume, not a copied directory.
    node1.succeed(f"btrfs subvolume show /var/lib/briard/data/.snapshots/{member} >/dev/null")
    ro = node1.succeed(f"btrfs property get -ts /var/lib/briard/data/.snapshots/{member} ro").strip()
    assert ro == "ro=true", f"the member is writable ({ro}); a rollback point must not be"

    # Every member has a sidecar carrying the manifest the app was running, which the member
    # itself cannot hold: the volume keeps manifests in .apps/, a sibling of the data subvolume
    # the snapshot copies.
    import json as _sj

    sidecar = _sj.loads(node1.succeed(f"cat /var/lib/briard/data/.snapshots/{member}.json"))
    assert sidecar["app"] == "home-assistant", sidecar
    assert sidecar["trigger"] == "start", sidecar

    # The start is evaluated after its boot. The pre-start hook writes the sample pending; only
    # the evaluator of the long-running agent (here the harness --inbound-listen) clears it, with
    # the app health of the boot (/api/config through our token, for Home Assistant). A pending
    # sample left behind, or one without that verdict, means the evaluator never ran or never
    # reached the app. The health match is what fails when the evaluator does not run (only it
    # writes one), and a missing sidecar fails the first grep rather than passing the negated one.
    node1.wait_until_succeeds(
        "f=$(ls -1 /var/lib/briard/data/.snapshots | grep '^home-assistant-start-' | grep -v '[.]json$' | tail -n1)"
        " && test -n \"$f\""
        " && grep -q '\"health\":\"healthy\"' \"/var/lib/briard/data/.snapshots/$f.json\""
        " && ! grep -q '\"pending\":true' \"/var/lib/briard/data/.snapshots/$f.json\"",
        timeout=420,
    )

    # Byte-identical to what the volume names, derived rather than restated: a version literal
    # would assert that someone typed a string twice, not that the member carries the identity it
    # was taken under.
    on_volume = node1.succeed("cat /var/lib/briard/.apps/home-assistant.json").strip()
    assert sidecar["manifest"].strip() == on_volume, (
        f"the sidecar's manifest is not the one on the volume:\n{sidecar['manifest']!r}\n{on_volume!r}"
    )
    print(f"the inbound channel took {member}, pinned to the manifest on the volume")

    # The mint ran inside the container, in the stopped window s6's `run` provides, and the
    # token HA now holds is the one we chose.
    access = exchange(token)
    assert access.startswith("ey"), f"the token did not exchange for an access token: {access}"

    # The signal the S1 readiness gate reads, through the token: the per-config-entry setup states
    # of HA. Waited for, not sampled: /manifest.json goes 200 while HA is still setting up
    # default_config, so the list is legitimately empty for a few seconds (measured: a sample then
    # reads `[]`). Waiting is also the stronger claim: the entries appear and stay readable.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {access}' "
        "${ha}/api/config/config_entries/entry | grep -q entry_id",
        timeout=300,
    )
    entries = node1.succeed(
        f"curl -fsS -H 'Authorization: Bearer {access}' "
        "${ha}/api/config/config_entries/entry"
    )
    assert '"state"' in entries, f"config entries carry no state: {entries[:200]}"

    # ---- The clock sample, held still by Home Assistant itself ----
    # The one assertion guarding an internal API. The clock sample is taken while Home Assistant
    # runs, and what makes it application-consistent rather than something HA must recover from is
    # HA's own `recorder.lock_database()`, not a documented integration surface. The rest of the
    # quiesce is unit-tested against a stub; only this says the method still exists, still locks
    # and still reports whether the lock held, on the image the catalog pins, so an upstream
    # rename lands here as a red rig instead of a silent downgrade to crash-consistent members
    # that nobody notices for a release.
    #
    # ⚠️ After the wait above (the trap the entries assertion records): a briard view exists only
    # once the integration async_setup has run, later than /manifest.json answering. Measured: a
    # take a second after "We found a custom integration briard" gets a 404, the product fallback
    # working correctly and the rig asking too early. The view readiness is waited for on top,
    # because a config entry is not the same event as our setup; the wait is bounded, so a view
    # that never appears still fails.
    node1.wait_until_succeeds(
        f"curl -fsS -o /dev/null -X POST -H 'Authorization: Bearer {access}' "
        "-H 'Content-Type: application/json' -d '{\"hold\":false}' "
        "${ha}/api/briard/quiesce",
        timeout=120,
    )
    # The trigger is the harness (`--clock` supplies what the host would); everything under it is
    # the product path.
    node1.succeed("briard-guest-agent --clock=home-assistant")
    nightlies = node1.succeed(
        "ls -1 /var/lib/briard/data/.snapshots | grep '^home-assistant-clock-' || true"
    ).split()
    assert nightlies, (
        "the clock sample took no member:\n"
        + node1.succeed("journalctl -t briard-guest-agent --no-pager -l | tail -40 || true")
    )
    nightly = nightlies[0]
    nightly_meta = _sj.loads(node1.succeed(f"cat /var/lib/briard/data/.snapshots/{nightly}.json"))
    assert nightly_meta["trigger"] == "clock", nightly_meta
    # The claim, and it is the guest's to make: the host renders `crash`, and only the code that
    # watched Home Assistant hold still upgrades it. `quiesced` here means the view answered, the
    # recorder took the lock, and it was still held when the snapshot finished.
    assert nightly_meta["consistency"] == "quiesced", (
        f"the clock sample is {nightly_meta['consistency']!r}, so Home Assistant did not hold still. "
        "If this began failing after a Home Assistant bump, check that "
        "recorder.lock_database/unlock_database still exist:\n"
        + node1.succeed("podman logs briard-home-assistant-app 2>&1 | tail -40 || true")
    )
    print(f"the clock sample {nightly} was taken with Home Assistant's recorder locked")

    # Our integration imports off the event loop. Home Assistant flags a blocking import on the
    # loop at every start and refuses some blocking calls from custom integrations outright, so a
    # stub importing on the loop is one release from not loading at all. The integration is set up
    # by now (the quiesce above went through it).
    blocking = node1.succeed(
        "journalctl --no-pager -u briard-home-assistant-app.service "
        "| grep -c 'Detected blocking call to import_module with args (.briard_ha.,)' || true"
    ).strip()
    # The control: the same query must see our integration load, or a journal holding none of Home
    # Assistant's output would pass the assertion below by finding nothing at all.
    loaded = node1.succeed(
        "journalctl --no-pager -u briard-home-assistant-app.service | grep -c 'custom integration briard' || true"
    ).strip()
    assert loaded != "0", "the journal query sees none of Home Assistant's lines; the check below would be vacuous"
    assert blocking == "0", f"our integration imported on the event loop {blocking} time(s)"

    # ── Healing, at a boundary that does not restart the container ───────────────────
    #
    # s6 re-executes `run` at every s6 service start, and `homeassistant.restart` is one (HA exits
    # 100; s6 restarts the service, the container never dies). A config restore comes back through
    # the same boundary (it wipes /config including `.storage/auth`, then returns via this exit-100
    # path), so a mint that heals a mismatch here heals the restore. Deliberately measured without
    # a container restart: a mechanism that needed one would be a different, worse design.
    ctr = "briard-home-assistant-app"
    started_before = node1.succeed(f"podman inspect -f '{{{{.State.StartedAt}}}}' {ctr}").strip()

    # One member per boundary. A container start is the unit pre-start's to take (it reads the
    # clean-stop marker), so `run` notifies on every run but the container's first. Counted in the
    # unit's journal, where both land: the pre-start logs `app-starting …: took`, and notify.py
    # prints `briard: <outcome>` on every call, first after the journal prefix (`]: briard: `);
    # the integration logs `[briard_ha] briard: …` too, which a bare `briard: ` would count.
    # ⚠️ Count the call, not the success: measured with the skip removed, the first-run notify
    # lands in the pre-start's second and is refused as a name collision, so `briard: took` stays
    # at zero and a count of it passes either way. The pre-start line makes the zero mean
    # something: it proves the query sees this unit's output.
    unit_log = "journalctl --no-pager -u briard-home-assistant-app.service"
    pre_took = node1.succeed(f"{unit_log} | grep -c 'app-starting home-assistant: took' || true").strip()
    assert pre_took != "0", "the unit pre-start took no member; the zero below would be vacuous"
    run_took = node1.succeed(f"{unit_log} | grep -c '\\]: briard: ' || true").strip()
    assert run_took == "0", f"`run` notified {run_took} time(s) on the container's first run"
    node1.succeed(f"podman exec {ctr} test -e /run/briard-started")

    # Rotate the node's value out from under a live HA: HA's store and tmpfs now disagree.
    node1.succeed("head -c 64 /dev/urandom | od -An -tx1 | tr -d ' \n' > /run/briard/home-assistant/token")
    rotated = node1.succeed("cat /run/briard/home-assistant/token").strip()
    assert rotated != token, "the rotation wrote the same value"
    node1.fail(f"curl -fsS -X POST ${ha}/auth/token -d grant_type=refresh_token -d refresh_token={rotated}")

    # ⚠️ Ask whether HA can be restarted; that it answers is a different fact.
    # `homeassistant.restart` creates a task for `hass.async_stop` and returns 200 without waiting,
    # and `async_stop` returns bare on `CoreState.not_running` (the branch commented "just ignore",
    # the one neighbour that logs nothing). That state lasts from construction until
    # `async_start()`, through all of integration setup, while the http integration has served
    # since early in that phase, so a restart asked for on a 200 from /manifest.json can be
    # discarded in silence, with nothing to retry it. `/api/config` carries HA's own state, so
    # RUNNING is the boundary to wait on, as the second restart in this file does. Measured
    # without it: two of six contended tier runs lost the restart here, and HA served on for the
    # next thirty minutes.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {access}' ${ha}/api/config "
        "| grep -qE '\"state\": ?\"RUNNING\"'",
        timeout=300,
    )
    # Also the admin proof. Reading config entries does not need admin (measured against
    # 2026.7.1: `ConfigManagerEntryIndexView.get` carries no `@require_admin`, only the flow views
    # do, and a group-less system user reads the list with a 200). Acting does, and
    # `/api/services/...` returns 401 for a non-admin, so this line fails if the mint ever stops
    # putting our user in the admin group.
    node1.succeed(
        f"curl -fsS -X POST -H 'Authorization: Bearer {access}' "
        "${ha}/api/services/homeassistant/restart"
    )
    node1.wait_until_succeeds("curl -fsS -o /dev/null ${ha}${fixture.healthPath}", timeout=300)

    # Wait for the transition, do not assume it. The in-process restart is not instant, and
    # /manifest.json keeps answering from the old process for a moment after the call is accepted
    # (measured here at 0.04s); a single-shot check then reads the pre-restart token state and
    # fails a working mint. Waiting on the rotated value, proven refused above, is the honest form:
    # the boundary being crossed, with a deadline rather than a hope.
    node1.wait_until_succeeds(
        "curl -fsS -o /dev/null -X POST ${ha}/auth/token "
        f"-d grant_type=refresh_token -d refresh_token={rotated}",
        timeout=300,
    )

    # The mismatch healed itself at the boundary: the new value answers...
    assert exchange(rotated).startswith("ey"), "the mint did not heal the token at the restart boundary"
    # ...and the prune revoked the old one, which is what makes a token resurrected by a
    # backup restore harmless.
    node1.fail(f"curl -fsS -X POST ${ha}/auth/token -d grant_type=refresh_token -d refresh_token={token}")
    # ...without the container ever restarting.
    started_after = node1.succeed(f"podman inspect -f '{{{{.State.StartedAt}}}}' {ctr}").strip()
    assert started_before == started_after, (
        f"the container restarted ({started_before} -> {started_after}); the mint must ride "
        "the s6 service-start boundary, not a container bounce"
    )
    # ...and that restart, the container's second run, is the one `run` notifies for.
    node1.wait_until_succeeds(f"test \"$({unit_log} | grep -c '\\]: briard: ')\" = 1", timeout=60)

    # ── Through the reverse proxy ───────────────────────────────────────────────────────
    #
    # Routing to a runtime-installed app, keyed on the app name, from the table the guest
    # converge wrote. Without it an installed HA answers only on :8123 while :80 on the VIP
    # serves only the page of Briard itself.
    host = routed_host(node1, "home-assistant")
    assert host == "briard-brave-elf-home-assistant.local", f"the node composed {host}"

    # HA itself, through the reverse proxy, on :80 -- no port in the address.
    node1.wait_until_succeeds(
        f"curl -fsS -o /dev/null -H 'Host: {host}' http://192.168.1.100${fixture.healthPath}", timeout=120
    )
    # And /healthz under that name is not the node's answer: under a routed name every path belongs
    # to the app (HA has no /healthz of its own, so HA's 404 comes back, which is the proof, since
    # the node's own /healthz is a 200 saying "reverse proxy up"). The node's answer stays at the
    # bare address, where the OS health gate reads it, which is why a wedged app cannot change it.
    under_name = node1.succeed(f"curl -sS -H 'Host: {host}' http://192.168.1.100/healthz || true")
    assert "reverse proxy up" not in under_name, (
        f"/healthz under the app's name was answered by the reverse proxy: {under_name!r}"
    )
    node_health = node1.succeed("curl -fsS http://192.168.1.100/healthz")
    assert "2 app(s) routed" in node_health, f"the node's /healthz = {node_health!r}"
    for svc in ("home-assistant", "${mosquitto.name}"):
        assert svc in node_health, f"the node's /healthz does not name what it routes: {node_health!r}"

    # One list, so "published but not routed" need not be asserted: it is not expressible. The
    # mDNS responder in the reverse proxy reads this same table; there is no flattened second copy
    # for it to read while stale (the routes.hosts check below).

    # ── (b) Does HA's own zeroconf see what the guest announces? ────────
    #
    # The gate, a measurement rather than a feature: an in-HA integration discovered over zeroconf
    # needs one leg nothing else verifies, that HA's zeroconf sees the guest mDNS from inside the
    # container. It decides the integration reach into HA, the worth of (d) and the wiring of Music
    # Assistant, and it is cheap, so it is measured rather than reasoned (python-zeroconf sets
    # SO_REUSEADDR/SO_REUSEPORT so sharing :5353 is normal, and Linux defaults IP_MULTICAST_LOOP on
    # for IPv4, but "should work" is not a measurement).
    #
    # ⚠️ Also the coexistence gate for the responder in the reverse proxy. HA is host-networked,
    # so its python-zeroconf and that responder bind :5353 in one namespace, and the reverse proxy
    # is a promoter chain member: a responder that cannot bind does not merely lose the name, it
    # fails the reverse proxy and hands the resource to a peer that would fail identically. Both
    # run at once here, so the pairing is exercised on every run rather than argued.
    #
    # ⚠️ Inside the container on purpose, not from the guest shell: under host networking both
    # share a netns, so a guest-side probe ought to behave identically, but that is the assumption
    # under test, and a probe outside the container cannot refute it. This runs HA's own zeroconf
    # library, in its interpreter, in the namespace HA lives in.
    #
    # ⚠️ What a green here does not buy: HA browses only the service types some manifest declares
    # (`async_get_zeroconf`), and `_mqtt._tcp` appears nowhere in home-assistant/core, so Home
    # Assistant will not act on this record until (d) teaches mqtt to ask for it. What is proven is
    # the transport: a record the guest publishes is visible to the stack of HA.
    import json as _zj
    host_mq = routed_host(node1, "${mosquitto.name}")
    want = next(
        s for s in _zj.loads(node1.succeed("cat /run/briard/routes.json"))["apps"]
        if s["name"] == "${mosquitto.name}"
    )["announce"][0]
    seen = node1.succeed(
        "podman exec -i briard-home-assistant-app python3 - < ${zeroconfProbe}"
    )
    print("HA's zeroconf saw: " + seen.strip())
    # Every expected value is the node's, read from the table it published from.
    assert want["name"] in seen, f"HA's zeroconf did not see {want['name']!r}: {seen!r}"
    assert str(want["port"]) in seen, f"the record HA saw carries the wrong port: {seen!r}"
    assert host_mq in seen, f"the record HA saw does not point at {host_mq!r}: {seen!r}"
    assert "192.168.1.100" in seen, f"the record HA resolved does not carry the VIP: {seen!r}"

    # ── HA announces itself on the household's NIC ───────────────────────────
    #
    # Home Assistant picks the interface it advertises and answers mDNS on (and the address it
    # builds its own URL from, which every integration hands to LAN devices) by the source address
    # the kernel gives it for 224.0.0.251. Left to the default route that is eth0, qemu's user-net,
    # which nothing on the LAN can reach (measured without the route: the advert carried only
    # 10.0.2.15). briard-vip routes multicast out of the VIP's NIC. HA is installed after the
    # promotion here, so this proves the route, not the chain order; hass-failover starts HA
    # during a promotion.
    node1.succeed("ip route show 224.0.0.0/4 | grep -q dev")
    advert = _zj.loads(node1.succeed(
        "podman exec -i briard-home-assistant-app python3 - < ${h.haAdvertProbe}"
    ))
    print(f"HA's own advert: {advert}")
    assert "192.168.1.100" in advert["addresses"], f"HA does not advertise the VIP: {advert}"
    assert "10.0.2.15" not in advert["addresses"], f"HA still advertises the user-net address: {advert}"
    assert "10.0.2.15" not in advert["txt"].get("internal_url", ""), f"HA's own URL is the user-net address: {advert}"

    node1.fail("test -e /run/briard/routes.hosts")

    # ── (c) Briard's own integration, and the broker wired from inside it ───
    #
    # A household that installs the broker should not then have to tell Home Assistant about it.
    # The wiring goes through HA's own config-flow API, so `async_validate_broker_settings` runs on
    # submit and a dead broker yields an error instead of an entry pointing at nothing, and it is
    # driven in-process by briard's own integration, which needs no token, no HTTP and no waiting
    # for HA to serve, because it is the serving HA.
    #
    # Only a stub lands in /config; the implementation is bind-mounted outside it, which keeps the
    # restore wipe and the household's backups away from briard's code. That is invisible from
    # outside, so the test asserts both halves separately, then that Home Assistant loaded the
    # thing they add up to.
    #
    # A fresh token: the healing section above rotated the value and pruned every other token on
    # our user, so the `access` from before that boundary is revoked by design.
    wired = exchange(node1.succeed("cat /run/briard/home-assistant/token").strip())

    # The stub, planted into /config by the wrapper in the stopped window s6's `run` provides, is a
    # real package, not a symlink into the mount: a household restoring its backup outside briard
    # would restore a dangling link, and HA's scan skips what is not a directory, leaving a
    # permanent "Integration 'briard' not found" against the `briard:` line below.
    node1.succeed(f"test -f {dataroot}/${fixture.dataDir}/custom_components/briard/__init__.py")
    node1.succeed(f"test -f {dataroot}/${fixture.dataDir}/custom_components/briard/manifest.json")
    # And the line that switches it on, in a file the household owns. Re-planted at every start.
    node1.succeed(f"grep -q '^briard:' {dataroot}/${fixture.dataDir}/configuration.yaml")

    # ⚠️ The household is left holding stock Home Assistant plus one line, and the line above alone
    # would not catch the failure that matters. On a node's first boot /config is empty and briard
    # must make `configuration.yaml` exist before HA reads it, so it relays
    # `async_ensure_config_exists` rather than authoring anything: `_write_default_config` is
    # skipped entirely once the file exists, so a briard-authored file would silently cost the
    # household `default_config` (most of Home Assistant) and the three `!include` files it names.
    # That regression leaves `^briard:` satisfied, hence the separate stock-half assertions.
    for stock in ("default_config:", "automation: !include automations.yaml"):
        node1.succeed(f"grep -qF '{stock}' {dataroot}/${fixture.dataDir}/configuration.yaml")
    for made in ("automations.yaml", "scripts.yaml", "scenes.yaml", "secrets.yaml", ".HA_VERSION"):
        node1.succeed(f"test -f {dataroot}/${fixture.dataDir}/{made}")
    print("the household's configuration.yaml:\n" + node1.succeed(f"cat {dataroot}/${fixture.dataDir}/configuration.yaml"))
    # The implementation is not in /config, the whole placement decision: what is not there cannot
    # be wiped by a restore or ride out in a backup.
    node1.fail(f"test -e {dataroot}/${fixture.dataDir}/custom_components/briard/briard_ha.py")
    node1.succeed("test -f /run/briard/home-assistant/integration/briard_ha.py")

    # Home Assistant loaded it. `/api/config` lists the components HA set up, so this is its own
    # verdict on the whole chain at once: the stub answered the custom-component scan, the
    # manifest passed the loader's version check, the `briard:` line reached the config HA read,
    # and the stub resolved an implementation that lives outside /config.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {wired}' ${ha}/api/config "
        "| grep -q '\"briard\"'",
        timeout=300,
    )

    # The node's alerts as Repairs issues. No host agent runs here, so the copy the guest agent
    # writes for `dashboard.alerts` is written by hand the same way: beside its name, then moved in.
    # Repairs shows what needs attention by the rule of the dashboard and the tray: the latest
    # record of each key that is an open condition or an event, at warning or above, not dismissed;
    # one issue per record (key and time), so the next record of a key is a new issue. A resolved,
    # an info and a dismissed alert make none; dismissing or resolving removes the issue. HA's own
    # websocket is the judge, and the strings HA loaded for the domain prove the planted
    # translation reached it.
    node1.succeed(f"test -f {dataroot}/${fixture.dataDir}/custom_components/briard/translations/en.json")
    # Briard's mark, served by HA's own brands API from beside the planted stub -- the URL HA's
    # integration page and Repairs ask for. Compared with the staged copy, byte for byte.
    node1.succeed(
        f"curl -fsS -H 'Authorization: Bearer {wired}' ${ha}/api/brands/integration/briard/icon.png "
        "| cmp - /run/briard/home-assistant/stub/brand/icon.png"
    )
    import shlex
    def push_alerts(records):
        node1.succeed(
            f"echo {shlex.quote(_zj.dumps(records))} > /run/briard/alerts/alerts.json.new "
            "&& mv -f /run/briard/alerts/alerts.json.new /run/briard/alerts/alerts.json"
        )
    def rec(key, kind, sev, title, minute=0, dismissed=False):
        r = {"key": key, "kind": kind, "severity": sev, "title": title, "body": f"body of {title}", "at": f"2026-10-07T12:{minute:02d}:00Z"}
        if dismissed:
            r["dismissed"] = True
        return r
    def repairs():
        return _zj.loads(node1.succeed(
            f"podman exec -i -e TOKEN={wired} briard-home-assistant-app python3 - < ${repairsProbe}"
        ))
    def await_issue(issue_id):
        node1.wait_until_succeeds(
            f"podman exec -i -e TOKEN={wired} briard-home-assistant-app python3 - < ${repairsProbe} | grep -qF '\"issue_id\": \"{issue_id}\"'",
            timeout=120,
        )
        return sorted(i["issue_id"] for i in repairs()["issues"])
    disk_open = rec("disk", "open", "warning", "Disk space low")
    unreachable = rec("host-unreachable", "event", "warning", "The host agent was unreachable", minute=2)
    push_alerts([
        disk_open,
        rec("clock", "open", "warning", "Clock not synchronised"),
        rec("clock", "resolved", "", "Clock synchronised", minute=1),
        rec("upgrade-rolled-back:os", "event", "info", "Update rolled back"),
        rec("fast-nic", "open", "info", "Fast network card"),
        rec("backup", "open", "warning", "Backup not working", dismissed=True),
        unreachable,
    ])
    got_ids = await_issue("disk@2026-10-07T12:00:00Z")
    want_ids = ["disk@2026-10-07T12:00:00Z", "host-unreachable@2026-10-07T12:02:00Z"]
    assert got_ids == want_ids, f"want exactly the warning condition and the warning event: {got_ids}"
    got = repairs()
    disk = [i for i in got["issues"] if i["issue_id"].startswith("disk@")][0]
    assert disk["severity"] == "warning" and not disk["is_fixable"], disk
    assert disk["translation_key"] == "alert" and disk["translation_placeholders"]["title"] == "Disk space low", disk
    assert disk["learn_more_url"] == "http://briard.local/#alert-disk", disk
    assert got["strings"].get("component.briard.issues.alert.title") == "{title}", f"HA did not load the issue strings: {got['strings']}"
    # Disk dismissed on the dashboard, and the event again: the first issue goes, the event's is a new one.
    push_alerts([dict(disk_open, dismissed=True), unreachable, rec("host-unreachable", "event", "warning", "The host agent was unreachable", minute=3)])
    got_ids = await_issue("host-unreachable@2026-10-07T12:03:00Z")
    assert got_ids == ["host-unreachable@2026-10-07T12:03:00Z"], f"want only the event's new record: {got_ids}"
    push_alerts([disk_open, rec("disk", "resolved", "", "Disk space recovered", minute=4)])
    node1.wait_until_succeeds(
        f"podman exec -i -e TOKEN={wired} briard-home-assistant-app python3 - < ${repairsProbe} | grep -q '\"issues\": \\[\\]'",
        timeout=120,
    )
    print("what needs attention became Repairs issues, one per record, and dismissal and resolution removed them")

    # The apps' updates as update entities, from the file the guest agent writes beside the alert
    # copy for `dashboard.catalog` (written by hand here, the same way). An offered update turns
    # the app's entity on, with both versions and the app's page on the dashboard as its link; a
    # copy whose latest is what runs turns it off. The entity cannot install: the page does that.
    def push_apps(records):
        node1.succeed(
            f"echo {shlex.quote(_zj.dumps(records))} > /run/briard/alerts/apps.json.new "
            "&& mv -f /run/briard/alerts/apps.json.new /run/briard/alerts/apps.json"
        )
    def updates():
        return _zj.loads(node1.succeed(
            f"podman exec -i -e TOKEN={wired} briard-home-assistant-app python3 - < ${updatesProbe}"
        ))
    push_apps([{"name": "home-assistant", "title": "Home Assistant", "running": "2026.9.4", "latest": "2026.10.1", "update": True}])
    node1.wait_until_succeeds(
        f"podman exec -i -e TOKEN={wired} briard-home-assistant-app python3 - < ${updatesProbe} | grep -q '\"state\": \"on\"'",
        timeout=120,
    )
    got = updates()
    assert [u["entity_id"] for u in got] == ["update.briard_home_assistant"], f"want exactly Home Assistant's: {got}"
    attrs = got[0]["attributes"]
    assert attrs["installed_version"] == "2026.9.4" and attrs["latest_version"] == "2026.10.1", attrs
    assert attrs["release_url"] == "http://briard.local/apps/home-assistant", attrs
    assert attrs["supported_features"] == 0, f"the entity must not install: {attrs}"
    push_apps([{"name": "home-assistant", "title": "Home Assistant", "running": "2026.10.1", "latest": "2026.10.1", "update": False}])
    node1.wait_until_succeeds(
        f"podman exec -i -e TOKEN={wired} briard-home-assistant-app python3 - < ${updatesProbe} | grep -q '\"state\": \"off\"'",
        timeout=120,
    )
    print("the node's offered update became an update entity, and taking it turned the entity off")

    # The external URL, from the file the guest agent writes for `dashboard.casa` (written by hand
    # here, the same way). An empty slot is filled live, with no restart. A set one is never
    # overwritten, judged after more than one whole poll (30 s, a timer running since setup) has
    # passed with a different URL offered.
    def ext_url():
        return _zj.loads(node1.succeed(
            f"curl -fsS -H 'Authorization: Bearer {wired}' ${ha}/api/config"
        )).get("external_url")
    def offer(url):
        node1.succeed(
            f"printf %s {shlex.quote(url)} > /run/briard/home-assistant/external-url.new "
            "&& mv -f /run/briard/home-assistant/external-url.new /run/briard/home-assistant/external-url"
        )
    assert ext_url() is None, f"a fresh Home Assistant already has an external URL: {ext_url()}"
    offer("https://home-assistant.brave-elf.briard.casa")
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {wired}' ${ha}/api/config "
        "| grep -q '\"external_url\": *\"https://home-assistant.brave-elf.briard.casa\"'",
        timeout=120,
    )
    # And HA's own mDNS record carries it now, with no restart: HA builds that record once at boot,
    # so without the integration's re-announce it would still hand phones the internal URL.
    casa_url = "https://home-assistant.brave-elf.briard.casa"
    for _ in range(12):
        txt = _zj.loads(node1.succeed(
            "podman exec -i briard-home-assistant-app python3 - < ${h.haAdvertProbe}"
        ))["txt"]
        if txt.get("base_url") == casa_url:
            break
        node1.sleep(5)
    assert txt.get("external_url") == casa_url and txt.get("base_url") == casa_url, f"HA's record still announces {txt}"
    print(f"HA's record announces the external URL with no restart: {txt}")
    offer("https://home-assistant.other.briard.casa")
    node1.sleep(35)
    assert ext_url() == "https://home-assistant.brave-elf.briard.casa", f"the set external URL was overwritten: {ext_url()}"
    print("the offered URL became Home Assistant's external URL, and a set one was left alone")

    # The entry is waited for once, then its contents are read, rather than polled for existence:
    # an entry naming the wrong broker would satisfy any existence check.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {wired}' "
        "'${ha}/api/config/config_entries/entry?domain=mqtt' | grep -q entry_id",
        timeout=300,
    )
    mqtt_entries = _zj.loads(node1.succeed(
        f"curl -fsS -H 'Authorization: Bearer {wired}' "
        "'${ha}/api/config/config_entries/entry?domain=mqtt'"
    ))
    assert len(mqtt_entries) == 1, f"want exactly one mqtt entry, got {mqtt_entries}"
    # It loaded: the flow validated the broker by connecting to it, so "loaded" is Home Assistant
    # reporting that it reached the broker, not that a file was written.
    assert mqtt_entries[0]["state"] == "loaded", f"the mqtt entry is not loaded: {mqtt_entries[0]}"
    assert mqtt_entries[0]["source"] == "user", f"unexpected flow source: {mqtt_entries[0]}"
    print("HA is wired to the broker: " + _zj.dumps(mqtt_entries[0]))

    # ⚠️ And it is idempotent, the guard that matters most: the integration runs at every HA start,
    # and mqtt is `single_config_entry: true`, so a second entry is not merely untidy: a household
    # pointing HA at their own broker must never find their one slot taken by a localhost entry
    # they did not ask for. The wiring is what the integration does when HA starts, not a script
    # to re-run, so the honest way to run it twice is to start Home Assistant twice.
    #
    # ⚠️ Wait for the first start to finish before asking for the second. Measured: restarting
    # seconds after a previous restart had `/api/services` answer 4xx while HA was still coming
    # back, failing the test for a reason unrelated to the guard. `/api/config` reports HA's own
    # state, so "RUNNING" is that boundary rather than a sleep.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {wired}' ${ha}/api/config "
        "| grep -qE '\"state\": ?\"RUNNING\"'",
        timeout=300,
    )
    node1.succeed(
        f"curl -fsS -X POST -H 'Authorization: Bearer {wired}' "
        "${ha}/api/services/homeassistant/restart"
    )
    node1.wait_until_succeeds("curl -fsS -o /dev/null ${ha}${fixture.healthPath}", timeout=300)
    # A fresh exchange again, waited for rather than assumed: the mint at the restart boundary
    # replaces the refresh token object behind our value, revoking every access token issued
    # before it, and /auth/token answers only once HA's auth store is back up.
    token_now = node1.succeed("cat /run/briard/home-assistant/token").strip()
    node1.wait_until_succeeds(
        "curl -fsS -o /dev/null -X POST ${ha}/auth/token "
        f"-d grant_type=refresh_token -d refresh_token={token_now}",
        timeout=300,
    )
    restarted = exchange(token_now)
    again = _zj.loads(node1.succeed(
        f"curl -fsS -H 'Authorization: Bearer {restarted}' "
        "'${ha}/api/config/config_entries/entry?domain=mqtt'"
    ))
    assert len(again) == 1, f"a second start created another entry: {again}"
    assert again[0]["entry_id"] == mqtt_entries[0]["entry_id"], (
        f"the entry was replaced rather than left alone: {again} vs {mqtt_entries}"
    )

    # ── (d) A running Home Assistant is re-wired without a restart ──────────
    #
    # Everything above happens at an HA start; this closes the gap. converge restarts only the apps
    # whose rendered bytes changed, so installing the broker beside an already running Home
    # Assistant leaves it running, unwired, with nothing in the product that will ever restart it:
    # the household is told the broker is installed and HA goes on disagreeing until somebody
    # happens to restart it. The proof has to be that nothing restarted, hence the container start
    # time read before and after: an entry coming back is satisfied just as well by an HA that
    # bounced, and a bounce is what this exists to avoid.
    #
    # It drives the real push half (agent/hass's Nudge, via hass-nudge) rather than a curl, because
    # the unit tests know Home Assistant's event view only from a stub of it. Here that belief meets
    # the pinned image: an admin-gated view, a Bearer from our own system token, a JSON-object body.
    started_at = node1.succeed(
        "podman inspect -f '{{.State.StartedAt}}' briard-home-assistant-app"
    ).strip()

    # Deleted, not disabled, by the rule the integration keeps: a disabled entry still occupies the
    # household's one mqtt slot and is a durable no. Only an empty slot is filled, so deleting is
    # the one way to ask for the wiring again, and what a household that removed it and then
    # reinstalled the broker would have done.
    node1.succeed(
        f"curl -fsS -X DELETE -H 'Authorization: Bearer {restarted}' "
        f"${ha}/api/config/config_entries/entry/{again[0]['entry_id']}"
    )
    node1.wait_until_fails(
        f"curl -fsS -H 'Authorization: Bearer {restarted}' "
        "'${ha}/api/config/config_entries/entry?domain=mqtt' | grep -q entry_id",
        timeout=60,
    )

    # ⚠️ Wait for "RUNNING" before nudging, not a sleep dressed up: a nudge landing while HA is
    # still starting is dropped on purpose (the start hook will run, against a settled Home
    # Assistant), so nudging a `starting` HA would prove nothing and pass anyway.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {restarted}' ${ha}/api/config "
        "| grep -qE '\"state\": ?\"RUNNING\"'",
        timeout=300,
    )
    print(node1.succeed("hass-nudge ${toString fixture.port}"))

    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {restarted}' "
        "'${ha}/api/config/config_entries/entry?domain=mqtt' | grep -q entry_id",
        timeout=120,
    )
    rewired = _zj.loads(node1.succeed(
        f"curl -fsS -H 'Authorization: Bearer {restarted}' "
        "'${ha}/api/config/config_entries/entry?domain=mqtt'"
    ))
    assert len(rewired) == 1, f"want exactly one mqtt entry after the nudge, got {rewired}"
    # Loaded again: the flow validated the broker by connecting a second time, so the whole wiring
    # ran, not just a file being written back.
    assert rewired[0]["state"] == "loaded", f"the re-wired entry is not loaded: {rewired[0]}"
    assert rewired[0]["entry_id"] != again[0]["entry_id"], (
        f"the entry was never actually deleted: {rewired} vs {again}"
    )
    # And Home Assistant never restarted. Same container, same start time: the wiring happened
    # inside the process that was already serving the household.
    still = node1.succeed("podman inspect -f '{{.State.StartedAt}}' briard-home-assistant-app").strip()
    assert still == started_at, (
        f"Home Assistant restarted between the delete and the re-wire ({started_at} -> {still}); "
        "the nudge proves nothing unless the process is the same one"
    )
    print("re-wired in place, no restart: " + _zj.dumps(rewired[0]))

    # ── (e) Onboarding through the dashboard, resumable by HA's own frontend ──
    #
    # The install ends by handing the household a logged-in Home Assistant: briard creates the
    # first user through HA's onboarding API and sends the browser to HA's own onboarding page with
    # the returned code, which resumes at the first undone step. Every claim below is the server
    # half, driven through the reverse proxy under the name the browser would use, because the code
    # is bound to the client_id and HA's `getAuth` refuses a callback whose state names any other
    # origin (`limitHassInstance`). The frontend half is source-verified (ha-onboarding.ts
    # `_fetchOnboardingSteps`, `_curStep`) and not driven here.
    import shlex as _sx
    # The browser is at the name the dashboard links: the bare alias the reverse proxy routes beside
    # the flock-scoped name, so that is the origin the code is bound to.
    host = "briard-home-assistant.local"
    assert host in next(
        s for s in _zj.loads(node1.succeed("cat /run/briard/routes.json"))["apps"]
        if s["name"] == "home-assistant"
    )["hosts"], "the bare alias is not routed"
    origin = f"http://{host}"
    client_id = origin + "/"  # genClientId(): protocol//host + trailing slash
    def through_proxy(path, bearer=None, post=None, form=None):
        """One request through the reverse proxy, as the browser would make it."""
        cmd = f"curl -fsS -H 'Host: {host}' "
        if bearer:
            cmd += f"-H 'Authorization: Bearer {bearer}' "
        if post is not None:
            cmd += f"-X POST -H 'Content-Type: application/json' -d {_sx.quote(_zj.dumps(post))} "
        if form is not None:
            cmd += "-X POST " + " ".join(f"-d {_sx.quote(k + '=' + v)}" for k, v in form.items()) + " "
        return node1.succeed(cmd + f"http://192.168.1.100{path}")
    def steps_done():
        """HA's own view of onboarding, unauthenticated -- the thing our button reads."""
        return {s["step"]: s["done"] for s in _zj.loads(through_proxy("/api/onboarding"))}

    # Nothing is done, in particular not the user step: HA marks it done at startup if any owner
    # exists, and the system user the control channel minted is not one. The first half of "the
    # briard admin is the HA owner": our own user must not take the flag before the household's.
    assert steps_done() == {"user": False, "core_config": False, "analytics": False, "integration": False}, steps_done()

    # The minter refuses with no owner: before the user step there is none (only our system user,
    # which never takes the flag), and the integration's login view answers 409 rather than
    # minting for whatever admin exists. The refuse-and-surface branch, measured on a real Home
    # Assistant, not a fake; the control channel's own token is the caller, as the dashboard's
    # would be.
    mint_url = "http://192.168.1.100/api/briard/login"
    mint_body = _sx.quote(_zj.dumps({"client_id": client_id}))
    # A fresh exchange: the restarts above rotated the refresh token, revoking every access token
    # issued before them, so `access` from the first claim is dead by now.
    system = exchange(node1.succeed("cat /run/briard/home-assistant/token").strip())
    refused = node1.succeed(
        f"curl -sS -o /dev/null -w '%{{http_code}}' -X POST -H 'Host: {host}' -H 'Authorization: Bearer {system}' "
        f"-H 'Content-Type: application/json' -d {mint_body} {mint_url}"
    ).strip()
    assert refused == "409", f"the minter answered {refused} with no owner; want 409"
    # And with no bearer at all it is HA's own 401: the view sits behind HA's auth, not beside it.
    bare = node1.succeed(
        f"curl -sS -o /dev/null -w '%{{http_code}}' -X POST -H 'Host: {host}' -H 'Content-Type: application/json' -d {mint_body} {mint_url}"
    ).strip()
    assert bare == "401", f"the minter answered {bare} to an unauthenticated call; want 401"

    # The user step runs through the dashboard, the way the household's does. The host's one-time
    # code is written as the `dashboard.handoff` verb writes it (no host agent drives this rig; the
    # verb has its own test) and redeemed under the node's own name at the reverse proxy, which
    # forwards every name it does not route to the dashboard; then "Open Home Assistant" does the
    # user step and sends the browser on with the code.
    node_name = "briard-brave-elf.local"
    code0 = "c0ffee" * 10 + "0123"
    issued = node1.succeed("date -u +%Y-%m-%dT%H:%M:%SZ").strip()
    handoff = _zj.dumps({"code": code0, "name": "Kostas", "username": "kostas", "language": "en", "issued": issued})
    node1.succeed("mkdir -p -m 0700 /run/briard/dashboard")
    node1.succeed(f"printf %s {_sx.quote(handoff)} > /run/briard/dashboard/handoff.json && chmod 0600 /run/briard/dashboard/handoff.json")
    # No session: refused, and the refusal says nothing about what is installed.
    node1.fail(f"curl -fsS -H 'Host: {node_name}' http://192.168.1.100/")
    # The code: one redemption, an HttpOnly cookie, and the handoff is gone.
    headers = node1.succeed(f"curl -sS -D - -o /dev/null -H 'Host: {node_name}' 'http://192.168.1.100/?code={code0}'")
    assert " 303 " in headers.splitlines()[0], headers
    cookies = [l.split(":", 1)[1].split(";")[0].strip() for l in headers.splitlines() if l.lower().startswith("set-cookie: briard_session=")]
    assert cookies and "HttpOnly" in headers, headers
    cookie = cookies[0]
    node1.fail("test -e /run/briard/dashboard/handoff.json")
    node1.fail(f"curl -fsS -o /dev/null -H 'Host: {node_name}' 'http://192.168.1.100/?code={code0}'")
    # Trusted: the page offers the first open; the button is gated on RUNNING, not on HTTP.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Host: {node_name}' -H 'Cookie: {cookie}' http://192.168.1.100/ | grep -q 'Open Home Assistant'",
        timeout=120,
    )
    # The button: a 303 to HA's own onboarding page, under HA's name, carrying the code and the
    # state HA's frontend checks (hassUrl without the trailing slash, clientId with it).
    from urllib.parse import urlparse as _up, parse_qs as _pq
    import base64 as _b64
    loc = node1.succeed(
        f"curl -sS -o /dev/null -w '%{{redirect_url}}' -X POST -H 'Host: {node_name}' -H 'Cookie: {cookie}' "
        "http://192.168.1.100/open/home-assistant"
    ).strip()
    u = _up(loc)
    assert (u.scheme, u.netloc, u.path) == ("http", host, "/onboarding.html"), loc
    q = _pq(u.query)
    assert q.get("auth_callback") == ["1"] and q.get("code"), loc
    assert _zj.loads(_b64.b64decode(q["state"][0])) == {"hassUrl": origin, "clientId": client_id}, loc
    first = {"auth_code": q["code"][0]}
    # The password is kept by the integration in Home Assistant's own storage ("On your phone",
    # below): nothing of it on briard's side of the volume. Every later open is minted (below).
    node1.fail("ls /var/lib/briard/dashboard | grep -qi password")

    # The owner flag, read from the store on the volume rather than asked of HA: the household's
    # user holds it, alone, and our system user is still system_generated and not owner. The auth
    # store saves on a short delay, so wait for the flag to land, then parse.
    auth_store = f"{dataroot}/${fixture.dataDir}/.storage/auth"
    node1.wait_until_succeeds(f"grep -q '\"is_owner\": true' {auth_store}", timeout=30)
    users = _zj.loads(node1.succeed(f"cat {auth_store}"))["data"]["users"]
    owners = [u for u in users if u.get("is_owner")]
    assert [u["name"] for u in owners] == ["Kostas"], f"owners: {owners}"
    ours = [u for u in users if u.get("system_generated")]
    assert ours and not any(u.get("is_owner") for u in ours), f"the system user took ownership: {ours}"

    # The resume, as getAuth does it: the code exchanges for tokens with the same client_id.
    # This is what `onboarding.html?auth_callback=1&code=…&state=…` does on arrival.
    tokens = _zj.loads(through_proxy("/auth/token", form={
        "grant_type": "authorization_code", "code": first["auth_code"], "client_id": client_id,
    }))
    human = tokens["access_token"]
    assert tokens.get("refresh_token"), f"the exchange returned no refresh token: {tokens}"
    # ...and the page the browser lands on is served while onboarding is in progress.
    through_proxy("/onboarding.html")

    # Analytics was marked done by the dashboard, out of order and with the control channel's
    # token, before the browser was sent on: preferences untouched means off, and the frontend
    # resumes at the first undone step (`_curStep`), so this is the page it skips. The browser's
    # code was not spent on it; it exchanged above.
    assert steps_done() == {"user": True, "core_config": False, "analytics": True, "integration": False}, steps_done()

    # Core config, the step HA's location page ends on; marking it starts HA's four default
    # integrations. Two create their entry unconditionally and prove the step ran (their setup
    # wants the internet this VM lacks; that is not the claim). The third, `met`, is the
    # measurement: its onboarding flow aborts `no_home` while the home coordinates are unset or
    # still HA's default (met/config_flow.py, 2026.7.1), as here, since nobody ran the location
    # page. Measured: a 120s wait for a `met` entry runs out; none ever comes. That absence is the
    # cost of completing core_config by API instead of handing the browser to the location page,
    # and why the install does the latter. The negative is not vacuous: all three flows start in
    # the same loop and `met`'s abort has no await before it, so once both unconditional entries
    # exist the `met` flow has run and chosen.
    through_proxy("/api/onboarding/core_config", bearer=human, post={})
    for domain in ("radio_browser", "google_translate"):
        node1.wait_until_succeeds(
            f"curl -fsS -H 'Host: {host}' -H 'Authorization: Bearer {human}' "
            f"'http://192.168.1.100/api/config/config_entries/entry?domain={domain}' | grep -q entry_id",
            timeout=120,
        )
    met = _zj.loads(through_proxy("/api/config/config_entries/entry?domain=met", bearer=human))
    assert met == [], f"met set itself up without a home location: {met}"

    # The integration step hands back a fresh code for the final login. ⚠️ Called exactly once,
    # with the human token and a same-origin redirect: the view marks itself done before it
    # validates either (views.py, 2026.7.1), so a wrong call burns the step with no code.
    final = _zj.loads(through_proxy("/api/onboarding/integration", bearer=human, post={
        "client_id": client_id, "redirect_uri": origin + "/?auth_callback=1",
    }))
    assert final.get("auth_code") and final["auth_code"] != first["auth_code"], f"no fresh code: {final}"
    landed = _zj.loads(through_proxy("/auth/token", form={
        "grant_type": "authorization_code", "code": final["auth_code"], "client_id": client_id,
    }))
    assert landed.get("refresh_token"), f"the final code did not exchange: {landed}"

    # Onboarded. Every step done, and the user step refuses forever after: the 403 that makes
    # "fresh HA only" true for the first-open path.
    assert all(steps_done().values()), steps_done()
    node1.fail(
        f"curl -fsS -X POST -H 'Host: {host}' -H 'Content-Type: application/json' "
        f"-d {_sx.quote(_zj.dumps({'name': 'x', 'username': 'x', 'password': 'x', 'client_id': client_id, 'language': 'en'}))} "
        "http://192.168.1.100/api/onboarding/users"
    )
    print("onboarded by API: " + _zj.dumps(steps_done()))

    # The later open: on a set-up Home Assistant the button mints a login for the owner through
    # the integration and lands the browser on HA's own auth callback (the front page,
    # auth_callback=1, the same state as the resume, storeToken so it outlives the tab).
    def owner_tokens():
        """HA's refresh tokens for the browser's client_id, and the owner they must belong to."""
        store = _zj.loads(node1.succeed(f"cat {auth_store}"))["data"]
        owner = [u["id"] for u in store["users"] if u.get("is_owner")][0]
        return owner, [t for t in store["refresh_tokens"] if t.get("client_id") == client_id]
    # The baseline is waited for: two codes exchanged above (`first`, `final`) mean two tokens for
    # this client_id, and the store lands them on a debounce; read too early it holds one, and the
    # later open then looks like two.
    node1.wait_until_succeeds(f"[ $(grep -c '\"client_id\": \"{client_id}\"' {auth_store}) -ge 2 ]", timeout=30)
    owner_id, before = owner_tokens()
    assert len(before) == 2, f"tokens for {client_id} before the later open: {len(before)}, want the two exchanged above"
    later = node1.succeed(
        f"curl -sS -o /dev/null -w '%{{redirect_url}}' -X POST -H 'Host: {node_name}' -H 'Cookie: {cookie}' "
        "http://192.168.1.100/open/home-assistant"
    ).strip()
    u = _up(later)
    assert (u.scheme, u.netloc, u.path) == ("http", host, "/"), later
    q = _pq(u.query)
    assert q.get("auth_callback") == ["1"] and q.get("storeToken") == ["true"] and q.get("code"), later
    assert _zj.loads(_b64.b64decode(q["state"][0])) == {"hassUrl": origin, "clientId": client_id}, later
    minted = q["code"][0]
    assert minted not in (first["auth_code"], final["auth_code"]), "the later open reused a code"
    # The code exchanges, and logs in as the owner: HA's own refresh-token store names the user
    # each token belongs to, and the new one is the household owner's, not our system user's or
    # anyone else's.
    landed_later = _zj.loads(through_proxy("/auth/token", form={
        "grant_type": "authorization_code", "code": minted, "client_id": client_id,
    }))
    assert landed_later.get("refresh_token"), f"the minted code did not exchange: {landed_later}"
    # The store saves on a short delay, and the earlier codes' tokens carry the same client_id, so
    # the evidence is one more token, every one of them the owner's.
    node1.wait_until_succeeds(
        f"[ $(grep -c '\"client_id\": \"{client_id}\"' {auth_store}) -ge {len(before) + 1} ]", timeout=30
    )
    _, after = owner_tokens()
    assert len(after) == len(before) + 1, f"tokens for {client_id}: {len(before)} -> {len(after)}"
    assert all(t["user_id"] == owner_id for t in after), f"a token for {client_id} is not the owner's: {after}"
    # Spent: the same code again is refused.
    node1.fail(
        f"curl -fsS -H 'Host: {host}' -X POST -d grant_type=authorization_code -d code={minted} "
        f"-d client_id={_sx.quote(client_id)} http://192.168.1.100/auth/token"
    )
    print("later open minted for the owner: " + owner_id)

    # ON YOUR PHONE: the companion app signs in on HA's own login page, with the owner's username
    # and the four words the first open set and the integration keeps, in HA's own storage beside
    # the auth store. Proven the only way a password can be: HA's own login flow, through the
    # reverse proxy as the app reaches it.
    import re as _re
    def on_phone(new=False):
        """The dialog's three values as the dashboard shows them, and the dialog."""
        path = "/phone/home-assistant?new" if new else "/phone/home-assistant"
        body = node1.succeed(
            f"curl -fsS -X POST -H 'Host: {node_name}' -H 'Cookie: {cookie}' -H 'HX-Request: true' "
            f"'http://192.168.1.100{path}'"
        )
        return _re.findall(r'<p class="key"[^>]*>([^<]*)</p>', body), body
    def login(pw):
        """HA's own username/password flow; the flow's final result."""
        flow = _zj.loads(through_proxy("/auth/login_flow", post={
            "client_id": client_id, "handler": ["homeassistant", None], "redirect_uri": origin + "/",
        }))
        return _zj.loads(through_proxy(f"/auth/login_flow/{flow['flow_id']}", post={
            "client_id": client_id, "username": "kostas", "password": pw,
        }))
    shown, dialog = on_phone()
    assert len(shown) == 3 and shown[1] == "kostas" and len(shown[2].split(" ")) == 4, f"the dialog: {dialog}"
    kept = shown[2]
    signed = login(kept)
    assert signed.get("type") == "create_entry" and signed.get("result"), f"the kept words do not sign in: {signed}"
    phone_store = auth_store.rsplit("/", 1)[0] + "/briard.phone"
    node1.succeed(f"grep -qF {_sx.quote(kept)} {phone_store}")

    # They outlive a restart: read again from HA's storage at its next start. The crossing is
    # waited for, not assumed: the control channel's access token dies with the old process.
    system = exchange(node1.succeed("cat /run/briard/home-assistant/token").strip())
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {system}' ${ha}/api/config | grep -qE '\"state\": ?\"RUNNING\"'",
        timeout=300,
    )
    node1.succeed(f"curl -fsS -X POST -H 'Authorization: Bearer {system}' ${ha}/api/services/homeassistant/restart")
    node1.wait_until_fails(f"curl -fsS -o /dev/null -H 'Authorization: Bearer {system}' ${ha}/api/config", timeout=300)
    token_now = node1.succeed("cat /run/briard/home-assistant/token").strip()
    node1.wait_until_succeeds(
        f"curl -fsS -o /dev/null -X POST ${ha}/auth/token -d grant_type=refresh_token -d refresh_token={token_now}",
        timeout=300,
    )
    system = exchange(token_now)
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {system}' ${ha}/api/config | grep -qE '\"state\": ?\"RUNNING\"'",
        timeout=300,
    )
    shown, dialog = on_phone()
    assert shown == ["http://briard-home-assistant.local", "kostas", kept], f"after a restart the dialog shows {shown}: {dialog}"
    assert login(kept).get("type") == "create_entry", "the kept words stopped signing in at a restart"

    # New password: four new words that sign in, the old ones refused, and no session ended --
    # the later open's refresh token still exchanges.
    shown, dialog = on_phone(new=True)
    assert len(shown) == 3 and len(shown[2].split(" ")) == 4 and shown[2] != kept, f"New password: {dialog}"
    assert login(shown[2]).get("type") == "create_entry", "the new words do not sign in"
    old = login(kept)
    assert old.get("type") == "form" and old.get("errors"), f"the replaced words still sign in: {old}"
    through_proxy("/auth/token", form={"grant_type": "refresh_token", "refresh_token": landed_later["refresh_token"], "client_id": client_id})
    assert on_phone()[0] == shown, "the next open does not show the new words"
    print("on your phone: the kept words sign in, outlive a restart, and a new password ends no session")

    # The device list and revoke: the Trusted devices page lists this browser and points at HA's
    # own profile for the session it holds there. Revoking takes it out of briard's registry and
    # nothing else (HA's store keeps every refresh token it held; registry-only by decision); the
    # cookie is refused after, on the page and the button; the way back is a fresh code, as at
    # install.
    trusted_page = node1.succeed(f"curl -fsS -H 'Host: {node_name}' -H 'Cookie: {cookie}' http://192.168.1.100/devices")
    assert "this device" in trusted_page and f'href="{origin}/profile/security"' in trusted_page, trusted_page
    ids = _re.findall(r'name="id" value="([0-9a-f]+)"', trusted_page)
    assert len(ids) == 1, f"devices on the page: {ids}"
    registry = "/var/lib/briard/dashboard/devices.json"
    assert [d["id"] for d in _zj.loads(node1.succeed(f"cat {registry}"))["devices"]] == ids
    tokens_before = len(owner_tokens()[1])
    revoked = node1.succeed(
        f"curl -sS -o /dev/null -w '%{{http_code}}' -X POST -H 'Host: {node_name}' -H 'Cookie: {cookie}' "
        f"-d id={ids[0]} http://192.168.1.100/devices/revoke"
    ).strip()
    assert revoked == "303", f"revoke answered {revoked}; want 303"
    node1.fail(f"curl -fsS -H 'Host: {node_name}' -H 'Cookie: {cookie}' http://192.168.1.100/")
    node1.fail(f"curl -fsS -X POST -H 'Host: {node_name}' -H 'Cookie: {cookie}' http://192.168.1.100/open/home-assistant")
    assert _zj.loads(node1.succeed(f"cat {registry}"))["devices"] == [], "the registry still holds the revoked device"
    assert len(owner_tokens()[1]) == tokens_before, "revoking a briard device touched Home Assistant's tokens"
    code1 = "decade" * 10 + "4567"
    handoff = _zj.dumps({"code": code1, "name": "Kostas", "username": "kostas", "language": "en", "issued": node1.succeed("date -u +%Y-%m-%dT%H:%M:%SZ").strip()})
    node1.succeed(f"printf %s {_sx.quote(handoff)} > /run/briard/dashboard/handoff.json && chmod 0600 /run/briard/dashboard/handoff.json")
    again = node1.succeed(f"curl -sS -D - -o /dev/null -H 'Host: {node_name}' 'http://192.168.1.100/?code={code1}'")
    assert " 303 " in again.splitlines()[0], f"a fresh code after the last device left answered: {again}"
    laptop = [l.split(":", 1)[1].split(";")[0].strip() for l in again.splitlines() if l.lower().startswith("set-cookie: briard_session=")][0]
    print(f"device revoked from the registry only; HA kept its {tokens_before} tokens")

    # Quick-connect: a second browser asks and shows a six-digit code, and the trusted one types
    # it. The session goes to the asker's own cookie (the code alone collects nothing), and nobody
    # untrusted can approve. Measured through the reverse proxy under the node's name, the way
    # both browsers would reach it.
    asked = node1.succeed(f"curl -sS -D - -o /dev/null -X POST -A 'phone/1.0' -H 'Host: {node_name}' http://192.168.1.100/join")
    assert " 303 " in asked.splitlines()[0], asked
    join = [l.split(":", 1)[1].split(";")[0].strip() for l in asked.splitlines() if l.lower().startswith("set-cookie: briard_join=")][0]
    waiting = node1.succeed(f"curl -fsS -H 'Host: {node_name}' -H 'Cookie: {join}' http://192.168.1.100/join")
    m = _re.search(r'<p class="code">(\d{3}) (\d{3})</p>', waiting)
    assert m and 'http-equiv="refresh"' in waiting, waiting
    join_code = m.group(1) + m.group(2)
    # Untrusted cannot approve; a code nobody shows approves nothing.
    node1.fail(f"curl -fsS -X POST -H 'Host: {node_name}' -d code={join_code} http://192.168.1.100/devices/approve")
    node1.fail(f"curl -fsS -X POST -H 'Host: {node_name}' -H 'Cookie: {laptop}' -d code=000000 http://192.168.1.100/devices/approve")
    # Still waiting, then the trusted device types the code.
    node1.succeed(f"curl -fsS -H 'Host: {node_name}' -H 'Cookie: {join}' http://192.168.1.100/join | grep -q 'class=\"code\"'")
    approved = node1.succeed(
        f"curl -sS -o /dev/null -w '%{{http_code}}' -X POST -H 'Host: {node_name}' -H 'Cookie: {laptop}' "
        f"-d 'code={join_code[:3]} {join_code[3:]}' http://192.168.1.100/devices/approve"
    ).strip()
    assert approved == "303", f"approve answered {approved}; want 303"
    # The code alone (no join cookie) collects nothing; the asker's next refresh collects.
    bare = node1.succeed(f"curl -sS -D - -o /dev/null -H 'Host: {node_name}' http://192.168.1.100/join")
    assert " 410 " in bare.splitlines()[0] and "briard_session=" not in bare.lower(), bare
    collected = node1.succeed(f"curl -sS -D - -o /dev/null -H 'Host: {node_name}' -H 'Cookie: {join}' http://192.168.1.100/join")
    assert " 303 " in collected.splitlines()[0], collected
    phone = [l.split(":", 1)[1].split(";")[0].strip() for l in collected.splitlines() if l.lower().startswith("set-cookie: briard_session=")][0]
    node1.fail(f"curl -fsS -H 'Host: {node_name}' -H 'Cookie: {join}' http://192.168.1.100/join")
    both = node1.succeed(f"curl -fsS -H 'Host: {node_name}' -H 'Cookie: {phone}' http://192.168.1.100/devices")
    assert "phone/1.0" in both and len(_re.findall(r'name="id" value="', both)) == 2, both
    assert len(_zj.loads(node1.succeed(f"cat {registry}"))["devices"]) == 2
    print("quick-connect: the phone was let in by its code, typed on the laptop")

  '';
}
