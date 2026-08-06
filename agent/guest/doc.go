// Package guest defines the GuestManager seam — the host↔guest boundary and
// the upgrade/rollback mechanism (start/stop/health/snapshot/restore) — and its
// real Manager, which maps the seam onto guestagent control-channel verbs plus a
// host-side readiness probe. The guest is the VM that carries the payload. Scope
// is asymmetric: per-service data snapshot/restore, whole-VM code (the generation
// pin travels on SnapshotRef).
package guest
