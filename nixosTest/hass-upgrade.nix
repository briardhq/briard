# A REAL Home Assistant upgrade carrying a REAL recorder schema migration
# through the pipeline, with its data intact (closing a known gap: "the update pipeline
# never processed a real HA upgrade").
#
# HA boots on the `from` image (2025.11.0, recorder schema 51), the recorder writes
# history, then the service is upgraded to `to` (2025.12.0, schema 53) and HA runs the
# real migration on first boot (migrators 52+53). We assert the *outcome*, not just
# liveness: the recorder schema advances 51→53 (the migration signal — HA must detect
# the older schema and record the bump), the pre-upgrade `states` history survives, HA
# re-serves on the migrated DB, and the pre-upgrade btrfs snapshot is kept as a
# still-schema-51 (valid) rollback point.
#
# NB the signal is schema_version, not column shape: HA creates a fresh DB at its code's
# full model shape, so statistics_meta.unit_class (added to *existing* DBs by v52) is
# already present at schema 51 — column presence can't distinguish pre/post. The
# schema_changes ledger is what a real migration moves; the column set is printed only
# as a diagnostic.
#
# Harness scope (in-place, under the running promoter): this drives the core
# service-upgrade PRIMITIVES — snapshot → pin+retag → restart → health-gate — with
# drbd-reactor live throughout, which is also the product's shape: a service install
# never pauses the promoter ([V3b.3](f)); the maintenance bracket belongs to the
# OS-upgrade path (covered by the maintenance-contract test), and the health-gated
# auto-rollback is host-side orchestration (agent/host/service.go, unit-tested there).
# What is NEW and untested until now — a real recorder schema migration surviving the
# upgrade with its data intact — is exactly what this proves. The forced-failure →
# rollback half is hass-upgrade-rollback.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  # Where the RENDERER puts HA's data, never restated: the service's subvolume plus its
  # container's subdirectory, bind-mounted to /config inside.
  # The SUBVOLUME is the service's (what a rollback point snapshots); haDir is the container's
  # subdirectory inside it (what HA writes to, bind-mounted as /config).
  subvol = "/var/lib/briard/${fixture.name}";
  haDir = "${subvol}/${fixture.container}";
  db = "${haDir}/home-assistant_v2.db";
  snap = "/var/lib/briard/.snapshots/${fixture.name}-preupgrade";
  # Current recorder schema = max(schema_version) in HA's schema_changes ledger. This
  # is the migration signal: HA-to must detect the older schema, run migrators 52+53,
  # and record the advance. (Column-shape deltas are NOT a usable signal — HA creates a
  # fresh DB at its code's full model shape, so e.g. statistics_meta.unit_class is
  # already present at schema 51; the version ledger is what a real migration moves.)
  schemaQ = "SELECT COALESCE(MAX(schema_version),0) FROM schema_changes;";
  # Diagnostic only: the statistics_meta column set, printed pre/post to see what (if
  # anything) the migration reshaped on SQLite.
  colsQ = "SELECT group_concat(name) FROM pragma_table_info('statistics_meta');";
