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
# front door, and that /healthz forwarded the question to HA. Both came from the build-time payload
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
  '';
}
