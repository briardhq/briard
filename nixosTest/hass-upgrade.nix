# A real Home Assistant upgrade carrying a real recorder schema migration through the pipeline,
# with its data intact. HA boots on `from` (2025.11.0, recorder schema 51), the recorder writes
# history, the app is upgraded to `to` (2025.12.0, schema 53), and HA runs the real migration on
# first boot (migrators 52+53). Asserted is the outcome, not just liveness: the schema advances
# 51→53 (the migration signal, see schemaQ), the pre-upgrade `states` history survives, HA
# re-serves on the migrated DB, and the pre-upgrade btrfs snapshot stays a still-schema-51
# (valid) rollback point.
#
# Harness scope (in place, under the running promoter): the core app-upgrade primitives,
# snapshot → new manifest → converge (a bounce) → health-gate, with drbd-reactor live
# throughout, which is the product shape too: an app install never pauses the promoter; the
# maintenance bracket belongs to the OS-upgrade path (covered by the maintenance-contract test),
# and the health-gated auto-rollback is host-side orchestration (agent/host/app.go, unit-tested
# there). What the unit tests cannot show, a real recorder schema migration surviving the upgrade
# with its data intact, is what this proves; the forced-failure → rollback half is
# hass-upgrade-rollback.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # HA's address and readiness path come from its fixture's manifest, never restated here.
  haVip = "http://192.168.1.100:${toString fixture.port}";
  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  # Where the renderer puts HA data, never restated: the app subvolume (what a rollback point
  # snapshots), and haDir, the container subdirectory inside it (what HA writes to, bind-mounted
  # as /config).
  subvol = "/var/lib/briard/data/${fixture.name}";
  haDir = "${subvol}/${fixture.dataDir}";
  db = "${haDir}/home-assistant_v2.db";
  snap = "/var/lib/briard/data/.snapshots/${fixture.name}-preupgrade";
  # The migration signal: max(schema_version) in the schema_changes ledger, which HA-to must
  # advance after detecting the older schema and running migrators 52+53. Not column shape: HA
  # creates a fresh DB at the full model shape of its code, so statistics_meta.unit_class (added
  # to existing DBs by v52) is already present at schema 51 and cannot tell pre from post.
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
      # Two 2.4 GB HA images resident (from serving, to staged), the live Python stack and the
      # migration: real memory and disk headroom. 3072 rather than the 2048 hass-failover and
      # hass-payload take, and the extra GB buys headroom, not speed (a guest grows page cache
      # into what it is given: 4096 measured 4192 MB resident, 3072 measures 3168, 2048 measures
      # 2143, all in the same time within the noise). The recorder schema migration is the one
      # workload in the set whose peak could grow with a future HA, so this keeps a margin those
      # two do not need.
      virtualisation.memorySize = 3072;
      virtualisation.diskSize = 20480;
      # Sqlite3 to read the recorder DB + btrfs to take the pre-upgrade snapshot
      # directly (lib.nix nodes ship no sqlite3, and btrfs only with a fixture; the guest's
      # systemd units carry their own paths).
      environment.systemPackages = [ pkgs.sqlite pkgs.btrfs-progs ];
    };

  # HA boot is slow and the promoter selects the primary dynamically.
  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}
    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=1200)
    node1.succeed("modprobe drbd")
    node1.succeed("briard-test-storage --seed")
    # Single node: no peer — just make it UpToDate so the promoter has quorum-of-1.
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-primary-storage.service", timeout=120)
    node1.succeed("mountpoint -q /var/lib/briard")

    # Both ends warm: `from` is what gets installed, `to` is resident and ready to be installed
    # over it — never a pull on the upgrade path. By DIGEST, which is what the manifests pin.
    node1.succeed('podman image exists "$(cat /run/briard/fixture/ref)"')
    node1.succeed('podman image exists "$(cat /run/briard/fixture/variants/to/ref)"')

    # The install: HA `from` onto the volume this node holds.
    install_fixture(node1)

    # HA (`from`) boots and serves. Generous: first boot initializes the recorder DB.
    node1.wait_until_succeeds("curl -fsS -o /dev/null ${haVip}${fixture.healthPath}", timeout=360)
    node1.wait_until_succeeds("test -f ${db}", timeout=60)
    # Non-vacuous 'data survived': wait for the recorder to commit real history first.
    node1.wait_until_succeeds("test $(sqlite3 ${db} 'SELECT COUNT(*) FROM states') -gt 0", timeout=180)

    # ---- Baseline, read while HA-from is live ----
    # WAL allows concurrent readers, and read live the DB keeps its -shm/-wal healthy, while
    # opening it at rest right after the container stops can hit a hot-WAL "unable to open
    # database file". These values are stable, so a live read is correct and robust.
    pre_schema = node1.succeed("sqlite3 ${db} '${schemaQ}'").strip()
    pre_cols = node1.succeed("sqlite3 ${db} \"${colsQ}\"").strip()
    pre_states = int(node1.succeed("sqlite3 ${db} 'SELECT COUNT(*) FROM states'").strip())
    assert pre_schema == "51", f"baseline recorder schema = {pre_schema}, want 51 (2025.11.0)"
    assert pre_states > 0, f"no recorder history to preserve (states={pre_states})"
    print(f"baseline: schema={pre_schema} states={pre_states} statistics_meta_cols=[{pre_cols}]")

    # ---- Snapshot the rollback point, then pin `to` + cycle onto it in place ----
    # drbd-reactor stays untouched, as in the product: an app upgrade never pauses the promoter
    # (the maintenance bracket belongs to the OS-upgrade path, proven non-destructive by the
    # maintenance contract), and an app restart is not a DRBD event, so the running promoter
    # does not react and the volume stays mounted throughout.
    node1.succeed("findmnt /var/lib/briard")            # still mounted (promoter untouched)
    node1.succeed("btrfs subvolume show ${subvol}")  # the app's data dir is a real subvolume
    node1.succeed("mkdir -p /var/lib/briard/data/.snapshots")
    # -r read-only, as the data.member verb of the guest agent takes it (which also adds -i, the
    # app quota group).
    #
    # ⚠️ Taken live on purpose. The install path stops the app first, so this does not mirror the
    # product: it is the evidence the product comments cite. btrfs snapshots atomically and HA
    # replays its WAL on open, so a crash-consistent point is still recoverable for Home
    # Assistant: a measured fact about one app, never a catalog-wide guarantee (apps-pair.nix
    # measured mosquitto failing it), and what makes the post-crash members of HA trustworthy.
    # Do not "fix" this to stop first: doing so deletes the measurement.
    node1.succeed("btrfs subvolume snapshot -r ${subvol} ${snap}") # the {code,data} rollback point

    # The upgrade: the `to` manifest under the same app name, which is what a version change is:
    # the volume manifest moves, and converge re-renders and bounces the container onto the new
    # digest (starting an already-running unit would leave HA-from serving while every file said
    # otherwise). The bounce stops HA-from cleanly, flushing its DB, before HA-to opens the
    # schema-51 database and migrates it.
    install_fixture(node1, variant="to")

    # ---- Health-gate + migration completion ----
    # HA may answer HTTP while the recorder migrates live in the background, so the real
    # gate is schema==53 (migration done), not merely manifest 200.
    node1.wait_until_succeeds("curl -fsS -o /dev/null ${haVip}${fixture.healthPath}", timeout=360)
    node1.wait_until_succeeds("test $(sqlite3 ${db} '${schemaQ}') -eq 53", timeout=300)

    # ---- Migrated and kept its data ----
    post_schema = node1.succeed("sqlite3 ${db} '${schemaQ}'").strip()
    post_cols = node1.succeed("sqlite3 ${db} \"${colsQ}\"").strip()
    post_states = int(node1.succeed("sqlite3 ${db} 'SELECT COUNT(*) FROM states'").strip())
    assert post_schema == "53", f"recorder schema = {post_schema} after upgrade, want 53 (migration didn't run)"
    assert post_states >= pre_states, f"recorder history shrank across migration: {pre_states} -> {post_states}"
    print(f"migrated: schema {pre_schema}->{post_schema}, states {pre_states}->{post_states}, HA serving on the migrated DB")
    print(f"statistics_meta cols: pre=[{pre_cols}] post=[{post_cols}]")

    # ---- The snapshot is a valid rollback point: still the pre-migration DB (schema 51) ----
    node1.succeed("btrfs subvolume show ${snap}")
    # Taken live, so the WAL-mode DB has recent commits (the schema_changes rows included) in its
    # -wal, not yet checkpointed; an immutable read ignores the WAL and misses them. Copy the DB
    # set to a writable dir and open it normally: sqlite replays the WAL as HA-from would on a
    # rollback, so this also proves the snapshot is a recoverable rollback point.
    node1.succeed("mkdir -p /tmp/snapchk && cp -a ${snap}/${fixture.dataDir}/home-assistant_v2.db* /tmp/snapchk/")
    snap_schema = node1.succeed("sqlite3 /tmp/snapchk/home-assistant_v2.db '${schemaQ}'").strip()
    assert snap_schema == "51", f"pre-upgrade snapshot schema = {snap_schema}, want 51 — not a valid rollback point"
    print("pre-upgrade snapshot kept and still schema 51 — a valid rollback point")
  '';
}
