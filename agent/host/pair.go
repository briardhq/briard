package host

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"briard.io/agent/drbd"
	"briard.io/agent/guestagent"
	"briard.io/agent/platform"
	"briard.io/shared/api"
)

// guestMesher is the slice of the guest client the pairing reconcile drives: give the node its
// replication-subnet address, then either adjust the running resource in place (the primary) or
// bring it up fresh (a blank joiner). *guestagent.Client satisfies it; a fake drives the test.
type guestMesher interface {
	ConfigureNet(ctx context.Context, dev, cidr, vipDev string) error
	Adjust(ctx context.Context, req guestagent.ProvisionRequest) error
	BringUp(ctx context.Context, spec guestagent.BringUpSpec) error
}

// witnessStarter starts the host-side witness-forwarder for a managed cloud-witness pairing.
// platformWitness satisfies it in production (systemd-run); a fake records the spec in tests.
type witnessStarter interface {
	StartForwarder(ctx context.Context, s platform.ForwarderSpec) error
}

// platformWitness is the production witnessStarter -- a thin adapter over platform.StartForwarder.
type platformWitness struct{}

func (platformWitness) StartForwarder(ctx context.Context, s platform.ForwarderSpec) error {
	return platform.StartForwarder(ctx, s)
}

// ApplyPair handles a DirectivePair (runtime anchor pairing): parse the target mesh and
// reconcile this node's DRBD to it, returning the terminal outcome (announce-before-act).
// The op is idempotent -- a re-delivered pair directive reconciles to the same mesh -- so a retry
// re-reports done.
func (cfg Config) applyPair(ctx context.Context, g guestMesher, w witnessStarter, d api.Directive, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	var spec api.MeshSpec
	if err := json.Unmarshal([]byte(d.Payload), &spec); err != nil {
		return failed("bad mesh payload: " + err.Error())
	}
	logf("directive kind=pair: reconcile %s to a %d-peer mesh (join=%t)", spec.Resource, len(spec.Peers), spec.Join)
	// A blank join resyncs the whole volume from the primary; bound it generously but finitely.
	pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := cfg.reconcileMesh(pctx, g, w, spec, logf); err != nil {
		logf("directive pair failed: %v", err)
		return failed(err.Error())
	}
	logf("directive pair applied: node is in the %d-peer mesh", len(spec.Peers))
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone}
}

