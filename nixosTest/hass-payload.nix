# Home Assistant INSTALLED AS A SERVICE, data on the DRBD volume.
#
# The same path drbd-promote proves for the dummy, with the real HA image (digest-pinned in a
# signed manifest): bring r0 up, let drbd-reactor promote and start the ordered chain, install HA
# onto the volume it now holds, and assert HA actually boots and serves — and that its recorder
# SQLite + `.storage` landed on the DRBD btrfs subvolume. Single node: this validates the service
# wiring, not failover (that is hass-failover), and HA is heavy enough that one instance is the
# right cost.
#
# It also carries [V3b.30](b), the one measurement that needs real HA and a real announcement in
# one place: whether HA's OWN zeroconf library, inside HA's own container, sees a record the
# guest published. The broker is installed alongside for exactly that, and for nothing else.
#
# HA reachable THROUGH the front door under its own name is asserted again, at the bottom of this
# file ([B.48] landed the routing table). It was lost when the build-time service slot went
# ([V3b.3](e2)) -- the door's `-backend` came from that slot, so with the slot gone no node routed
# to a service at all -- and it comes back keyed on the SERVICE's name rather than on there being
# exactly one thing to forward to, which is what the table changed.
{ pkgs, guestModule, fixture, mosquitto }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # The zeroconf probe, as its OWN file rather than a heredoc inside the test script. Nix's
  # indented strings strip the SMALLEST indentation found in the string, so a Python body sitting
  # at column 0 stops anything from being stripped and silently de-indents the whole test script
  # around it -- which is a driver syntax error a long way from its cause.
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

  node = h.mkNode {
    # The broker rides along for ONE claim -- [V3b.30](b), at the bottom of this file: whether
    # HA's own zeroconf stack sees what the guest announces. It needs a real service record on
    # the LAN, and the product publishes exactly one.
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
      # hass-nudge is the REAL push half of the control channel as a command (agent/hass's Nudge),
      # so claim (d) drives the shipping code against the real Home Assistant rather than a curl
      # that resembles it -- the same split service-probe makes in services-pair.nix.
      environment.systemPackages = [ (pkgs.callPackage ./hass-nudge-pkg.nix { }) ];
      # HA needs real memory + disk headroom: the 2.4 GB image is `podman load`ed
      # onto the writable root (disk, not RAM), and HA's Python stack wants ~1 GB live.
      # 2048 is MEASURED, not guessed: a guest grows page cache into whatever it is given, so
      # this test sat at 3166 MB resident of 3072 declared and would have sat at 4192 of 4096.
      # At 2048 it is 2143 MB resident and the same 129s ([B.127]).
      virtualisation.memorySize = 2048;
      virtualisation.diskSize = 10240;
    };

  # HA boot is slow and the promoter selects the primary dynamically.
  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}
    node1.start()
    node1.wait_for_unit("multi-user.target")
    # HA's 2.4 GB image is loaded before anything promotes -- an install must not be the thing
    # that fetches it, and on the failover path a pull would be fatal.
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=600)
    node1.succeed("modprobe drbd")
    node1.succeed("drbdadm create-md --force r0")
    node1.succeed("systemctl start drbd@r0.target")
    # Single node: no peer to connect to — just make it UpToDate so it's promotable.
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    # Arm the one-time format the way BRING-UP does ([B.126]): the product no longer formats on
    # the promotion path, so a harness that seeds a resource by hand leaves the same marker.
    node1.succeed("mkdir -p /run/briard && touch /run/briard/data.format")

    # Give the node a flock name before anything converges, the way the agent does at bring-up:
    # the per-service names are composed from it, so a node with none routes nothing.
    name_the_flock(node1)

    # Hand off to the promoter: it has quorum alone, promotes, and runs the ordered chain. Nothing
    # is installed yet, so the front door comes up answering for itself.
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-data.service", timeout=120)
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)

    # THE INSTALL: HA's manifest onto the volume, then the product's own converge renders it,
    # warms it (already resident) and starts it.
    dataroot = install_fixture(node1)
    assert dataroot == "/var/lib/briard/home-assistant", f"the renderer chose {dataroot}"
    # The broker, for the zeroconf claim at the bottom. Installed through the same converge as HA.
    install_fixture(node1, service="${mosquitto.name}")

    # The DRBD device is mounted; HA's /config subvolume lives on it. (Assert the
    # real mount, not the subvolume path — a fresh btrfs subvolume isn't reliably
    # seen as a mountpoint until traversed; the data-file checks below confirm
    # HA's state actually lands on the subvolume.)
    node1.succeed("mountpoint -q /var/lib/briard")

    # HA serves at the VIP. /manifest.json is static + unauthenticated, so a 200
    # means HA's HTTP stack is up — the readiness signal the health-gate will use.
    # Generous timeout: first boot initializes the recorder DB + onboarding store.
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json", timeout=300)

    # The unit that runs HA came from the RENDERER, not from this file.
    for unit in fixture_units(node1):
        node1.succeed(f"systemctl is-active {unit}")

    # The claim: HA's state landed on the DRBD subvolume — the recorder SQLite
    # and the `.storage` config, so they replicate + snapshot as one unit.
    node1.wait_until_succeeds(f"test -f {dataroot}/app/home-assistant_v2.db", timeout=60)
    node1.succeed(f"test -d {dataroot}/app/.storage")
    node1.succeed(f"test -f {dataroot}/app/configuration.yaml")

    # ── THE CONTROL CHANNEL (agent/hass) ─────────────────────────────────────────────
    #
    # THIS IS THE CATALOG GATE. Briard shadow-mounts its own script over the s6 `run` the
    # image owns, and hands over to that image's own `run` extracted byte-for-byte. The
    # compatibility risk of doing so is deliberately relocated HERE: an upstream layout
    # change (s6 v2 -> v3, a moved service directory) must fail a blessed image on its way
    # into the catalog, never a fleet of homes at once. Everything below runs against the
    # image the manifest actually pins.
    def exchange(refresh):
        """Refresh -> access, the documented exchange, client_id omitted."""
        return node1.succeed(
            "curl -fsS -X POST http://127.0.0.1:8123/auth/token "
            f"-d grant_type=refresh_token -d refresh_token={refresh} "
            "| sed 's/.*\"access_token\":\"\\([^\"]*\\)\".*/\\1/'"
        ).strip()

    # The value is OURS and is chosen first, on tmpfs, 0600 — so a consumer knows the token
    # from t=0, with no return channel out of the container and no startup race.
    node1.succeed("test -f /run/briard/hass/token")
    perms = node1.succeed("stat -c %a /run/briard/hass/token").strip()
    assert perms == "600", f"the token is mode {perms}; it is a credential for the whole HA API"
    token = node1.succeed("cat /run/briard/hass/token").strip()

    # The mint ran inside the container, in the stopped window s6's `run` provides, and the
    # token HA now holds is the one we chose.
    access = exchange(token)
    assert access.startswith("ey"), f"the token did not exchange for an access token: {access}"

    # The signal the S1 readiness gate reads, through the token: HA's per-config-entry
    # setup states. WAITED FOR, not sampled — /manifest.json goes 200 while HA is still
    # setting up default_config, so the list is legitimately EMPTY for a few seconds after
    # the door opens (measured: the first run of this assertion failed on `[]`). Waiting is
    # also the stronger claim: the entries appear and stay readable.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {access}' "
        "http://127.0.0.1:8123/api/config/config_entries/entry | grep -q entry_id",
        timeout=300,
    )
    entries = node1.succeed(
        f"curl -fsS -H 'Authorization: Bearer {access}' "
        "http://127.0.0.1:8123/api/config/config_entries/entry"
    )
    assert '"state"' in entries, f"config entries carry no state: {entries[:200]}"

    # ── HEALING, AT A BOUNDARY THAT DOES NOT RESTART THE CONTAINER ───────────────────
    #
    # s6 re-executes `run` at every SERVICE start, and `homeassistant.restart` is one
    # (HA exits 100; s6 restarts the service, the container never dies). That is the same
    # boundary a config restore comes back through — a restore wipes /config including
    # `.storage/auth`, then returns via exactly this exit-100 path — so proving the mint
    # heals a mismatch here proves it heals the restore. Deliberately measured WITHOUT a
    # container restart: a mechanism that needed one would be a different, worse design.
    ctr = "briard-home-assistant-app"
    started_before = node1.succeed(f"podman inspect -f '{{{{.State.StartedAt}}}}' {ctr}").strip()

    # Rotate the node's value out from under a live HA: HA's store and tmpfs now disagree.
    node1.succeed("head -c 64 /dev/urandom | od -An -tx1 | tr -d ' \n' > /run/briard/hass/token")
    rotated = node1.succeed("cat /run/briard/hass/token").strip()
    assert rotated != token, "the rotation wrote the same value"
    node1.fail(f"curl -fsS -X POST http://127.0.0.1:8123/auth/token -d grant_type=refresh_token -d refresh_token={rotated}")

    # ⚠️ ASK WHETHER HA CAN BE RESTARTED; THAT IT ANSWERS IS A DIFFERENT FACT. `homeassistant.restart`
    # creates a task for `hass.async_stop` and returns 200 without waiting for it, and `async_stop`
    # returns BARE on `CoreState.not_running` -- the branch whose comment is "just ignore", and the
    # one neighbour of it that logs nothing at all. That state runs from construction until
    # `async_start()`, i.e. through the whole of integration setup, while the http integration has
    # been serving since early in the same phase. So a restart asked for on the strength of a 200
    # from /manifest.json can be discarded in silence, with nothing to retry it. `/api/config`
    # carries HA's own state, so RUNNING is the boundary to wait on -- the same gate the second
    # restart in this file already uses. [B.127] measured the alternative: two of six contended
    # tier runs lost the restart exactly here, and HA served on for the next thirty minutes.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {access}' http://127.0.0.1:8123/api/config "
        "| grep -qE '\"state\": ?\"RUNNING\"'",
        timeout=300,
    )
    # ALSO THE ADMIN PROOF. Reading config entries does NOT need admin — measured against
    # 2026.7.1: `ConfigManagerEntryIndexView.get` carries no `@require_admin` (only the flow
    # views do), and a group-less system user reads the list with a 200. What needs admin is
    # ACTING, and `/api/services/...` is exactly that: the same call returns 401 for a
    # non-admin. So this line is what would fail if the mint ever stopped putting our user in
    # the admin group.
    node1.succeed(
        f"curl -fsS -X POST -H 'Authorization: Bearer {access}' "
        "http://127.0.0.1:8123/api/services/homeassistant/restart"
    )
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://127.0.0.1:8123/manifest.json", timeout=300)

    # WAIT FOR THE TRANSITION, DO NOT ASSUME IT. HA's in-process restart is not instant and
    # /manifest.json keeps answering from the OLD process for a moment after the call is accepted
    # -- measured here at 0.04s, which is how this raced. A single-shot check at that instant reads
    # the PRE-restart token state and fails a working mint. Waiting on the rotated value itself is
    # the honest form: it was proven REFUSED four lines above, so this is the boundary being
    # crossed, with a deadline rather than a hope.
    node1.wait_until_succeeds(
        "curl -fsS -o /dev/null -X POST http://127.0.0.1:8123/auth/token "
        f"-d grant_type=refresh_token -d refresh_token={rotated}",
        timeout=300,
    )

    # The mismatch healed itself at the boundary: the new value answers...
    assert exchange(rotated).startswith("ey"), "the mint did not heal the token at the restart boundary"
    # ...and the prune revoked the old one, which is what makes a token resurrected by a
    # backup restore harmless.
    node1.fail(f"curl -fsS -X POST http://127.0.0.1:8123/auth/token -d grant_type=refresh_token -d refresh_token={token}")
    # ...without the container ever restarting.
    started_after = node1.succeed(f"podman inspect -f '{{{{.State.StartedAt}}}}' {ctr}").strip()
    assert started_before == started_after, (
        f"the container restarted ({started_before} -> {started_after}); the mint must ride "
        "the service-start boundary, not a container bounce"
    )

    # ── THROUGH THE FRONT DOOR ───────────────────────────────────────────────────────
    #
    # THE ASSERTION THIS FILE OWED BACK ([B.48]). Until [V3.15] the door forwarded to a backend
    # baked at guest-build time; [V3b.3](e2) deleted that slot, and for two epochs no node routed
    # to a runtime-installed service at all -- an installed HA answered only on :8123 while the
    # VIP's :80 served Briard's own page. It comes back keyed on the service's NAME, from the table
    # the guest's converge wrote.
    host = routed_host(node1, "home-assistant")
    assert host == "briard-brave-elf-home-assistant.local", f"the node composed {host}"

    # HA itself, through the door, on :80 -- no port in the address.
    node1.wait_until_succeeds(
        f"curl -fsS -o /dev/null -H 'Host: {host}' http://192.168.1.100/manifest.json", timeout=120
    )
    # And /healthz under that name is NOT the node's answer: under a name we route, EVERY path
    # belongs to the service (HA has no /healthz of its own, so what comes back is HA's 404 --
    # which is precisely the proof, because the node's own /healthz is a 200 saying "front door
    # up"). The node's answer is still there at the bare address, where the OS health gate reads
    # it, and that is why a wedged service cannot change it.
    under_name = node1.succeed(f"curl -sS -H 'Host: {host}' http://192.168.1.100/healthz || true")
    assert "front door up" not in under_name, (
        f"/healthz under the service's name was answered by the front door: {under_name!r}"
    )
    node_health = node1.succeed("curl -fsS http://192.168.1.100/healthz")
    assert "2 service(s) routed" in node_health, f"the node's /healthz = {node_health!r}"
    for svc in ("home-assistant", "${mosquitto.name}"):
        assert svc in node_health, f"the node's /healthz does not name what it routes: {node_health!r}"

    # ONE LIST, so "published but not routed" is no longer something to assert — it is not
    # expressible. The mDNS publisher reads this same table with jq; there is no flattened second
    # copy for it to read while stale, and this is the assertion that keeps it that way.

    # ── (b) DOES HA'S OWN ZEROCONF SEE WHAT THE GUEST ANNOUNCES? ([V3b.30](b)) ────────
    #
    # THE GATE, and it is a MEASUREMENT rather than a feature: [V3b.29] §6.5 sketched an in-HA
    # integration discovered over zeroconf and left two legs unverified, of which this is one --
    # that HA's zeroconf sees the guest's mDNS FROM INSIDE THE CONTAINER. It decides [V3b.30](a)'s
    # reach into HA, (d)'s worth, and Music Assistant's wiring, and it is cheap, so it is measured
    # rather than reasoned. The reasoning said it SHOULD work -- python-zeroconf sets
    # SO_REUSEADDR/SO_REUSEPORT so sharing :5353 with avahi is normal, and Linux defaults
    # IP_MULTICAST_LOOP on for IPv4 -- and "should" is not a measurement.
    #
    # ⚠️ INSIDE THE CONTAINER ON PURPOSE, not from the guest shell. Under host networking the two
    # share a netns, so the socket behaviour ought to be identical and a guest-side probe would be
    # the easier thing to write -- but "ought to be identical" is the assumption under test, and a
    # probe run outside the container cannot refute it. This runs HA's OWN zeroconf library, in
    # HA's own interpreter, in the namespace HA actually lives in.
    #
    # ⚠️ WHAT A GREEN HERE DOES NOT BUY, so nobody reads more into it: HA browses only the service
    # types some manifest declares (`async_get_zeroconf`), and `_mqtt._tcp` appears NOWHERE in
    # home-assistant/core. So Home Assistant will not act on this record until (d) teaches mqtt to
    # ask for it. What is proven is the TRANSPORT -- that a record the guest publishes is visible
    # to HA's stack -- which is the leg §6.5 could not verify.
    import json as _zj
    host_mq = routed_host(node1, "${mosquitto.name}")
    want = next(
        s for s in _zj.loads(node1.succeed("cat /run/briard/routes.json"))["services"]
        if s["name"] == "${mosquitto.name}"
    )["announce"][0]
    seen = node1.succeed(
        "podman exec -i briard-home-assistant-app python3 - < ${zeroconfProbe}"
    )
    print("HA's zeroconf saw: " + seen.strip())
    # Every expected value is the NODE's, read from the table it published from.
    assert want["name"] in seen, f"HA's zeroconf did not see {want['name']!r}: {seen!r}"
    assert str(want["port"]) in seen, f"the record HA saw carries the wrong port: {seen!r}"
    assert host_mq in seen, f"the record HA saw does not point at {host_mq!r}: {seen!r}"
    assert "192.168.1.100" in seen, f"the record HA resolved does not carry the VIP: {seen!r}"

    node1.fail("test -e /run/briard/routes.hosts")

    # ── (c) BRIARD'S OWN INTEGRATION, AND THE BROKER WIRED FROM INSIDE IT ([B.124]) ───
    #
    # A household that installs the broker should not then have to tell Home Assistant about it.
    # The wiring is driven through HA's OWN config-flow API -- so `async_validate_broker_settings`
    # runs on submit and a dead broker yields an error instead of an entry pointing at nothing --
    # and since [B.124] it is driven IN-PROCESS, by briard's own integration, which needs no
    # token, no HTTP and no waiting for HA to serve because it IS the serving HA.
    #
    # WHAT THE PLACEMENT COSTS AND WHY IT IS ASSERTED HERE. Only a stub lands in /config; the
    # implementation is bind-mounted outside it. That is what keeps the restore wipe and the
    # household's backups away from briard's code, and it is invisible from the outside -- so the
    # test asserts both halves separately, and then asserts that Home Assistant actually loaded
    # the thing they add up to.
    #
    # A FRESH token: the healing section above rotated the value and pruned every other token on
    # our user, so the `access` from before that boundary is revoked by design.
    wired = exchange(node1.succeed("cat /run/briard/hass/token").strip())

    # The stub, planted into /config by the wrapper in the stopped window s6's `run` provides --
    # a REAL package, not a symlink into the mount: a household restoring its backup outside
    # briard would restore a dangling link, and HA's scan skips what is not a directory, leaving
    # a permanent "Integration 'briard' not found" against the `briard:` line below.
    node1.succeed(f"test -f {dataroot}/app/custom_components/briard/__init__.py")
    node1.succeed(f"test -f {dataroot}/app/custom_components/briard/manifest.json")
    # And the line that switches it on, in a file the household owns. Re-planted at every start.
    node1.succeed(f"grep -q '^briard:' {dataroot}/app/configuration.yaml")

    # ⚠️ WHAT THE HOUSEHOLD IS LEFT HOLDING IS STOCK HOME ASSISTANT PLUS ONE LINE, and the line
    # above alone would not have caught the failure that matters. On a node's first boot /config
    # is empty and briard has to make `configuration.yaml` exist before HA reads it, so the file
    # the household lives with is one briard had a hand in. It RELAYS `async_ensure_config_exists`
    # rather than authoring anything -- because `_write_default_config` is skipped entirely once
    # the file exists, so a briard-authored file would silently cost the household `default_config`
    # (most of Home Assistant) and the three `!include` files it names. That regression leaves
    # `^briard:` perfectly satisfied, which is why the stock half is asserted separately.
    for stock in ("default_config:", "automation: !include automations.yaml"):
        node1.succeed(f"grep -qF '{stock}' {dataroot}/app/configuration.yaml")
    for made in ("automations.yaml", "scripts.yaml", "scenes.yaml", "secrets.yaml", ".HA_VERSION"):
        node1.succeed(f"test -f {dataroot}/app/{made}")
    print("the household's configuration.yaml:\n" + node1.succeed(f"cat {dataroot}/app/configuration.yaml"))
    # THE IMPLEMENTATION IS NOT IN /config, which is the whole placement decision: what is not
    # there cannot be wiped by a restore and cannot ride out in a backup.
    node1.fail(f"test -e {dataroot}/app/custom_components/briard/briard_ha.py")
    node1.succeed("test -f /run/briard/hass/integration/briard_ha.py")

    # HOME ASSISTANT LOADED IT. `/api/config` lists the components HA actually set up, so this is
    # HA's own verdict on the whole chain at once -- the stub answered the custom-component scan,
    # the manifest passed the loader's version check, the `briard:` line reached the config HA
    # read, and the stub resolved an implementation that lives outside /config.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {wired}' http://127.0.0.1:8123/api/config "
        "| grep -q '\"briard\"'",
        timeout=300,
    )

    # The entry is not asserted to exist "eventually" by polling forever: it is waited for once,
    # then its CONTENTS are read, because an entry naming the wrong broker would satisfy any
    # existence check.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {wired}' "
        "'http://127.0.0.1:8123/api/config/config_entries/entry?domain=mqtt' | grep -q entry_id",
        timeout=300,
    )
    mqtt_entries = _zj.loads(node1.succeed(
        f"curl -fsS -H 'Authorization: Bearer {wired}' "
        "'http://127.0.0.1:8123/api/config/config_entries/entry?domain=mqtt'"
    ))
    assert len(mqtt_entries) == 1, f"want exactly one mqtt entry, got {mqtt_entries}"
    # It LOADED -- the flow validated the broker by connecting to it, so a state of "loaded" is
    # Home Assistant reporting that it reached the broker, not that a file was written.
    assert mqtt_entries[0]["state"] == "loaded", f"the mqtt entry is not loaded: {mqtt_entries[0]}"
    assert mqtt_entries[0]["source"] == "user", f"unexpected flow source: {mqtt_entries[0]}"
    print("HA is wired to the broker: " + _zj.dumps(mqtt_entries[0]))

    # ⚠️ AND IT IS IDEMPOTENT, which is the guard that matters most: the integration runs at EVERY
    # HA start, and mqtt is `single_config_entry: true`, so a second entry is not merely untidy --
    # a household pointing HA at their own broker must never find their one slot taken by a
    # localhost entry they did not ask for.
    #
    # THE WHOLE START IS REPEATED, which is what changed with [B.124]: the wiring is no longer a
    # script that can be re-run on its own, it is what the integration does when Home Assistant
    # starts, so the honest way to run it twice is to start Home Assistant twice.
    #
    # ⚠️ WAIT FOR THE FIRST START TO FINISH BEFORE ASKING FOR THE SECOND. Measured on the wirer:
    # restarting seconds after a previous restart had the service API answer 4xx while HA was
    # still coming back, failing the test for a reason unrelated to the guard under test.
    # `/api/config` reports HA's own state, so "RUNNING" is that boundary rather than a sleep.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {wired}' http://127.0.0.1:8123/api/config "
        "| grep -qE '\"state\": ?\"RUNNING\"'",
        timeout=300,
    )
    node1.succeed(
        f"curl -fsS -X POST -H 'Authorization: Bearer {wired}' "
        "http://127.0.0.1:8123/api/services/homeassistant/restart"
    )
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://127.0.0.1:8123/manifest.json", timeout=300)
    # A fresh exchange again, and WAITED FOR rather than assumed: the mint at the restart boundary
    # replaces the refresh token object behind our value, so every access token issued before it
    # is revoked -- and /auth/token starts answering only once HA's auth store is back up.
    token_now = node1.succeed("cat /run/briard/hass/token").strip()
    node1.wait_until_succeeds(
        "curl -fsS -o /dev/null -X POST http://127.0.0.1:8123/auth/token "
        f"-d grant_type=refresh_token -d refresh_token={token_now}",
        timeout=300,
    )
    restarted = exchange(token_now)
    again = _zj.loads(node1.succeed(
        f"curl -fsS -H 'Authorization: Bearer {restarted}' "
        "'http://127.0.0.1:8123/api/config/config_entries/entry?domain=mqtt'"
    ))
    assert len(again) == 1, f"a second start created another entry: {again}"
    assert again[0]["entry_id"] == mqtt_entries[0]["entry_id"], (
        f"the entry was replaced rather than left alone: {again} vs {mqtt_entries}"
    )

    # ── (d) A RUNNING HOME ASSISTANT IS RE-WIRED WITHOUT A RESTART ([B.131]) ──────────
    #
    # Everything above happens at an HA start, and that is the gap this closes. converge restarts
    # only the services whose rendered bytes changed ([V3b.3](f)), so installing the broker beside
    # a Home Assistant that is ALREADY RUNNING leaves it running, unwired, and with nothing in the
    # product that will ever restart it -- the household is told the broker is installed and Home
    # Assistant goes on disagreeing until somebody happens to restart it.
    #
    # THE PROOF HAS TO BE THAT NOTHING RESTARTED, which is why the container's start time is read
    # before and after: an assertion that the entry came back is satisfied just as well by an HA
    # that bounced, and a bounce is exactly what this exists to avoid.
    #
    # It drives the REAL push half (agent/hass's Nudge, via the hass-nudge helper) rather than a
    # curl, because everything the unit tests know about Home Assistant's event view they know
    # from a stub of it. This is where that belief meets the pinned image: an admin-gated view, a
    # Bearer from our own system token, and a JSON-object body.
    started_at = node1.succeed(
        "podman inspect -f '{{.State.StartedAt}}' briard-home-assistant-app"
    ).strip()

    # DELETED, NOT DISABLED, and the difference is the rule the integration keeps: a disabled entry
    # still occupies the household's one mqtt slot and is a durable NO. Only an empty slot is
    # filled -- so deleting is the one way to ask for the wiring again, and it is what a household
    # who removed it and then reinstalled the broker would have done.
    node1.succeed(
        f"curl -fsS -X DELETE -H 'Authorization: Bearer {restarted}' "
        f"http://127.0.0.1:8123/api/config/config_entries/entry/{again[0]['entry_id']}"
    )
    node1.wait_until_fails(
        f"curl -fsS -H 'Authorization: Bearer {restarted}' "
        "'http://127.0.0.1:8123/api/config/config_entries/entry?domain=mqtt' | grep -q entry_id",
        timeout=60,
    )

    # ⚠️ WAIT FOR "RUNNING" BEFORE NUDGING, and it is not a sleep dressed up: a nudge that lands
    # while HA is still starting is DROPPED on purpose (the start hook is what will run, against a
    # settled Home Assistant), so nudging a `starting` HA would prove nothing and pass anyway.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {restarted}' http://127.0.0.1:8123/api/config "
        "| grep -qE '\"state\": ?\"RUNNING\"'",
        timeout=300,
    )
    print(node1.succeed("hass-nudge 8123"))

    node1.wait_until_succeeds(
        f"curl -fsS -H 'Authorization: Bearer {restarted}' "
        "'http://127.0.0.1:8123/api/config/config_entries/entry?domain=mqtt' | grep -q entry_id",
        timeout=120,
    )
    rewired = _zj.loads(node1.succeed(
        f"curl -fsS -H 'Authorization: Bearer {restarted}' "
        "'http://127.0.0.1:8123/api/config/config_entries/entry?domain=mqtt'"
    ))
    assert len(rewired) == 1, f"want exactly one mqtt entry after the nudge, got {rewired}"
    # LOADED again, so the flow validated the broker by connecting to it a second time -- the whole
    # wiring ran, not just a file being written back.
    assert rewired[0]["state"] == "loaded", f"the re-wired entry is not loaded: {rewired[0]}"
    assert rewired[0]["entry_id"] != again[0]["entry_id"], (
        f"the entry was never actually deleted: {rewired} vs {again}"
    )
    # AND HOME ASSISTANT NEVER RESTARTED. Same container, same start time: the wiring happened
    # inside the process that was already serving the household.
    still = node1.succeed("podman inspect -f '{{.State.StartedAt}}' briard-home-assistant-app").strip()
    assert still == started_at, (
        f"Home Assistant restarted between the delete and the re-wire ({started_at} -> {still}); "
        "the nudge proves nothing unless the process is the same one"
    )
    print("re-wired in place, no restart: " + _zj.dumps(rewired[0]))

    # ── (e) ONBOARDING THROUGH THE DASHBOARD, RESUMABLE BY HA'S OWN FRONTEND ([V3b.31a](f), [V3b.31b]) ──
    #
    # The install ends by handing the household a logged-in Home Assistant ([V3b.31a](d)): briard
    # creates the first user through HA's onboarding API and sends the browser to HA's OWN
    # onboarding page with the returned code, which resumes at the first undone step. Every claim
    # below is the SERVER half of that path, driven through the front door under the name the
    # browser would use -- because the code is bound to the client_id, and HA's `getAuth` refuses
    # a callback whose state names any other origin (`limitHassInstance`). The frontend half is
    # source-verified (ha-onboarding.ts `_fetchOnboardingSteps`, `_curStep`) and not driven here.
    import shlex as _sx
    origin = f"http://{host}"
    client_id = origin + "/"  # genClientId(): protocol//host + trailing slash
    def door(path, bearer=None, post=None, form=None):
        """One request through the front door, as the browser would make it."""
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
        return {s["step"]: s["done"] for s in _zj.loads(door("/api/onboarding"))}

    # NOTHING IS DONE, and in particular the USER step is not: HA marks it done at startup if any
    # OWNER exists, and the system user the control channel minted is not one. That is the
    # first half of "the briard admin is the HA owner" ([V3b.31a](e)) -- our own user must not
    # have taken the flag before the household's could.
    assert steps_done() == {"user": False, "core_config": False, "analytics": False, "integration": False}, steps_done()

    # THE MINTER REFUSES WITH NO OWNER ([V3b.31d], [V3b.31a](e)): before the user step there is
    # no owner -- only our system user, which never takes the flag -- and the integration's login
    # view answers 409 rather than minting for whatever admin exists. This is the refuse-and-
    # surface branch measured on a real Home Assistant, not on a fake; the control channel's own
    # token is the caller, as the dashboard's would be.
    mint_url = "http://192.168.1.100/api/briard/login"
    mint_body = _sx.quote(_zj.dumps({"client_id": client_id}))
    # A fresh exchange: the restarts above rotated the refresh token, which revoked every access
    # token issued before them ([V3b.29] §6) -- `access` from the first claim is dead by now.
    system = exchange(node1.succeed("cat /run/briard/hass/token").strip())
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

    # THE USER STEP RUNS THROUGH THE DASHBOARD ([V3b.31b]), the way the household's does. The
    # host's one-time code is written here as the `dashboard.handoff` verb writes it (no host
    # agent drives this rig; the verb has its own test), redeemed under the node's OWN name at the
    # door -- which forwards every name it does not route to the dashboard -- and then "Open Home
    # Assistant" does the user step and sends the browser on with the code.
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
    # Trusted: the page offers the first open -- the button is gated on RUNNING, not on HTTP.
    node1.wait_until_succeeds(
        f"curl -fsS -H 'Host: {node_name}' -H 'Cookie: {cookie}' http://192.168.1.100/ | grep -q 'Open Home Assistant'",
        timeout=120,
    )
    # THE BUTTON: a 303 to HA's own onboarding page, under HA's name, carrying the code and the
    # state HA's frontend checks -- hassUrl without the trailing slash, clientId with it.
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
    # The starting password is kept on the volume and shown -- never invented-and-hidden.
    node1.succeed("test -s /var/lib/briard/dashboard/home-assistant-password")
    password = node1.succeed("cat /var/lib/briard/dashboard/home-assistant-password").strip()
    node1.succeed(f"curl -fsS -H 'Host: {node_name}' -H 'Cookie: {cookie}' http://192.168.1.100/ | grep -qF {_sx.quote(password)}")

    # THE OWNER FLAG, read from the store on the volume rather than asked of HA: the household's
    # user holds it, alone, and our system user is still system_generated and not owner.
    # (The auth store saves on a short delay; wait for the flag to land, then parse.)
    auth_store = f"{dataroot}/app/.storage/auth"
    node1.wait_until_succeeds(f"grep -q '\"is_owner\": true' {auth_store}", timeout=30)
    users = _zj.loads(node1.succeed(f"cat {auth_store}"))["data"]["users"]
    owners = [u for u in users if u.get("is_owner")]
    assert [u["name"] for u in owners] == ["Kostas"], f"owners: {owners}"
    ours = [u for u in users if u.get("system_generated")]
    assert ours and not any(u.get("is_owner") for u in ours), f"the system user took ownership: {ours}"

    # THE RESUME, as getAuth does it: the code exchanges for tokens with the same client_id.
    # This is what `onboarding.html?auth_callback=1&code=…&state=…` does on arrival.
    tokens = _zj.loads(door("/auth/token", form={
        "grant_type": "authorization_code", "code": first["auth_code"], "client_id": client_id,
    }))
    human = tokens["access_token"]
    assert tokens.get("refresh_token"), f"the exchange returned no refresh token: {tokens}"
    # ...and the page the browser lands on is served while onboarding is in progress.
    door("/onboarding.html")

    # ANALYTICS WAS MARKED DONE BY THE DASHBOARD, out of order and with the CONTROL CHANNEL's
    # token before the browser was sent on: preferences untouched means off, and the frontend
    # resumes at the first UNDONE step (`_curStep`), so this is the page it skips. The browser's
    # code was not spent on it -- it exchanged above.
    assert steps_done() == {"user": True, "core_config": False, "analytics": True, "integration": False}, steps_done()

    # CORE CONFIG -- the step HA's location page ends on. Marking it is what starts HA's four
    # default integrations. Two of them create their entry unconditionally and prove the step
    # ran (their setup wants the internet this VM does not have; that is not the claim). The
    # third, `met`, is THE MEASUREMENT: its onboarding flow aborts `no_home` while the home
    # coordinates are unset or still HA's own default (met/config_flow.py, 2026.7.1) -- which they
    # are here, because nobody ran the location page first. MEASURED 2026-09-04: the first run of
    # this claim waited 120s for a `met` entry that was never going to come. That absence is the
    # cost of completing core_config by API instead of handing the browser to HA's location page,
    # and it is why the install does the latter ([V3b.31a](d)). The negative is not vacuous: all
    # three flows start in the same loop and `met`'s abort has no await before it, so once both
    # unconditional entries exist the `met` flow has run and chosen.
    door("/api/onboarding/core_config", bearer=human, post={})
    for domain in ("radio_browser", "google_translate"):
        node1.wait_until_succeeds(
            f"curl -fsS -H 'Host: {host}' -H 'Authorization: Bearer {human}' "
            f"'http://192.168.1.100/api/config/config_entries/entry?domain={domain}' | grep -q entry_id",
            timeout=120,
        )
    met = _zj.loads(door("/api/config/config_entries/entry?domain=met", bearer=human))
    assert met == [], f"met set itself up without a home location: {met}"

    # THE INTEGRATION STEP hands back a FRESH code for the final login. ⚠️ Called exactly once,
    # with the HUMAN token and a same-origin redirect: the view marks itself done BEFORE it
    # validates either (views.py, 2026.7.1), so a wrong call burns the step with no code.
    final = _zj.loads(door("/api/onboarding/integration", bearer=human, post={
        "client_id": client_id, "redirect_uri": origin + "/?auth_callback=1",
    }))
    assert final.get("auth_code") and final["auth_code"] != first["auth_code"], f"no fresh code: {final}"
    landed = _zj.loads(door("/auth/token", form={
        "grant_type": "authorization_code", "code": final["auth_code"], "client_id": client_id,
    }))
    assert landed.get("refresh_token"), f"the final code did not exchange: {landed}"

    # ONBOARDED. Every step done, and the user step refuses forever after -- the 403 that makes
    # "fresh HA only" true for the first-open path.
    assert all(steps_done().values()), steps_done()
    node1.fail(
        f"curl -fsS -X POST -H 'Host: {host}' -H 'Content-Type: application/json' "
        f"-d {_sx.quote(_zj.dumps({'name': 'x', 'username': 'x', 'password': 'x', 'client_id': client_id, 'language': 'en'}))} "
        "http://192.168.1.100/api/onboarding/users"
    )
    print("onboarded by API: " + _zj.dumps(steps_done()))

    # THE LATER OPEN ([V3b.31d]): on a set-up Home Assistant the button MINTS a login for the
    # owner through the integration and lands the browser on HA's own auth callback -- the front
    # page, auth_callback=1, the same state as the resume, storeToken so it outlives the tab.
    def owner_tokens():
        """HA's refresh tokens for the browser's client_id, and the owner they must belong to."""
        store = _zj.loads(node1.succeed(f"cat {auth_store}"))["data"]
        owner = [u["id"] for u in store["users"] if u.get("is_owner")][0]
        return owner, [t for t in store["refresh_tokens"] if t.get("client_id") == client_id]
    # The baseline is WAITED FOR: two codes exchanged above (`first`, `final`) mean two tokens for
    # this client_id, and the store lands them on a debounce ((f)8) -- read too early it held
    # one, and the later open then looked like two.
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
    # The code exchanges, and what it logs in as is THE OWNER: HA's own refresh-token store names
    # the user each token belongs to, and the new one belongs to the household's owner, not to
    # our system user or anyone else.
    landed_later = _zj.loads(door("/auth/token", form={
        "grant_type": "authorization_code", "code": minted, "client_id": client_id,
    }))
    assert landed_later.get("refresh_token"), f"the minted code did not exchange: {landed_later}"
    # (The store saves on a short delay; the earlier codes' tokens carry the same client_id, so
    # the evidence is one MORE token, and every one of them the owner's.)
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
  '';
}
