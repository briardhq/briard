# The forced-failure half of hass-upgrade.nix (which proves a real recorder migration succeeds
# with its data intact): when the upgrade breaks something that worked, the health gate
# (agent/guest/entrygate) trips, and the pre-upgrade {code + data} snapshot is a valid rollback
# point that still holds the recorder history, the safety the whole update strategy rests on.
#
# Only real HA proves it: a real entry regression produces a real `migration_error`, the real
# gate returns Rollback on the real config-entry states of HA, and a real recorder rollback keeps
# named history. The break is authored deterministically: the briard_canary custom integration
# (nixosTest/fixtures/briard_canary) loads cleanly on 2025.11.0 and fails its config-entry
# migration on 2025.12.0 (a version-conditional ConfigFlow.VERSION plus a refusing
# async_migrate_entry), so the HA state machine marks its entry `migration_error`. The canary also
# dumps the live state of every entry to a file, which the test waits on without authenticating;
# the pre/post samples come from the product sampler (entrygate-eval -sample), and the real gate
# judges them, asserting VERDICT=rollback.
#
# Harness scope matches hass-upgrade.nix: single-node lib.nix, primitives driven directly. The
# live restore (the host agent install undo, agent/host/app.go, unit-tested there and run end to
# end on the dummy by agent-install-undo.nix) and the ReadinessAssessor seam (unit-tested in
# agent/guest) need a host on virtio-serial, which a lib.nix node lacks; see the rollback section.
{ pkgs, guestModule, entrygateEval, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # HA's address and readiness path come from its fixture's manifest, never restated here.
  haVip = "http://192.168.1.100:${toString fixture.port}";
  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  canary = ./fixtures/briard_canary;
  # The renderer layout, never restated here: the subvolume is the app's (what a rollback point
  # snapshots), and HA writes into its container subdirectory inside it, bind-mounted as /config
  # (the build-time slot used to choose both).
  subvol = "/var/lib/briard/data/${fixture.name}";
  haDir = "${subvol}/${fixture.dataDir}";
  db = "${haDir}/home-assistant_v2.db";
  cfg = "${haDir}/configuration.yaml";
  entriesFile = "${haDir}/briard_entry_states.json";
  snap = "/var/lib/briard/data/.snapshots/${fixture.name}-preupgrade";
  schemaQ = "SELECT COALESCE(MAX(schema_version),0) FROM schema_changes;";
  # A specific named recorder row that must survive into the rollback point, asserted by id and
  # exact content rather than a count (non-vacuous). sun.sun is always present via default_config
  # and records an initial state on boot.
  namedRowCountQ = "SELECT COUNT(*) FROM states s JOIN states_meta m ON s.metadata_id = m.metadata_id WHERE m.entity_id = 'sun.sun';";
  namedRowPickQ = "SELECT s.state_id || '|' || m.entity_id || '|' || COALESCE(s.state,'') FROM states s JOIN states_meta m ON s.metadata_id = m.metadata_id WHERE m.entity_id = 'sun.sun' ORDER BY s.state_id DESC LIMIT 1;";
  # Same projection, selected by state_id — to re-read one specific row in the rollback point.
  # (Kept in the let block so its embedded '' quotes don't collide with the '' testScript string.)
  namedRowByIdPrefixQ = "SELECT s.state_id || '|' || m.entity_id || '|' || COALESCE(s.state,'') FROM states s JOIN states_meta m ON s.metadata_id = m.metadata_id WHERE s.state_id = ";
in
pkgs.testers.runNixOSTest {
  name = "hass-upgrade-rollback";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      # 3072: the GB over the 2048 hass-failover and hass-payload take buys headroom, not speed (a
      # guest grows page cache into what it is given: 4096 measured 4192 MB resident, 3072
      # measures 3167, in the same time within the noise). The recorder schema migration here is
      # the one workload whose peak could grow with a future HA, so it keeps a margin those two
      # do not need.
      virtualisation.memorySize = 3072;
      virtualisation.diskSize = 20480;
      environment.systemPackages = [ pkgs.sqlite pkgs.btrfs-progs pkgs.jq entrygateEval ];
    };

  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}
    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=1200)
    node1.succeed("modprobe drbd")
    node1.succeed("briard-test-storage --seed")
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-primary-storage.service", timeout=120)
    node1.succeed("mountpoint -q /var/lib/briard")

    node1.succeed('podman image exists "$(cat /run/briard/fixture/ref)"')
    node1.succeed('podman image exists "$(cat /run/briard/fixture/variants/to/ref)"')
    # The install: HA `from`, onto the volume this node holds.
    install_fixture(node1)

    # HA (`from`) boots and serves; the recorder initializes + commits real history, and
    # HA writes its default configuration.yaml.
    node1.wait_until_succeeds("curl -fsS -o /dev/null ${haVip}${fixture.healthPath}", timeout=360)
    node1.wait_until_succeeds("test -f ${db}", timeout=60)
    node1.wait_until_succeeds("test $(sqlite3 ${db} 'SELECT COUNT(*) FROM states') -gt 0", timeout=180)
    node1.wait_until_succeeds("test -f ${cfg}", timeout=60)

    # ---- Inject the canary + load it via YAML, then restart `from` so it takes effect ----
    node1.succeed("mkdir -p ${haDir}/custom_components")
    node1.succeed("cp -r ${canary} ${haDir}/custom_components/briard_canary")
    node1.succeed("chmod -R u+w ${haDir}/custom_components/briard_canary")
    node1.succeed("grep -q '^briard_canary:' ${cfg} || echo 'briard_canary:' >> ${cfg}")
    node1.succeed("sync")
    # Bounce HA onto the new config as converge does: stop the container, then start the units in
    # renderer order (pod first), each its own transaction -- agent/guestagent/converge.go stopApp
    # (less its pod stop) plus start loop, by hand: the path the product takes, not a
    # `systemctl restart` it never issues.
    units = fixture_units(node1)
    node1.succeed(f"systemctl stop {units[-1]}")
    for u in units:
        node1.succeed(f"systemctl start {u}")
    node1.wait_until_succeeds("curl -fsS -o /dev/null ${haVip}${fixture.healthPath}", timeout=360)

    # ---- Baseline: canary loaded, schema 51, named history present (while `from` is live) ----
    # Wait for the periodic canary dump to report its own entry loaded (jq, so a loaded
    # default_config entry can't be mistaken for the canary settling).
    canary_is = lambda st: f"jq -e '.[]|select(.domain==\"briard_canary\" and .state==\"{st}\")' ${entriesFile} >/dev/null"
    node1.wait_until_succeeds(canary_is("loaded"), timeout=240)
    # The sample the gate judges comes from the product, not the fixture: the canary dump is only
    # what we wait on (the cheapest sign the entry settled), and a verdict computed from it would
    # prove the fixture can write a file. This goes through agent/hass: read the control token off
    # tmpfs, exchange it for an access token, present the Bearer, trim the answer to the triple
    # the gate reasons over.
    node1.succeed("entrygate-eval -sample ${toString fixture.port} > /tmp/pre.json")
    print("pre-upgrade entry states: " + node1.succeed("cat /tmp/pre.json"))
    # Non-vacuity: a sampler that returned nothing would make every verdict below Pass by
    # construction, and the test would go green having proved the opposite of its subject.
    node1.succeed("jq -e 'length > 0' /tmp/pre.json >/dev/null")
    node1.succeed("jq -e '.[]|select(.domain==\"briard_canary\" and .state==\"loaded\")' /tmp/pre.json >/dev/null")

    pre_schema = node1.succeed("sqlite3 ${db} '${schemaQ}'").strip()
    pre_states = int(node1.succeed("sqlite3 ${db} 'SELECT COUNT(*) FROM states'").strip())
    pre_named = int(node1.succeed("sqlite3 ${db} \"${namedRowCountQ}\"").strip())
    # A specific named row, captured by identity — asserted byte-identical in the rollback point below.
    pre_row = node1.succeed("sqlite3 ${db} \"${namedRowPickQ}\"").strip()
    pre_sid = pre_row.split("|")[0]
    assert pre_schema == "51", f"baseline schema = {pre_schema}, want 51"
    assert pre_states > 0, f"no recorder history to preserve (states={pre_states})"
    assert pre_named > 0 and pre_sid.isdigit(), f"no named sun.sun row at baseline (row={pre_row!r}) — assertion would be vacuous"
    print(f"baseline: schema={pre_schema} states={pre_states} named_row={pre_row!r}")

    # ---- Snapshot the rollback point, then pin `to` + cycle onto it in place ----
    node1.succeed("findmnt /var/lib/briard")
    node1.succeed("mkdir -p /var/lib/briard/data/.snapshots")
    node1.succeed("btrfs subvolume snapshot -r ${subvol} ${snap}")

    # The upgrade: the `to` manifest under the same app name, applied by the product's own
    # converge, which bounces the container onto the new digest.
    install_fixture(node1, variant="to")

    # ---- Post-upgrade: HA serves, the recorder migrates, the canary regresses ----
    node1.wait_until_succeeds("curl -fsS -o /dev/null ${haVip}${fixture.healthPath}", timeout=360)
    node1.wait_until_succeeds("test $(sqlite3 ${db} '${schemaQ}') -eq 53", timeout=300)
    # The canary's entry lands migration_error (HA ran async_migrate_entry, which refused).
    node1.wait_until_succeeds(canary_is("migration_error"), timeout=300)
    # Sampled through the product again, and the token still answers -- across a container bounce
    # onto a different image digest, a boundary at which the wrapper re-mints the token
    # (agent/hass/ensure-token.py, run from the s6 run script at every container start).
    node1.succeed("entrygate-eval -sample ${toString fixture.port} > /tmp/post.json")
    print("post-upgrade entry states: " + node1.succeed("cat /tmp/post.json"))
    # The product's sampler and HA's own computed state agree about the regression. If these ever
    # disagreed, every verdict below would be judging something other than what HA thinks.
    node1.succeed("jq -e '.[]|select(.domain==\"briard_canary\" and .state==\"migration_error\")' /tmp/post.json >/dev/null")

    # ---- The gate must trip: the real verdict over the real states, sampled the real way ----
    verdict = node1.succeed("entrygate-eval /tmp/pre.json /tmp/post.json")
    print("gate: " + verdict.strip())
    assert "VERDICT=rollback" in verdict, f"health-gate did not trip on the regression: {verdict!r}"
    # The floor it sits above stayed happy throughout, the entire reason this layer exists: a gate
    # that only fired when the app was also down would be the floor again.
    node1.succeed("curl -fsS -o /dev/null ${haVip}${fixture.healthPath}")

    # ---- Rollback-with-history-intact: the {code+data} snapshot is a valid rollback point ----
    # The live restore (stop app → data.restore swaps the subvolume back → the `from` manifest →
    # converge → re-serve) is the install undo, driven over the guest control channel, which can't
    # run here: the guest agent serves it over virtio-serial only, and a lib.nix node has no host.
    # agent-install-undo.nix runs it end to end with the real agent on the dummy (broken upgrade →
    # {code+data} rollback → healthy re-serve). What this test uniquely proves, needing real HA,
    # is the two halves that rig can't: the gate trips on a real entry regression (above), and the
    # rollback point holds the pre-migration DB with its history intact (here).
    node1.succeed("btrfs subvolume show ${snap}")
    # WAL-replay copy: the snapshot was taken while HA-from was live, so recent commits sit in the
    # -wal, not the main file — copy the set + open normally (sqlite replays it), exactly as HA-from
    # would when it reopens this snapshot on a rollback. This also proves it's a recoverable point.
    node1.succeed("mkdir -p /tmp/rb && cp -a ${snap}/${fixture.dataDir}/home-assistant_v2.db* /tmp/rb/")
    rbdb = "/tmp/rb/home-assistant_v2.db"
    rb_schema = node1.succeed(f"sqlite3 {rbdb} '${schemaQ}'").strip()
    rb_states = int(node1.succeed(f"sqlite3 {rbdb} 'SELECT COUNT(*) FROM states'").strip())
    # The exact pre-upgrade named row, by id, is readable and unchanged in the rollback point.
    rb_query = "${namedRowByIdPrefixQ}" + pre_sid
    rb_row = node1.succeed("sqlite3 " + rbdb + " \"" + rb_query + "\"").strip()
    assert rb_schema == "51", f"rollback point schema = {rb_schema}, want 51 (schema not reverted)"
    assert rb_states >= pre_states, f"rollback point lost history: {pre_states} -> {rb_states}"
    assert rb_row == pre_row, f"named pre-upgrade row not intact in rollback point: {pre_row!r} -> {rb_row!r}"
    # The pre-upgrade sample (taken live, through the product's own sampler) shows the canary was
    # loaded — restoring this point returns HA to that working config (schema 51, entry v1).
    node1.succeed("jq -e '.[]|select(.domain==\"briard_canary\" and .state==\"loaded\")' /tmp/pre.json >/dev/null")
    print(f"rollback point valid: schema {rb_schema} (reverted from 53), states={rb_states} (>= pre {pre_states}), named row intact: {rb_row!r}")
  '';
}
