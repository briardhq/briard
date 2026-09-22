// Package guest defines the GuestManager seam — the host↔guest boundary
// (start/stop/health plus the guest's code identity) — and its real Manager,
// which maps the seam onto guestagent control-channel verbs plus a host-side
// readiness probe. The guest is the VM that carries the services.
//
// It holds no snapshot or restore of its own ([B.143], 2026-09-22): the service
// {manifest + data} rollback belongs to agent/host/service.go, which drives the
// guest agent's data.snapshot/data.restore verbs and pins the manifest.
package guest