// ReconcileMesh makes this node's DRBD match the target mesh. Both sides first take a
// routable replication-subnet address (a single node ran over loopback). Then:
//   - the existing serving primary (Join=false) ADJUSTS the running resource in place -- its disk
//     stays attached and UpToDate, quorum just widens as the joiners connect;
//   - a blank joiner (Join=true) BRINGS UP fresh as a NON-seed, so it attaches its fresh replica
//     and resyncs from the primary as SyncTarget (never declares itself UpToDate -- no split-brain).
//
// The node finds its own placement (disk vs diskless) by matching cfg.Node to a peer stanza, so a
// witness bring-up is diskless with no promoter. Blank-join only: invalidating an already-seeded
// joiner to re-home it is the cloud's.
func (cfg Config) reconcileMesh(ctx context.Context, g guestMesher, w witnessStarter, spec api.MeshSpec, logf func(string, ...any)) error {
	target := drbd.Resource{Name: spec.Resource, Device: spec.Device}
	self := -1
	for i, p := range spec.Peers {
		peer := drbd.Peer{Name: p.Name, NodeID: p.NodeID, Address: p.Address, Disk: p.Disk}
		// A forwarded cloud-witness mesh (spec.Witness set) reaches the diskless voter through
		// this node's OWN host forwarder, so every DISK anchor carries a witness-side local
		// address distinct from its LAN mesh Address -- which flips drbd.Config to the explicit-
		// connection form. The witness peer itself gets none (it is the diskless target).
		if spec.Witness != nil && p.Disk != "" {
			peer.WitnessLocal = spec.Witness.LocalAddr
		}
		target.Peers = append(target.Peers, peer)
		if p.Name == cfg.Node {
			self = i
		}
	}
	if self < 0 {
		return fmt.Errorf("pair: this node %q is not in the target mesh", cfg.Node)
	}

	// Give this node its replication-subnet NIC address before touching DRBD (DRBD binds/connects
	// there). Skipped when the sender didn't provide one (e.g. the address is already configured).
	if spec.SystemDev != "" {
		if err := g.ConfigureNet(ctx, spec.SystemDev, spec.SystemCIDR, cfg.VIPDev); err != nil {
			return fmt.Errorf("pair: configure %s: %w", spec.SystemDev, err)
		}
	}

	diskless := target.Peers[self].Disk == ""

	// A forwarded cloud witness needs its host-side hop up before DRBD tries the connection: address
	// the guest's private witness NIC (eth3) and start this host's witness-forwarder. Disk anchors
	// only -- the diskless witness is provisioned out of band, never reached through a
	// forwarder itself. Done before Adjust/BringUp so the path exists when DRBD dials it (DRBD's
	// connect retries, so this is belt-and-braces ordering, not a hard race).
	if spec.Witness != nil && !diskless {
		if err := cfg.bringUpWitness(ctx, g, w, spec.Peers, spec.Witness, logf); err != nil {
			return err
		}
	}
	var promoter []string
	if !diskless {
		promoter = cfg.Promoter // a witness runs no promoter
	}

	if spec.Join {
		logf("pair: joining as %s (diskless=%t) -- fresh bring-up + resync from the primary", cfg.Node, diskless)
		return g.BringUp(ctx, guestagent.BringUpSpec{
			Resource:  target,
			Diskless:  diskless,
			FreshInit: false, // NEVER seed a joiner: attach + resync as SyncTarget
			Promoter:  promoter,
		})
	}

	// The existing serving primary: apply the wider mesh to the RUNNING resource -- no create-md,
	// no restart, the disk is never touched (Adjust asserts exactly `drbdadm adjust`).
	logf("pair: adjusting the serving primary %s in place to the %d-peer mesh", cfg.Node, len(target.Peers))
	req := guestagent.ProvisionRequest{Resource: target.Name, ResConfig: target.Config()}
	if len(promoter) > 0 {
		req.ReactorConfig = drbd.ReactorConfig(target.Name, promoter)
	}
	return g.Adjust(ctx, req)
}

// BringUpWitness stands up this anchor's host-side path to the cloud witness: address the
// guest's private witness NIC (eth3) so its DRBD can bind the witness-side local address, then start
// the host witness-forwarder that tunnels that private link to the cloud witness-proxy over mTLS.
// The forwarder listens at the witness peer's mesh Address (what the guest's DRBD dials); its mTLS
// identity is the host-held anchor cert (cfg.Witness{Cert,Key,CA}). Fails fast if this host lacks
// the forwarder binary or cert material -- before any DRBD change, so a mis-provisioned anchor never
// half-applies a pairing.
func (cfg Config) bringUpWitness(ctx context.Context, g guestMesher, w witnessStarter, peers []api.MeshPeer, mw *api.MeshWitness, logf func(string, ...any)) error {
	listen := ""
	for _, p := range peers {
		if p.Disk == "" {
			listen = p.Address // the diskless witness peer's mesh Address = the host forwarder's listen addr
			break
		}
	}
	if listen == "" {
		return fmt.Errorf("pair: forwarded-witness mesh has no diskless peer to forward to")
	}
	if cfg.ForwarderBin == "" || cfg.WitnessCert == "" || cfg.WitnessKey == "" || cfg.WitnessCA == "" {
		return fmt.Errorf("pair: node lacks witness-forwarder config (bin/cert/key/ca) -- cannot reach the cloud witness")
	}
	// Address the private witness NIC (eth3) -- vipDev="" (the witness link never carries the VIP).
	if err := g.ConfigureNet(ctx, mw.Dev, mw.CIDR, ""); err != nil {
		return fmt.Errorf("pair: configure witness NIC %s: %w", mw.Dev, err)
	}
	logf("pair: starting host witness-forwarder %s -> %s (mTLS %s)", listen, mw.Target, mw.ServerName)
	return w.StartForwarder(ctx, platform.ForwarderSpec{
		Binary:     cfg.ForwarderBin,
		Listen:     listen,
		Target:     mw.Target,
		Cert:       cfg.WitnessCert,
		Key:        cfg.WitnessKey,
		CA:         cfg.WitnessCA,
		ServerName: mw.ServerName,
	})
}
