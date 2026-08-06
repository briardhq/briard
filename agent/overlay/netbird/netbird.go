// Package netbird implements overlay.OverlayProvider against NetBird (hosted
// netbird.io in v1; self-hosted is the v2 cloud piece). It drives the netbird
// client CLI against a per-node daemon (the system service owns the daemon
// lifecycle; this Provider issues up/status/down against it), so one Provider
// manages one node's overlay membership.
//
// Enrollment is coordinator-mediated: the cloud/controller mints the setup key
// (it holds the management token; the node never does -- the same stance as
// cert issuance, docs cloud-issues-certs-not-node) and hands it to the node,
// which only *consumes* it here via EnrollNode.
package netbird

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"briard.io/agent/overlay"
	"briard.io/shared/api"
)

// Runner runs the netbird CLI. The real impl shells out (NewOSRunner); tests
// supply a fake. Args are the netbird subcommand + flags (binary is the Runner's).
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Config configures a single node's netbird client instance.
type Config struct {
	Binary        string // netbird binary (default "netbird")
	ManagementURL string // e.g. https://api.netbird.io:443
	SetupKey      string // provisioned by the cloud/controller (coordinator-mediated)
	DaemonAddr    string // unix:///run/netbird/sock -- the daemon's CLI socket
	Interface     string // WireGuard iface name (default "wt0")
	Hostname      string // this node's name on the overlay
	DisableDNS    bool   // skip host DNS config (set in the netns test fleet)
}

const (
	defaultBinary  = "netbird"
	defaultMgmtURL = "https://api.netbird.io:443"
	defaultIface   = "wt0"
)

// Provider is the OverlayProvider backed by NetBird.
type Provider struct {
	cfg Config
	run Runner
}

// New builds a Provider. If run is nil it shells out to cfg.Binary.
func New(cfg Config, run Runner) *Provider {
	if cfg.Binary == "" {
		cfg.Binary = defaultBinary
	}
	if cfg.ManagementURL == "" {
		cfg.ManagementURL = defaultMgmtURL
	}
	if cfg.Interface == "" {
		cfg.Interface = defaultIface
	}
	if run == nil {
		run = NewOSRunner(cfg.Binary)
	}
	return &Provider{cfg: cfg, run: run}
}

var _ overlay.OverlayProvider = (*Provider)(nil)

// EnrollNode registers this node with the setup key and brings the overlay up,
// returning its identity (name + overlay address) once connected.
func (p *Provider) EnrollNode(ctx context.Context, req api.EnrollRequest) (api.NodeIdentity, error) {
	if p.cfg.SetupKey == "" {
		return api.NodeIdentity{}, errors.New("netbird: enroll needs a setup key (minted by the coordinator)")
	}
	name := req.NodeName
	if name == "" {
		name = p.cfg.Hostname
	}
	up := []string{"up", "--setup-key", p.cfg.SetupKey,
		"--management-url", p.cfg.ManagementURL, "--interface-name", p.cfg.Interface}
	if name != "" {
		up = append(up, "--hostname", name)
	}
	if p.cfg.DisableDNS {
		up = append(up, "--disable-dns")
	}
	if _, err := p.run.Run(ctx, p.args(up...)...); err != nil {
		return api.NodeIdentity{}, fmt.Errorf("netbird: enroll: %w", err)
	}
	return p.identity(ctx)
}

// NodeName returns this node's overlay FQDN.
func (p *Provider) NodeName(ctx context.Context) (string, error) {
	id, err := p.identity(ctx)
	return id.Name, err
}

// Up reconnects an already-enrolled node (no setup key).
func (p *Provider) Up(ctx context.Context) error {
	up := []string{"up", "--management-url", p.cfg.ManagementURL, "--interface-name", p.cfg.Interface}
	if p.cfg.DisableDNS {
		up = append(up, "--disable-dns")
	}
	if _, err := p.run.Run(ctx, p.args(up...)...); err != nil {
		return fmt.Errorf("netbird: up: %w", err)
	}
	return nil
}

// Health reports overlay connectivity from `netbird status --json`.
func (p *Provider) Health(ctx context.Context) (overlay.Health, error) {
	out, err := p.run.Run(ctx, p.args("status", "--json")...)
	if err != nil {
		return overlay.Health{}, fmt.Errorf("netbird: status: %w", err)
	}
	var s statusJSON
	if err := json.Unmarshal(out, &s); err != nil {
		return overlay.Health{}, fmt.Errorf("netbird: status parse: %w", err)
	}
	connected, relayed := 0, 0
	for _, d := range s.Peers.Details {
		if strings.EqualFold(d.Status, "Connected") {
			connected++
			if strings.EqualFold(d.ConnectionType, "Relayed") {
				relayed++
			}
		}
	}
	return overlay.Health{
		Up:      s.Management.Connected && s.Signal.Connected,
		PeersUp: connected,
		// Relayed when every connected peer is on relay (no direct path at all).
		// A single direct peer flips it false. On hosted netbird.io behind the
		// fleet's shared outer NAT this is the norm.
		Relayed: connected > 0 && relayed == connected,
	}, nil
}

// Teardown disconnects the overlay (leaves the daemon running).
func (p *Provider) Teardown(ctx context.Context) error {
	if _, err := p.run.Run(ctx, p.args("down")...); err != nil {
		return fmt.Errorf("netbird: down: %w", err)
	}
	return nil
}

func (p *Provider) identity(ctx context.Context) (api.NodeIdentity, error) {
	out, err := p.run.Run(ctx, p.args("status", "--json")...)
	if err != nil {
		return api.NodeIdentity{}, fmt.Errorf("netbird: status: %w", err)
	}
	var s statusJSON
	if err := json.Unmarshal(out, &s); err != nil {
		return api.NodeIdentity{}, fmt.Errorf("netbird: status parse: %w", err)
	}
	// NetbirdIp carries a CIDR mask (e.g. 100.116.35.175/16); the identity wants the bare address.
	addr := s.NetbirdIP
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		addr = addr[:i]
	}
	return api.NodeIdentity{Name: s.Fqdn, Address: addr}, nil
}

// Args appends the daemon socket to a subcommand's flags when configured.
func (p *Provider) args(base ...string) []string {
	if p.cfg.DaemonAddr != "" {
		base = append(base, "--daemon-addr", p.cfg.DaemonAddr)
	}
	return base
}

// statusJSON is the subset of `netbird status --json` this Provider reads.
type statusJSON struct {
	Management struct {
		Connected bool `json:"connected"`
	} `json:"management"`
	Signal struct {
		Connected bool `json:"connected"`
	} `json:"signal"`
	Fqdn      string `json:"fqdn"`
	NetbirdIP string `json:"netbirdIp"`
	Peers     struct {
		Connected int `json:"connected"`
		Details   []struct {
			Status         string `json:"status"`
			ConnectionType string `json:"connectionType"`
		} `json:"details"`
	} `json:"peers"`
}

// OSRunner shells out to the netbird binary.
type OSRunner struct{ binary string }

// NewOSRunner returns a Runner that execs the given netbird binary.
func NewOSRunner(binary string) OSRunner {
	if binary == "" {
		binary = defaultBinary
	}
	return OSRunner{binary: binary}
}

// Run executes `netbird <args...>` and returns combined output.
func (r OSRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, r.binary, args...).CombinedOutput()
}
