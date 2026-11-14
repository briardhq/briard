# The HA upgrade-pair fixture: two HA images that straddle a real recorder schema migration, so
# the upgrade tests (hass-upgrade, hass-upgrade-rollback) drive a real HA upgrade (not a dummy)
# through the app-upgrade pipeline and assert the DB actually migrated and survived. Each is the
# official container pinned by per-arch content digest, never a mutable tag, so Nix reproduces
# it bit-for-bit.
#
# ── Why this exact pair ──────────────────────────────────────────────────
# The recorder schema is versioned by `SCHEMA_VERSION` in
# homeassistant/components/recorder/db_schema.py; upgrades run the ordered migrators in
# migration.py. Release → schema (from the tagged sources): 2025.1=48, 2025.6/8/9/10=50,
# 2025.11=51, 2025.12=53, and 53 across the entire 2026 line (2026.1 … 2026.7.1). So there is
# no intra-2026 recorder migration: a `2026.x → 2026.y` pair crosses zero migrators, the silent
# no-op this fixture exists to avoid, and the pair must straddle the 2025 line.
#
# The tightest pair that crosses a migrator touching SQLite (HA recorder runs on SQLite here;
# several bumps are MySQL-only no-ops and would be vacuous for us: v46/47 FK rebuild, v53 utf8mb4):
#
#   from = 2025.11.0  (schema 51)
#   to   = 2025.12.0  (schema 53)
#
# What crosses, and what it rewrites on SQLite:
#   • v51 — no-op placeholder ("replaced by v52"; corrects a MySQL string-compare
#           issue). Nothing on SQLite.
#   • v52 — for an existing pre-52 DB: `ALTER TABLE statistics_meta ADD COLUMN
#           unit_class VARCHAR(255)` + backfill by matching unit_of_measurement
#           against the unit converters (applies to SQLite).
#   • v53 — MySQL-only (utf8mb4 charset conversion). No-op on SQLite.
#   Net on SQLite: schema_version 51 → 53.
#
# The migration signal is schema_version, not column shape: HA creates a fresh DB at its full
# model shape, so a fresh 2025.11.0 (schema 51) DB already has statistics_meta.unit_class and
# v52's ADD COLUMN is effectively a no-op on our fresh from-DB (the backfill has ~no rows too).
# What a real migration provably moves is the schema_changes ledger (HA-to must detect 51 < 53
# and record migrators 52+53), so the upgrade tests assert on schema_version 51↔53 (+ history
# preserved), not on the column. One month of application delta (2025.11 → 2025.12) keeps
# unrelated integration churn out of the way, isolating the recorder migration, the point of
# the fixture.
#
# Reversibility (feeds the rollback-safety analysis + the upgrade tests): HA recorder migrations
# are forward-only, with no v53→v51 down-migration, and our rollback needs none: it restores the
# whole pre-upgrade btrfs snapshot of the data subvolume (recorder DB + `.storage`), bringing
# back the schema-51 DB file verbatim. Reversibility is a property of the atomic snapshot, not
# of HA: total and version-agnostic. (The discarded post-upgrade writes are recorder history,
# disposable by design; the sacred-data caveat that bites other apps does not bite here.)
#
# ── Pin provenance / refresh procedure ────────────────────────────────────────
# Per-arch amd64 manifest digests, resolved from the ghcr registry API
# (anonymous pull token → manifest list → linux/amd64 sub-manifest digest). Pinned for
# x86_64 only; there is no arm64 pin.
# To (re)compute the FOD `sha256`: leave it `lib.fakeSha256`, run
#   nix build .#home-assistant-image-2025_11   (and .#..._2025_12)
# and paste the real hash the build prints back here.
{ dockerTools, lib, stdenv }:

let
  imageName = "ghcr.io/home-assistant/home-assistant";
  mk =
    { version, imageDigest, sha256 }:
    assert lib.assertMsg stdenv.hostPlatform.isx86_64
      "home-assistant-image-pair is pinned for x86_64 only in v0; add the arm64 sha256 to build on aarch64";
    dockerTools.pullImage {
      inherit imageName imageDigest sha256;
      finalImageName = imageName;
      finalImageTag = version;
      os = "linux";
      arch = "amd64";
    };
in
{
  # From — schema 51, pre-migration baseline.
  from = mk {
    version = "2025.11.0";
    imageDigest = "sha256:43ce8d90ebbd8eb45207e9cce3f8c8fe139f27ea87fbbf5c34a9183ee2b2cc9b";
    sha256 = "sha256-DY4ap1mUzWKtXsxMqmtz4slg/ooa/ou1v99knDcJzio=";
  };
  # To — schema 53, runs v52 (unit_class) on first boot.
  to = mk {
    version = "2025.12.0";
    imageDigest = "sha256:021a978e721eb38a202244cea9f5d0f23c430786fbfe573ccd40ca1827764e6c";
    sha256 = "sha256-ywFwazRr4SxUQ5dsb0pr9xg93zJlVGA+vuSS3SjsYQw=";
  };
}
