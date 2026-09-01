# The forced-failure path + rollback-with-history-intact.
#
# The sibling (hass-upgrade.nix) proves a REAL recorder migration *succeeds* with
# its data intact. This proves the other half: when the upgrade breaks something that
# worked before, the health-gate (agent/guest/entrygate) TRIPS, and the
# pre-upgrade {code + data} snapshot is a valid rollback point that still holds the
# recorder history — the safety the whole update strategy rests on.
#
# What this proves that nothing else can (it needs REAL HA): (1) a real HA entry
# regression produces a real `migration_error`, and (2) the real gate returns Rollback on
# HA's real config-entry states. The live swap-and-reserve under the maintenance bracket
# is Manager orchestration, unit-tested in agent/guest (managed-upgrade.nix, which proved it end
# to end on the dummy, was retired; the lab rollback demo rebuilds that proof) — see
# the rollback section for why it can't run in a lib.nix rig.
#
# The break is a REAL regressed migration, authored deterministically: the briard_canary
# custom integration (nixosTest/fixtures/briard_canary) loads cleanly on 2025.11.0 and
# fails its config-entry migration on 2025.12.0 (a version-conditional ConfigFlow.VERSION
# + a refusing async_migrate_entry), so HA's own state machine marks its entry
# `migration_error`. The canary also dumps every entry's live state to a file, so the
# test reads HA's real verdict without authenticating — then feeds the pre/post samples
# to the real gate via entrygate-eval, asserting VERDICT=rollback.
#
# Harness scope matches hass-upgrade.nix: single-node lib.nix, primitives driven directly
# (the Manager's maintenance bracket + the ReadinessAssessor seam are unit-tested in
# agent/guest — the guest agent is virtio-serial-only, so the real Manager can't run in a
# lib.nix node). What is NEW here: the gate trips on a real HA entry regression, and a
# real recorder rollback preserves named history.
{ pkgs, guestModule, entrygateEval, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  canary = ./fixtures/briard_canary;
  # The RENDERER's layout, never restated here: the SUBVOLUME is the service's (what a rollback
  # point snapshots), and HA writes into its container's subdirectory inside it, bind-mounted as
  # /config ([V3b.3](e2) -- the build-time slot used to choose both).
  subvol = "/var/lib/briard/${fixture.name}";
  haDir = "${subvol}/${fixture.container}";
  db = "${haDir}/home-assistant_v2.db";
  cfg = "${haDir}/configuration.yaml";
  entriesFile = "${haDir}/briard_entry_states.json";
  snap = "/var/lib/briard/.snapshots/${fixture.name}-preupgrade";
  schemaQ = "SELECT COALESCE(MAX(schema_version),0) FROM schema_changes;";
  # A specific named recorder row that must survive into the rollback point (non-vacuous —
  # we assert the exact row content by id, not just a count; see
  # verification-assertions-must-fail). sun.sun is always present via default_config and
  # records an initial state on boot.
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
      virtualisation.memorySize = 4096;
      virtualisation.diskSize = 20480;
      environment.systemPackages = [ pkgs.sqlite pkgs.btrfs-progs pkgs.jq entrygateEval ];
    };

  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}
    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=1200) # both 2.4 GB images
    node1.succeed("modprobe drbd")
    node1.succeed("drbdadm create-md --force r0")
    node1.succeed("systemctl start drbd@r0.target")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    # Arm the one-time format the way BRING-UP does ([B.126]): the product no longer formats on
    # the promotion path, so a harness that seeds a resource by hand leaves the same marker.
    node1.succeed("mkdir -p /run/briard && touch /run/briard/data.format")
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-data.service", timeout=120)
    node1.succeed("mountpoint -q /var/lib/briard")

    node1.succeed('podman image exists "$(cat /run/briard/fixture/ref)"')
    node1.succeed('podman image exists "$(cat /run/briard/fixture/variants/to/ref)"')
    # The install: HA `from`, onto the volume this node holds.
    install_fixture(node1)

    # HA (`from`) boots and serves; the recorder initializes + commits real history, and
    # HA writes its default configuration.yaml.
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json", timeout=360)
    node1.wait_until_succeeds("test -f ${db}", timeout=60)
    node1.wait_until_succeeds("test $(sqlite3 ${db} 'SELECT COUNT(*) FROM states') -gt 0", timeout=180)
    node1.wait_until_succeeds("test -f ${cfg}", timeout=60)

    # ---- Inject the canary + load it via YAML, then restart `from` so it takes effect ----
    node1.succeed("mkdir -p ${haDir}/custom_components")
    node1.succeed("cp -r ${canary} ${haDir}/custom_components/briard_canary")
    node1.succeed("chmod -R u+w ${haDir}/custom_components/briard_canary")
    node1.succeed("grep -q '^briard_canary:' ${cfg} || echo 'briard_canary:' >> ${cfg}")
    node1.succeed("sync")
    # Bounce HA onto the new config the way CONVERGE does, and not with `systemctl restart`:
    # a quadlet container is BoundTo its pod, and stopping the last member makes podman stop the
    # pod -- so a single restart job races its own dependency down and the start half dies with
    # "Bound to unit …-pod.service, but unit isn't active" (measured). Stop the container, then
    # start the units in the renderer's order (pod first), each its own transaction. That IS
    # agent/guestagent/converge.go's stopService + start loop, by hand.
    units = fixture_units(node1)
    node1.succeed(f"systemctl stop {units[-1]}")
    for u in units:
        node1.succeed(f"systemctl start {u}")
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json", timeout=360)

    # ---- Baseline: canary loaded, schema 51, named history present (while `from` LIVE) ----
    # Wait for the canary's periodic dump to report ITS entry as loaded (jq, so a loaded
    # default_config entry can't be mistaken for the canary settling).
    canary_is = lambda st: f"jq -e '.[]|select(.domain==\"briard_canary\" and .state==\"{st}\")' ${entriesFile} >/dev/null"
    node1.wait_until_succeeds(canary_is("loaded"), timeout=240)
    # THE SAMPLE THE GATE JUDGES COMES FROM THE PRODUCT, not from the fixture ([V3b.29](b)).
    # The canary's dump is still what we WAIT on -- it is the cheapest way to know the entry has
    # settled -- but computing the verdict from it would prove the fixture can write a file. This
    # goes through agent/hass: read the control token off tmpfs, exchange it for an access token,
    # present the Bearer, and trim HA's answer to the triple the gate reasons over.
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

    # ---- Snapshot the rollback point, then pin `to` + cycle onto it IN PLACE ----
    node1.succeed("findmnt /var/lib/briard")
    node1.succeed("mkdir -p /var/lib/briard/.snapshots")
    node1.succeed("btrfs subvolume snapshot -r ${subvol} ${snap}")

    # THE UPGRADE: the `to` manifest under the same service name, applied by the product's own
    # converge, which bounces the container onto the new digest ([V3b.3](e2)).
    install_fixture(node1, variant="to")

    # ---- Post-upgrade: HA serves, the recorder migrates, the canary regresses ----
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json", timeout=360)
    node1.wait_until_succeeds("test $(sqlite3 ${db} '${schemaQ}') -eq 53", timeout=300)
    # The canary's entry lands migration_error (HA ran async_migrate_entry, which refused).
    node1.wait_until_succeeds(canary_is("migration_error"), timeout=300)
    # Sampled through the product again, and the token still answers -- across a container bounce
    # onto a different image digest, which is the boundary (a)'s wrapper re-mints at.
    node1.succeed("entrygate-eval -sample ${toString fixture.port} > /tmp/post.json")
    print("post-upgrade entry states: " + node1.succeed("cat /tmp/post.json"))
    # The product's sampler and HA's own computed state agree about the regression. If these ever
    # disagreed, every verdict below would be judging something other than what HA thinks.
    node1.succeed("jq -e '.[]|select(.domain==\"briard_canary\" and .state==\"migration_error\")' /tmp/post.json >/dev/null")

    # ---- The gate MUST trip: the real verdict over the real states, sampled the real way ----
    verdict = node1.succeed("entrygate-eval /tmp/pre.json /tmp/post.json")
    print("gate: " + verdict.strip())
    assert "VERDICT=rollback" in verdict, f"health-gate did not trip on the regression: {verdict!r}"
    # And the floor it sits above was HAPPY throughout -- which is the entire reason this layer
    # exists. A gate that only fired when the service was also down would be the floor again.
    node1.succeed("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json")

    # ---- Rollback-with-history-intact: the {code+data} snapshot is a valid rollback point ----
    # The live swap-and-reserve (stop service → restore subvolume → re-pin `from` → re-serve)
    # is Manager orchestration under the maintenance bracket, and can't run in THIS rig: the
    # bracket (drbd-reactor pause-defused) lives in the guest agent's reactor.pause verb,
    # which is virtio-serial-only — so a bare `systemctl stop <service>` here makes the running
    # promoter demote + unmount the volume (exactly the hazard the bracket exists to avoid).
    # The real Manager doing the full swap + re-serve is unit-tested in agent/guest; the nested
    # end-to-end proof (managed-upgrade.nix) was retired and is the lab demo's to rebuild
    # (broken upgrade → gate trips → {code+data} rollback → healthy re-serve; see the service-install tests0). What THIS
    # test uniquely proves — needing REAL HA — is the two halves that rig can't: the gate trips on a
    # REAL entry regression (above), and the rollback point holds the pre-migration DB with its
    # history intact (here).
    node1.succeed("btrfs subvolume show ${snap}")
    # WAL-replay copy: the snapshot was taken while HA-from was live, so recent commits sit in the
    # -wal, not the main file — copy the set + open normally (sqlite replays it), exactly as HA-from
    # would when it reopens this snapshot on a rollback. This also proves it's a recoverable point.
    node1.succeed("mkdir -p /tmp/rb && cp -a ${snap}/${fixture.container}/home-assistant_v2.db* /tmp/rb/")
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
