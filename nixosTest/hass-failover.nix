# Kill the primary, HA fails over with config intact at the same VIP.
#
# drbd-failover proves the mechanism (3-node majority, kill-primary → VIP + data move) on the
# deterministic dummy, whose tick counter stands in for "committed data crossed the DRBD link".
# This proves the differentiated claim on the real app: HA state survives a takeover. The owner
# user, created through the (no-auth) onboarding API, lands in the sacred `.storage/auth*`; after
# the kill it must still be there on the promoted survivor -- config HA durably wrote, replicated
# under protocol C and re-adopted.
#
# Heavy (two HA cold starts + the 2.4 GB image), so it lives in the `ha` tag.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # HA's address and readiness path come from its fixture's manifest, never restated here.
  haVip = "http://192.168.1.100:${toString fixture.port}";
  baseNode = h.mkNode {
    inherit fixture;
    resource = h.mkResource [
      { name = "node1"; id = 0; }
      { name = "node2"; id = 1; }
      { name = "node3"; id = 2; }
    ];
  };
  # Any node can be promoted, so all get the memory and disk HA needs; the 2.4 GB image is
  # resident from its prebuilt store on every node.
  node =
    { ... }:
    {
      imports = [ baseNode ];
      # 2048 per node, measured: each guest grows page cache into what it is given, so at 3072
      # the trio is 9498 MB resident of 9216 declared (by far the heaviest in the tier), at 2048
      # 6426 MB of 6144, in 140s rather than 155s.
      virtualisation.memorySize = 2048;
      virtualisation.diskSize = 10240;
    };
