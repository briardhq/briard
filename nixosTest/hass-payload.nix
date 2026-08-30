# Home Assistant INSTALLED AS A SERVICE, data on the DRBD volume.
#
# The same path drbd-promote proves for the dummy, with the real HA image (digest-pinned in a
# signed manifest): bring r0 up, let drbd-reactor promote and start the ordered chain, install HA
# onto the volume it now holds, and assert HA actually boots and serves — and that its recorder
# SQLite + `.storage` landed on the DRBD btrfs subvolume. Single node: this validates the service
# wiring, not failover (that is hass-failover), and HA is heavy enough that one instance is the
# right cost.
#
# ⚠️ WHAT THIS NO LONGER PROVES, and where it went. It used to assert HA was reachable THROUGH the
# front door, and that /healthz forwarded the question to HA. Both came from the build-time service
# slot, which fed the reverse proxy its `-backend` at guest-build time; the slot is deleted
# ([V3b.3](e2)) and routing the front door to a runtime-installed service is [B.48]'s -- the sole
# owner of the routes table. So the door answers for itself here, exactly as it does on every
# shipped node, and the proxying assertion comes back when routing does: [B.48] carries the
# obligation to restore it, because this coverage was deleted rather than superseded.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    inherit fixture;
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
  '';
}
