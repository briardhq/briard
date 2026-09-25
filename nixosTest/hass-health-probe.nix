# WHICH SIGNAL SAYS HOME ASSISTANT IS BROKEN? ([B.167b]) -- a discovery probe, run by hand.
#
# The history marks an event red when the app is unhealthy after it, and the design names the
# per-service probe (`/manifest.json`) as the signal while doubting it: Home Assistant's recovery
# mode keeps its frontend serving. This rig does not decide; it breaks Home Assistant one way at a
# time and records what every candidate signal says, so the signal is chosen from evidence:
#
#   - the door, `/manifest.json` (today's healthPath): status and latency;
#   - `/api/config`: `state`, `recovery_mode`, `safe_mode`;
#   - config entries by state (`loaded`, `setup_error`, `setup_retry`, ...);
#   - ERROR lines in `/api/error_log`;
#   - the container unit: active state and restart count.
#
# Each fault starts from the same known-good copy of the data (a btrfs snapshot taken once Home
# Assistant is up), is injected, and is followed by a converge-shaped restart and a 3-minute
# window. The work is health-probe.py; this file stands Home Assistant up, takes the copy, and
# prints the matrix. It asserts only that the baseline is healthy on every signal -- a probe whose
# control is broken measures nothing.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  driver = ./health-probe.py;

  node = h.mkNode {
    fixtures = [ fixture ];
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
in
pkgs.testers.runNixOSTest {
  name = "hass-health-probe";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      environment.systemPackages = [ pkgs.python3 ];
      virtualisation.memorySize = 3072;
      virtualisation.diskSize = 10240;
    };

  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}
    import json

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=600)
    node1.succeed("modprobe drbd")
    node1.succeed("briard-test-storage --seed")
    name_the_flock(node1)
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-primary-storage.service", timeout=120)
    node1.wait_until_succeeds("test -S /run/briard/agent.sock", timeout=60)

    dataroot = install_fixture(node1)
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://127.0.0.1:8123/manifest.json", timeout=300)

    # Up means the briard integration is set up too -- its view exists only then, which is later
    # than the door answering (hass-payload records the trap).
    token = node1.succeed("cat /run/briard/home-assistant/token").strip()
    node1.wait_until_succeeds(
        "curl -fsS -X POST http://127.0.0.1:8123/auth/token "
        f"-d grant_type=refresh_token -d refresh_token={token} "
        "| sed 's/.*\"access_token\":\"\\([^\"]*\\)\".*/\\1/' "
        "| xargs -I{} curl -fsS -o /dev/null -X POST -H 'Authorization: Bearer {}' "
        "-H 'Content-Type: application/json' -d '{\"hold\":false}' http://127.0.0.1:8123/api/briard/quiesce",
        timeout=300,
    )

    # THE KNOWN-GOOD COPY, taken with Home Assistant stopped so every fault starts from the same
    # flushed state. Stopping the container releases the subvolume's bind; the pod stays up.
    units = fixture_units(node1)
    node1.succeed(f"systemctl stop {units[-1]}")
    node1.succeed(f"btrfs subvolume snapshot {dataroot} /var/lib/briard/.probe-pristine")

    out = node1.succeed("python3 ${driver} 2>/tmp/health-probe.log", timeout=5400)
    print(node1.succeed("cat /tmp/health-probe.log"))
    matrix = json.loads(out.strip().splitlines()[-1])

    print("fault                      door 1st200  door%60s  restarts  unit        auth  state     recovery safe  errors  entries")
    for fault, r in matrix.items():
        last = r["last"]
        print(
            f"{fault:<26} {str(r['door_first_200_s']):>9}  {r['door_200_share_last_60s']:>8}  {r['restarts']:>8}  "
            f"{last.get('unit', '?'):<10}  {str(last.get('auth')):<5} {str(last.get('state')):<9} "
            f"{str(last.get('recovery')):<8} {str(last.get('safe')):<5} {str(last.get('errors')):>6}  "
            f"{last.get('entries')}"
        )

    base = matrix["baseline"]["last"]
    assert matrix["baseline"]["door_200_share_last_60s"] == 1 and base.get("auth") and str(base.get("state")).upper() == "RUNNING" \
        and not base.get("recovery"), f"the control is not healthy, so nothing above means anything: {matrix['baseline']}"

    # A CRASHED HOME ASSISTANT IS RESTARTED ([B.168]), the one product claim this probe carries. Under
    # quadlet's default exit policy the pod goes down with its container, systemd stops the unit as
    # its dependent, and Restart=always never fires; the pod's ExitPolicy=continue is what prevents it.
    crash = matrix["custom-crash"]
    assert crash["restarts"] > 0, f"a crashed Home Assistant was not restarted: {crash}"
  '';
}
