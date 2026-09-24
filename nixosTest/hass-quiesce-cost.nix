# WHAT AN HOURLY SAMPLE COSTS HOME ASSISTANT ([B.167a]) -- a stopwatch with a verdict, run by hand.
#
# The history samples a running Home Assistant by the clock, and every such sample holds the
# recorder still (a truncating WAL checkpoint plus a held transaction, agent/hass/quiesce.go).
# Before the clock moves from nightly to hourly, this measures what that costs on a recorder with
# real history under real write load:
#
#   - the pause: how long getting the lock takes and how long it is held (the product reports
#     both, `acquire_ms` / `hold_ms`), p95 across samples;
#   - whether the lock HOLDS, since a lock Home Assistant breaks under backlog makes the sample
#     crash-consistent -- the anchor quietly becoming `crash` that the item warns about;
#   - whether an automation notices: a probe automation is timed inside and outside samples;
#   - the space an hourly member pins, extrapolated from the churn between two samples.
#
# The history is SYNTHETIC: entities with thousands of states each, written through the REST API
# so the recorder writes them as it would any state, then a writer that keeps going at each rate.
# The work is quiesce-cost.py, which prints a JSON verdict; this file stands Home Assistant up,
# adds the probe automation, and asserts the verdict.
#
# ⚠️ L0 IS NESTED AND CONTENDED, so the numbers are an UPPER bound: a pass here is a pass, and a
# fail needs a run on real hardware before it decides anything. Which is also why it is not in the
# nightly: a verdict that moves with the box's load is not a statement about the product.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  driver = ./quiesce-cost.py;

  # The probe: a state change on sensor.probe increments counter.probe, and the driver times the
  # gap. Appended to the configuration the image wrote, so everything else stays Home Assistant's
  # own default.
  probeConfig = pkgs.writeText "probe.yaml" ''

    counter:
      probe:
    automation probe:
      - triggers:
          - trigger: state
            entity_id: sensor.probe
        actions:
          - action: counter.increment
            target:
              entity_id: counter.probe
  '';

  node = h.mkNode {
    fixtures = [ fixture ];
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
in
pkgs.testers.runNixOSTest {
  name = "hass-quiesce-cost";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      environment.systemPackages = [ pkgs.python3 ];
      # hass-payload's measured 2048 plus room for a recorder that is being written to hard.
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

    # A token that works, which also waits out Home Assistant's own first-boot setup.
    token = node1.succeed("cat /run/briard/home-assistant/token").strip()

    def access():
        return node1.succeed(
            "curl -fsS -X POST http://127.0.0.1:8123/auth/token "
            f"-d grant_type=refresh_token -d refresh_token={token} "
            "| sed 's/.*\"access_token\":\"\\([^\"]*\\)\".*/\\1/'"
        ).strip()

    node1.wait_until_succeeds(
        "curl -fsS -X POST http://127.0.0.1:8123/auth/token "
        f"-d grant_type=refresh_token -d refresh_token={token} | grep -q access_token",
        timeout=300,
    )

    # THE PROBE AUTOMATION, loaded by HOME ASSISTANT'S OWN restart -- the in-process one (exit 100,
    # s6 re-runs it), never `systemctl restart` of the quadlet container, which races its own pod
    # down (hass-upgrade-rollback records the trap). Waiting on counter.probe EXISTING is the proof
    # the new configuration loaded; /manifest.json answers from the old process for a moment.
    node1.succeed(f"cat ${probeConfig} >> {dataroot}/app/configuration.yaml")
    node1.succeed(
        f"curl -fsS -X POST -H 'Authorization: Bearer {access()}' "
        "http://127.0.0.1:8123/api/services/homeassistant/restart"
    )
    node1.wait_until_succeeds(
        "curl -fsS -X POST http://127.0.0.1:8123/auth/token "
        f"-d grant_type=refresh_token -d refresh_token={token} "
        "| sed 's/.*\"access_token\":\"\\([^\"]*\\)\".*/\\1/' "
        "| xargs -I{} curl -fsS -H 'Authorization: Bearer {}' http://127.0.0.1:8123/api/states/counter.probe",
        timeout=300,
    )

    # The briard integration's quiesce view exists only once its setup has run, which is later
    # than the API answering (hass-payload records the same trap).
    node1.wait_until_succeeds(
        f"curl -fsS -o /dev/null -X POST -H 'Authorization: Bearer {access()}' "
        "-H 'Content-Type: application/json' -d '{\"hold\":false}' "
        "http://127.0.0.1:8123/api/briard/quiesce",
        timeout=120,
    )

    # THE MEASUREMENT. Generous: seeding 100k states through REST is minutes on its own.
    out = node1.succeed("python3 ${driver} 2>/tmp/quiesce-cost.log", timeout=3600)
    print(node1.succeed("cat /tmp/quiesce-cost.log | tail -60"))
    verdict = json.loads(out.strip().splitlines()[-1])

    print(f"seeded {verdict['seeded']} states; recorder {verdict['db_bytes'] / 1e6:.0f} MB; limits {verdict['limits']}")
    print("rate  achieved  held   acquire p50/p95/max   hold p50/p95/max   pause p95   probe p95 in/out (n)      MB/h")
    for r in verdict["rates"]:
        a, hd, p = r["acquire_ms"], r["hold_ms"], r["probe_ms"]
        print(
            f"{r['rate']:>4}  {r['achieved_rate']:>8}  {r['held_rate']:>4.0%}  "
            f"{a['p50']:>6}/{a['p95']}/{a['max']} ms   {hd['p50']:>5}/{hd['p95']}/{hd['max']} ms   "
            f"{r['pause_ms_p95']:>6} ms   {p['during_p95']}/{p['outside_p95']} ms ({p['during_n']}/{p['outside_n']})   "
            f"{r['mb_per_hour']}"
        )
        for f in r["failures"]:
            print(f"  FAIL at {r['rate']}/s: {f}")
    assert verdict["pass"], "the hourly sample costs more than the limits allow (see the table above)"
  '';
}
