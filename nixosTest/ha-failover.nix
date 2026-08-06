# Kill the primary, HA fails over with config intact at the same VIP.
#
# drbd-failover proves the mechanism (3-node majority, kill-primary → VIP + data
# move) for the deterministic dummy; this proves the *differentiated claim* on the
# real payload: real HA state survives a takeover. The dummy's tick counter stands
# in for "committed data crossed the DRBD link"; HA's equivalent is its **sacred
# `.storage`** — so we create the owner user through HA's own (no-auth)
# onboarding API, which writes `.storage/auth*`, and after the kill assert that
# account is still there on the promoted survivor. That is config-intact: the
# config HA durably wrote replicated under protocol C and HA re-adopted it.
#
# Heavy (two HA cold starts + the 2.4 GB image), so it lives in the `heavy` tier.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  baseNode = h.mkNode {
    resource = h.mkResource [
      { name = "node1"; id = 0; }
      { name = "node2"; id = 1; }
      { name = "node3"; id = 2; }
    ];
  };
  # Every node is HA-capable (any can be promoted), so all get the memory + disk
  # HA needs — the 2.4 GB image is `podman load`ed on whichever node holds primary.
  node =
    { ... }:
    {
      imports = [ baseNode ];
      virtualisation.memorySize = 3072;
      virtualisation.diskSize = 10240;
    };
in
pkgs.testers.runNixOSTest {
  name = "ha-failover";

  # crash() is QemuMachine-only and the primary is selected dynamically.
  skipTypeCheck = true;

  nodes = {
    node1 = node;
    node2 = node;
    node3 = node;
  };

  testScript = ''
    USER = "briard-test"
    VIP = "http://192.168.1.100:8123"

    machines = [node1, node2, node3]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
        m.succeed("drbdadm create-md --force r0")
        m.succeed("systemctl start drbd@r0.target")

    # Offline failover — the crux for HA: an outage that triggers failover
    # often kills WAN, and the 2.4 GB image must never be pulled at promotion (warm
    # standby). Prove offline *without* probing a public IP: a curl to e.g.
    # 1.1.1.1 is a bad control — it can fail for unrelated reasons (an upstream
    # firewall) and the nixosTest sandbox has no WAN egress anyway, so it passes
    # vacuously. Instead sever any default route and assert it's gone (a local
    # routing fact), and prove image warmth before the kill (below). The diagnostic
    # records pre-cut reachability of a known-good host to document the offline sandbox.
    print("pre-cut reach 1.0.0.1:", node1.execute("curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://1.0.0.1 || echo UNREACHABLE")[1].strip())
    for m in machines:
        m.succeed("ip route del default 2>/dev/null || true")
        m.fail("ip route show default | grep -q .")

    # Each node reaches both peers, then we skip the initial sync once.
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")

    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    # HA converges on the elected primary and serves at the VIP (slow cold start).
    node1.wait_until_succeeds(f"curl -fsS -o /dev/null {VIP}/manifest.json", timeout=300)
    primary = next(m for m in machines if role(m) == "Primary")
    print(f"primary={primary.name}")

    # Write real, durable HA config: the owner user, via the no-auth onboarding
    # step (first step, available as soon as the onboarding API is up). This lands
    # in the sacred `.storage/auth*`.
    primary.wait_until_succeeds(f"curl -fsS -o /dev/null {VIP}/api/onboarding", timeout=60)
    primary.succeed(
        "curl -fsS -X POST -H 'Content-Type: application/json' "
        f'-d \'{{"client_id":"{VIP}/","name":"Briard Test","username":"{USER}",'
        '"password":"briard-test-pw","language":"en"}\' '
        f"{VIP}/api/onboarding/users"
    )
    # HA persisted it; flush the guest page cache so the bytes are on the DRBD
    # device (→ replicated under protocol C) before the abrupt kill.
    primary.wait_until_succeeds(f"grep -rq {USER} /var/lib/briard/ha/.storage")
    primary.succeed("sync")

    # Warm standby is what makes offline failover possible: every node — the
    # survivor included — already has the 2.4 GB image in local podman storage, so a
    # promotion never pulls. Prove it before the kill (the load runs at boot). This
    # is the real positive proof, replacing the vacuous "can't reach the internet".
    for m in machines:
        m.wait_until_succeeds("podman image exists ghcr.io/home-assistant/home-assistant:2026.7.1")

    # Kill the primary abruptly (power-loss shape).
    primary.crash()
    survivors = [m for m in machines if m != primary]

    # The two survivors keep quorum, promote one of their own, reload the HA image
    # and boot HA, and the VIP answers again — same address, moved by gratuitous ARP.
    survivors[0].wait_until_succeeds(f"curl -fsS -o /dev/null {VIP}/manifest.json", timeout=400)

    # Config intact: the owner user crossed the DRBD link and the new primary's HA
    # re-adopted the replicated `.storage`. Assert on whichever survivor is now primary.
    new_primary = next(m for m in survivors if role(m) == "Primary")
    new_primary.succeed(f"grep -rq {USER} /var/lib/briard/ha/.storage")

    # The recorder SQLite rode the same volume and HA re-opened it intact on
    # the survivor — not quarantined as `.corrupt.<ts>` (the silent-history-loss
    # symptom warns of, which an abrupt crash on a torn DB would trigger). This
    # is the specific SQLite-on-DRBD risk the spike exists to retire.
    new_primary.succeed("test -f /var/lib/briard/ha/home-assistant_v2.db")
    new_primary.fail("ls /var/lib/briard/ha/home-assistant_v2.db.corrupt.*")

    # Single-primary preserved: exactly one survivor holds the DRBD primary role.
    primaries = [m.name for m in survivors if role(m) == "Primary"]
    assert len(primaries) == 1, f"expected one primary among survivors, got {primaries}"
  '';
}