in
pkgs.testers.runNixOSTest {
  name = "hass-failover";

  # crash() is QemuMachine-only and the primary is selected dynamically.
  skipTypeCheck = true;

  nodes = {
    node1 = node;
    node2 = node;
    node3 = node;
  };

  testScript = ''
    ${h.fixtureHelpers}
    USER = "briard-test"
    VIP = "${haVip}"

    machines = [node1, node2, node3]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        # The 2.4 GB image is resident on every node before anything promotes: warm standby makes
        # an offline failover possible (asserted again just before the kill).
        m.wait_for_unit("briard-test-fixture-install.service", timeout=900)
        m.succeed("modprobe drbd")
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")

    # Offline failover, the crux for HA: an outage that triggers failover often kills WAN, so the
    # 2.4 GB image must never be pulled at promotion. A curl to a public IP (1.1.1.1) is a bad
    # control: it can fail for unrelated reasons (an upstream firewall), and the sandbox has no
    # WAN egress anyway, so it passes vacuously. Instead: sever any default route and assert it
    # is gone (a local routing fact), and prove image warmth before the kill. The print documents
    # pre-cut reachability of a known-good host to document the offline sandbox.
    print("pre-cut reach 1.0.0.1:", node1.execute("curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://1.0.0.1 || echo UNREACHABLE")[1].strip())
    for m in machines:
        m.succeed("ip route del default 2>/dev/null || true")
        m.fail("ip route show default | grep -q .")

    # Each node reaches both peers; the initial sync runs for real (awaited before the crash).
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")

    for m in machines:
        m.succeed("systemctl start drbd-reactor.service")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    # The reverse proxy comes up on the elected primary with nothing installed, then HA is
    # installed onto its volume. The survivor never runs an install: it renders HA from the same
    # volume when it promotes, the half this test is about.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=300)
    primary = next(m for m in machines if role(m) == "Primary")
    print(f"primary={primary.name}")
    dataroot = install_fixture(primary)
    node1.wait_until_succeeds(f"curl -fsS -o /dev/null {VIP}${fixture.healthPath}", timeout=300)

    # Real, durable HA config: the owner user, via the no-auth first onboarding step (available
    # as soon as the onboarding API is up), into `.storage/auth*`.
    primary.wait_until_succeeds(f"curl -fsS -o /dev/null {VIP}/api/onboarding", timeout=60)
    primary.succeed(
        "curl -fsS -X POST -H 'Content-Type: application/json' "
        f'-d \'{{"client_id":"{VIP}/","name":"Briard Test","username":"{USER}",'
        '"password":"briard-test-pw","language":"en"}\' '
        f"{VIP}/api/onboarding/users"
    )
    # Once HA persisted it, flush the page cache so the bytes are on the DRBD device (replicated
    # under protocol C) before the abrupt kill.
    primary.wait_until_succeeds(f"grep -rq {USER} {dataroot}/${fixture.dataDir}/.storage")
    primary.succeed("sync")

    # Warm standby, the positive proof (rather than the vacuous "can't reach the internet"): every
    # node, the survivor included, holds the 2.4 GB image in local podman storage (loaded at boot),
    # so a promotion never pulls.
    for m in machines:
        # By the digest the manifest pins, not a tag: that is the ref `Pull=never` resolves, and a
        # tag could match an image the manifest does not name.
        m.succeed('podman image exists "$(cat /run/briard/fixture/ref)"')

    # The crash must find an UpToDate replica. The seed path syncs a joiner for real, so a replica
    # is a SyncTarget, which cannot promote, until the initial resync completes; "replicated" is
    # the claim under test, so the disk state is asserted before the only UpToDate copy goes.
    for m in machines:
        if m != primary:
            m.wait_until_succeeds("drbdadm dstate r0 | grep -qE '^(UpToDate|Diskless)'", timeout=300)
    # Kill the primary abruptly (power-loss shape).
    primary.crash()
    survivors = [m for m in machines if m != primary]

    # The survivors keep quorum, promote one of their own, reload the HA image and boot HA; the
    # VIP answers again at the same address, moved by gratuitous ARP.
    survivors[0].wait_until_succeeds(f"curl -fsS -o /dev/null {VIP}${fixture.healthPath}", timeout=400)

    # Config intact, on whichever survivor is now primary: the owner user crossed the DRBD link
    # and HA there re-adopted the replicated `.storage`.
    new_primary = next(m for m in survivors if role(m) == "Primary")
    new_primary.succeed(f"grep -rq {USER} {dataroot}/${fixture.dataDir}/.storage")

    # HA started inside this promotion (briard-apps is a chain member) and reads its network once,
    # at boot: the interface it advertises and answers mDNS on, and the address of its own URL,
    # are where the kernel sends 224.0.0.251. The chain claims the VIP (briard-vip routes
    # multicast out of the VIP NIC) before the apps start, so this HA saw the household LAN; the
    # other way round it would race the claim and, on losing, advertise the qemu user-net address
    # until its next restart.
    import json as _zj
    advert = _zj.loads(new_primary.succeed(
        "podman exec -i briard-home-assistant-app python3 - < ${h.haAdvertProbe}"
    ))
    print(f"the new primary's HA advertises: {advert}")
    assert "192.168.1.100" in advert["addresses"], f"HA booted at promotion does not advertise the VIP: {advert}"
    assert "10.0.2.15" not in advert["addresses"], f"HA booted at promotion advertises the user-net address: {advert}"

    # The recorder SQLite rode the same volume and HA re-opened it intact, not quarantined as
    # `.corrupt.<ts>` (the silent-history-loss symptom an abrupt crash on a torn DB would
    # trigger): the specific SQLite-on-DRBD risk the spike exists to retire.
    new_primary.succeed(f"test -f {dataroot}/${fixture.dataDir}/home-assistant_v2.db")
    new_primary.fail(f"ls {dataroot}/${fixture.dataDir}/home-assistant_v2.db.corrupt.*")

    # Single-primary preserved: exactly one survivor holds the DRBD primary role.
    primaries = [m.name for m in survivors if role(m) == "Primary"]
    assert len(primaries) == 1, f"expected one primary among survivors, got {primaries}"
  '';
}
