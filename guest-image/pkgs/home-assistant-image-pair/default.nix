# The HA upgrade-pair fixture: two digest-pinned HA images that straddle a
# real recorder schema migration, so the upgrade tests can drive a *real* `Manager.Upgrade`
# (not a dummy) through the pipeline and assert the DB actually migrated + survived.
#
# Same hermetic-pin discipline as ./home-assistant-image: pull the
# official container by per-arch content digest, never a mutable tag, so Nix
# reproduces it bit-for-bit. This file just pins TWO of them.
#
# ── Why this exact pair (the decision, 2026-07-23) ───────────────────────
# The recorder DB schema is versioned by `SCHEMA_VERSION` in
# homeassistant/components/recorder/db_schema.py; upgrades run the ordered
# migrators in migration.py. Mapping release → schema (read from the tagged
# sources): 2025.1=48, 2025.6/8/9/10=50, 2025.11=51, 2025.12=53, and it has
# stayed 53 across the ENTIRE 2026 line (2026.1 … 2026.7.1 all = 53).
#
#   ⇒ FINDING: there is NO intra-2026 recorder schema migration. A literal
#     `2026.x → 2026.y` pair crosses zero migrators —
#     it would be the silent no-op this fixture exists to avoid. The pair MUST straddle
#     the 2025 line, where the bumps actually live.
#
# Chosen pair — the tightest one that crosses a migrator that TOUCHES SQLITE (HA's
# recorder runs on SQLite here; several bumps are MySQL-only no-ops and would be
# vacuous for us — v46/47 FK rebuild, v53 utf8mb4):
#
#   from = 2025.11.0  (schema 51)
#   to   = 2025.12.0  (schema 53)
#
# What crosses, and what it rewrites on SQLite:
#   • v51 — no-op placeholder ("replaced by v52"; corrects a MySQL string-compare
#           issue). Nothing on SQLite.
#   • v52 — for an EXISTING pre-52 DB: `ALTER TABLE statistics_meta ADD COLUMN
#           unit_class VARCHAR(255)` + backfill by matching unit_of_measurement
#           against the unit converters (applies to SQLite).
#   • v53 — MySQL-only (utf8mb4 charset conversion). No-op on SQLite.
#   Net on SQLite: schema_version 51 → 53.
#
# The migration SIGNAL is schema_version, NOT column shape (corrected empirically): HA creates a fresh DB at its code's full model shape, so a fresh 2025.11.0
# (schema 51) DB ALREADY has statistics_meta.unit_class — column presence can't
# distinguish pre/post, so v52's ADD COLUMN is effectively a no-op on our fresh
# from-DB (the backfill has ~no rows too). What a real migration provably moves is the
# schema_changes ledger: HA-to must detect 51 < 53 and record migrators 52+53. So
# The upgrade tests assert on schema_version 51↔53 (+ history preserved), not on the column.
#
# One month of application delta (2025.11 → 2025.12) keeps unrelated integration
# churn out of the way, isolating the recorder migration — the point of the fixture.
#
# Reversibility (feeds the rollback-safety analysis + the upgrade testsd): HA recorder
# migrations are FORWARD-ONLY — there is no v53→v51 down-migration. Our rollback
# does NOT need one: it restores the whole pre-upgrade btrfs snapshot of the data
# subvolume (recorder DB + `.storage`), which brings back the schema-51 DB file
# verbatim. Reversibility is therefore a property of the atomic snapshot, not of
# HA — total and version-agnostic. (For HA the discarded post-upgrade writes are
# recorder history = disposable by design; the sacred-data caveat that bites other
# services, does not bite here.)
#
# ── Pin provenance / refresh procedure ────────────────────────────────────────
# Per-arch amd64 manifest digests, resolved 2026-07-23 from the ghcr registry API
# (anonymous pull token → manifest list → linux/amd64 sub-manifest digest). v0 is
# x86_64-only (see ./home-assistant-image); arm64 waits for the Pi target.
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
