package overlay

import (
	"context"

	"briard.io/shared/api"
)

// OverlayProvider is the seam to the WireGuard overlay (NetBird now, others
// later). Real impls live in subpackages (e.g. overlay/netbird); this package
// holds only the interface + a stub, so core stays provider-free.
type OverlayProvider interface {
	EnrollNode(ctx context.Context, req api.EnrollRequest) (api.NodeIdentity, error)
	NodeName(ctx context.Context) (string, error)
	Up(ctx context.Context) error
	Health(ctx context.Context) (Health, error)
	Teardown(ctx context.Context) error
}

// Health is the overlay's view of connectivity.
type Health struct {
	Up      bool
	Relayed bool // using a relay rather than a direct peer connection
	PeersUp int
}

// Stub is a no-op OverlayProvider for core tests and v0 wiring.
type Stub struct{ Name string }

func (s Stub) EnrollNode(context.Context, api.EnrollRequest) (api.NodeIdentity, error) {
	return api.NodeIdentity{Name: s.Name}, nil
}
func (s Stub) NodeName(context.Context) (string, error) { return s.Name, nil }
func (Stub) Up(context.Context) error                   { return nil }
func (Stub) Health(context.Context) (Health, error)     { return Health{Up: true}, nil }
func (Stub) Teardown(context.Context) error             { return nil }

var _ OverlayProvider = Stub{}
