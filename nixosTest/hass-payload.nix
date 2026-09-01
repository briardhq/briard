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
      # HA needs real memory + disk headroom: the 2.4 GB image is `podman load`ed
      # onto the writable root, and HA's Python stack wants ~1 GB live.
      virtualisation.memorySize = 3072;
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
  '';
}