in
pkgs.testers.runNixOSTest {
  name = "hass-upgrade";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      # Two 2.4 GB HA images resident (from serving + to staged) + HA's live Python
      # stack + the migration: give it real memory and disk headroom.
      virtualisation.memorySize = 4096;
      virtualisation.diskSize = 20480;
      # Sqlite3 to read the recorder DB + btrfs to take the pre-upgrade snapshot
      # directly (lib.nix nodes ship only curl; the guest services carry their own paths).
      environment.systemPackages = [ pkgs.sqlite pkgs.btrfs-progs ];
    };

  # HA boot is slow and the promoter selects the primary dynamically.
  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}
    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=1200) # both 2.4 GB images
    node1.succeed("modprobe drbd")
    node1.succeed("drbdadm create-md --force r0")
    node1.succeed("systemctl start drbd@r0.target")
    # Single node: no peer — just make it UpToDate so the promoter has quorum-of-1.
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-data.service", timeout=120)
    node1.succeed("mountpoint -q /var/lib/briard")

    # Both ends warm: `from` is what gets installed, `to` is resident and ready to be installed
    # over it — never a pull on the upgrade path. By DIGEST, which is what the manifests pin.
    node1.succeed('podman image exists "$(cat /run/briard/fixture/ref)"')
    node1.succeed('podman image exists "$(cat /run/briard/fixture/variants/to/ref)"')

    # The install: HA `from` onto the volume this node holds.
    install_fixture(node1)

    # HA (`from`) boots and serves. Generous: first boot initializes the recorder DB.
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json", timeout=360)
    node1.wait_until_succeeds("test -f ${db}", timeout=60)
    # Non-vacuous 'data survived': wait for the recorder to commit real history first.
    node1.wait_until_succeeds("test $(sqlite3 ${db} 'SELECT COUNT(*) FROM states') -gt 0", timeout=180)

    # ---- Baseline, read while HA-from is LIVE ----
    # Read against the running HA: WAL allows concurrent readers and the DB keeps its
    # -shm/-wal healthy. (Opening the DB at rest right after the container stops can hit a
    # hot-WAL "unable to open database file"; these values are stable, so a live read is
    # correct and robust.)
    pre_schema = node1.succeed("sqlite3 ${db} '${schemaQ}'").strip()
    pre_cols = node1.succeed("sqlite3 ${db} \"${colsQ}\"").strip()
    pre_states = int(node1.succeed("sqlite3 ${db} 'SELECT COUNT(*) FROM states'").strip())
    assert pre_schema == "51", f"baseline recorder schema = {pre_schema}, want 51 (2025.11.0)"
    assert pre_states > 0, f"no recorder history to preserve (states={pre_states})"
    print(f"baseline: schema={pre_schema} states={pre_states} statistics_meta_cols=[{pre_cols}]")

    # ---- Snapshot the rollback point, then pin `to` + cycle onto it IN PLACE ----
    # We do NOT touch drbd-reactor here, and neither does the product: a service upgrade
    # never pauses the promoter ([V3b.3](f)) — the maintenance bracket belongs to the
    # OS-upgrade path (proven non-destructive by the maintenance contract). This test's
    # job is the migration. A service restart is not a DRBD event, so the running promoter
    # doesn't react; the volume stays mounted throughout.
    node1.succeed("findmnt /var/lib/briard")            # still mounted (promoter untouched)
    node1.succeed("btrfs subvolume show ${subvol}")  # the service's data dir is a real subvolume
    node1.succeed("mkdir -p /var/lib/briard/.snapshots")
    # -r read-only, the exact form the guest agent's data.snapshot verb runs. Taken
    # live: btrfs snapshots atomically (crash-consistent; HA recovers its WAL on open), so it
    # is a valid rollback point without quiescing — the product's install path takes its
    # snapshot live the same way (agent/host/service.go).
    node1.succeed("btrfs subvolume snapshot -r ${subvol} ${snap}") # the {code,data} rollback point

    # THE UPGRADE: install the `to` manifest under the SAME service name. That is what a version
    # change is now ([V3b.3](e2)) -- the volume's manifest moves, and converge re-renders and
    # BOUNCES the container onto the new digest ([V3b.3](e1): starting an already-running unit
    # would have left HA-from serving while every file said otherwise). The bounce cleanly stops
    # HA-from, flushing its DB, before HA-to opens the schema-51 database and migrates it.
    install_fixture(node1, variant="to")

    # ---- Health-gate + migration completion ----
    # HA may answer HTTP while the recorder migrates live in the background, so the real
    # gate is schema==53 (migration DONE), not merely manifest 200.
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json", timeout=360)
    node1.wait_until_succeeds("test $(sqlite3 ${db} '${schemaQ}') -eq 53", timeout=300)

    # ---- Migrated AND kept its data ----
    post_schema = node1.succeed("sqlite3 ${db} '${schemaQ}'").strip()
    post_cols = node1.succeed("sqlite3 ${db} \"${colsQ}\"").strip()
    post_states = int(node1.succeed("sqlite3 ${db} 'SELECT COUNT(*) FROM states'").strip())
    assert post_schema == "53", f"recorder schema = {post_schema} after upgrade, want 53 (migration didn't run)"
    assert post_states >= pre_states, f"recorder history shrank across migration: {pre_states} -> {post_states}"
    print(f"migrated: schema {pre_schema}->{post_schema}, states {pre_states}->{post_states}, HA serving on the migrated DB")
    print(f"statistics_meta cols: pre=[{pre_cols}] post=[{post_cols}]")

    # ---- The snapshot is a VALID rollback point: still the pre-migration DB (schema 51) ----
    node1.succeed("btrfs subvolume show ${snap}")
    # The snapshot was taken live, so HA's WAL-mode DB has recent commits (including the
    # schema_changes rows) in its -wal, not yet checkpointed into the main file — an immutable
    # read (which ignores the WAL) misses them. Copy the DB set to a writable dir and open it
    # normally: sqlite replays the WAL, exactly as HA-from would when it reopens this snapshot
    # on a rollback — so this also proves the snapshot is a recoverable rollback point.
    node1.succeed("mkdir -p /tmp/snapchk && cp -a ${snap}/${fixture.container}/home-assistant_v2.db* /tmp/snapchk/")
    snap_schema = node1.succeed("sqlite3 /tmp/snapchk/home-assistant_v2.db '${schemaQ}'").strip()
    assert snap_schema == "51", f"pre-upgrade snapshot schema = {snap_schema}, want 51 — not a valid rollback point"
    print("pre-upgrade snapshot kept and still schema 51 — a valid rollback point")
  '';
}
