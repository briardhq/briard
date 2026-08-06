# Home Assistant rides the payload slot, data on the DRBD volume.
#
# The same promoter chain drbd-promote proves for the dummy, now with the real
# HA image (digest-pinned) in the slot: bring r0 up, let drbd-reactor
# promote and start the ordered unit {promote → data mount → payload → VIP}, then
# assert HA actually boots and serves at the VIP — and that its recorder SQLite +
# `.storage` landed on the DRBD btrfs subvolume. Single node:
# this validates the payload wiring, not failover (that is ha-failover), and HA is heavy
# enough that one instance is the right cost.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
in
pkgs.testers.runNixOSTest {
  name = "ha-payload";

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
    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.succeed("modprobe drbd")
    node1.succeed("drbdadm create-md --force r0")
    node1.succeed("systemctl start drbd@r0.target")
    # Single node: no peer to connect to — just make it UpToDate so it's promotable.
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")

    # Hand off to the promoter: it has quorum alone, promotes, and runs the
    # ordered unit — including `podman load` of the HA image and HA's own boot.
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-data.service", timeout=120)

    # The DRBD device is mounted; HA's /config subvolume lives on it. (Assert the
    # real mount, not the subvolume path — a fresh btrfs subvolume isn't reliably
    # seen as a mountpoint until traversed; the data-file checks below confirm
    # HA's state actually lands on the subvolume.)
    node1.succeed("mountpoint -q /var/lib/briard")

    # HA serves at the VIP. /manifest.json is static + unauthenticated, so a 200
    # means HA's HTTP stack is up — the readiness signal the health-gate will use.
    # Generous timeout: first boot initializes the recorder DB + onboarding store.
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json", timeout=300)

    # And HA is reachable THROUGH the front door, which is the address a user is given.
    # This is the assertion that would have caught the bug the slot's port option fixed — the
    # proxy's backend was hardcoded to :8080, the *fixture's* port, so on an HA guest it pointed
    # at nothing and the front door only ever really served the dummy.
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://192.168.1.100/manifest.json", timeout=60)
    # ...and the node's health tracks HA, because /healthz forwards the question to it.
    health = node1.succeed("curl -fsS http://192.168.1.100/healthz")
    assert "service healthy" in health, f"/healthz with HA installed said: {health!r}"

    # The claim: HA's state landed on the DRBD subvolume — the recorder SQLite
    # and the `.storage` config, so they replicate + snapshot as one unit.
    node1.wait_until_succeeds("test -f /var/lib/briard/ha/home-assistant_v2.db", timeout=60)
    node1.succeed("test -d /var/lib/briard/ha/.storage")
    node1.succeed("test -f /var/lib/briard/ha/configuration.yaml")
  '';
}
