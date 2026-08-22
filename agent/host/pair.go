package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"briard.io/agent/drbd"
	"briard.io/agent/guestagent"
	"briard.io/agent/platform"
	"briard.io/shared/api"
	"briard.io/shared/atomicfile"
)

// guestMesher is the slice of the guest client the pairing reconcile drives: give the node its
// replication-subnet address, then either adjust the running resource in place (the primary) or
// bring it up fresh (a blank joiner). *guestagent.Client satisfies it; a fake drives the test.
type guestMesher interface {
	ConfigureNet(ctx context.Context, dev, cidr, vipDev, vipAddr string) error
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
	pctx, cancel := cfg.beat.budget(ctx, 10*time.Minute)
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
	target, self, err := meshTarget(spec, cfg.Node)
	if err != nil {
		return err
	}

	// Give this node its replication-subnet NIC address before touching DRBD (DRBD binds/connects
	// there). Skipped when the sender didn't provide one (e.g. the address is already configured).
	if spec.SystemDev != "" {
		if err := g.ConfigureNet(ctx, spec.SystemDev, spec.SystemCIDR, cfg.VIPDev, cfg.VIPAddr); err != nil {
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
		if err := g.BringUp(ctx, guestagent.BringUpSpec{
			Resource:  target,
			Diskless:  diskless,
			FreshInit: false, // NEVER seed a joiner: attach + resync as SyncTarget
			Promoter:  promoter,
		}); err != nil {
			return err
		}
		return cfg.cacheMesh(spec, logf)
	}

	// The existing serving primary: apply the wider mesh to the RUNNING resource -- no create-md,
	// no restart, the disk is never touched (Adjust asserts exactly `drbdadm adjust`).
	logf("pair: adjusting the serving primary %s in place to the %d-peer mesh", cfg.Node, len(target.Peers))
	req := guestagent.ProvisionRequest{Resource: target.Name, ResConfig: target.Config()}
	if len(promoter) > 0 {
		req.ReactorConfig = drbd.ReactorConfig(target.Name, promoter)
	}
	if err := g.Adjust(ctx, req); err != nil {
		return err
	}
	return cfg.cacheMesh(spec, logf)
}

// meshTarget renders a MeshSpec into this node's DRBD resource and returns its own index in the
// peer list. Split out of reconcileMesh because the RESTORE path needs the identical derivation:
// a cached spec has to become the same .res the pairing produced, and a second rendering of the
// same fact is a second thing that can drift (AGENTS §5).
func meshTarget(spec api.MeshSpec, node string) (drbd.Resource, int, error) {
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
		if p.Name == node {
			self = i
		}
	}
	if self < 0 {
		return drbd.Resource{}, -1, fmt.Errorf("pair: this node %q is not in the target mesh", node)
	}
	return target, self, nil
}

// CacheMesh records the mesh this node has just been reconciled to, node-locally and durably.
//
// AFTER the apply, never before: the cache's meaning is "what this node IS in", so a pairing that
// failed halfway must not leave a mesh the next bring-up would converge to. Same rule and the same
// reason as cacheService writing only past the health gate.
//
// A write failure is LOUD BUT NOT FATAL, and the log line says what it costs, because the pairing
// itself succeeded and undoing it over a cache miss would be the worse trade -- the node is meshed
// and serving; what it has lost is the ability to still be meshed after its guest reboots.
func (cfg Config) cacheMesh(spec api.MeshSpec, logf func(string, ...any)) error {
	if cfg.MeshCache == "" {
		return nil
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(cfg.MeshCache, b, 0o600, 0o700); err != nil {
		logf("pair: WARNING: could not cache the mesh (%v); this node is paired now, but a guest "+
			"reboot would bring it back un-meshed on the single-node config from PEERS", err)
		return nil
	}
	return nil
}

// CachedMesh rebuilds this node's DRBD resource from the last pairing it applied. ok is false when
// nothing has ever been paired -- the shipped single-node state, where PEERS (or its self-peer
// default) is the right answer and this file is correctly absent.
//
// An unusable cache is reported and treated as absent, matching installedService: bringing up on
// the env mesh is what this node did before there was a cache at all, so it is the conservative
// fallback rather than a new failure mode. A spec that no longer names this node is the same case
// (a rename, or a cache carried onto another node) and says so distinctly, because "your mesh does
// not know you" is a different thing to go looking for than "your mesh will not parse".
func (cfg Config) cachedMesh(logf func(string, ...any)) (api.MeshSpec, drbd.Resource, bool) {
	if cfg.MeshCache == "" {
		return api.MeshSpec{}, drbd.Resource{}, false
	}
	b, err := os.ReadFile(cfg.MeshCache)
	if err != nil {
		return api.MeshSpec{}, drbd.Resource{}, false // absent = never paired, the shipped state
	}
	var spec api.MeshSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		logf("mesh cache at %s is unusable (%v); bringing up on the configured PEERS mesh", cfg.MeshCache, err)
		return api.MeshSpec{}, drbd.Resource{}, false
	}
	target, _, err := meshTarget(spec, cfg.Node)
	if err != nil {
		logf("mesh cache at %s does not name this node (%v); bringing up on the configured PEERS mesh", cfg.MeshCache, err)
		return api.MeshSpec{}, drbd.Resource{}, false
	}
	return spec, target, true
}

// RestoreWitnessHop re-establishes this anchor's host-side path to the cloud witness at bring-up.
//
// THE FORWARDER IS A TRANSIENT UNIT ON PURPOSE (systemd-run, detached from the agent's cgroup so an
// agent restart leaves the hop serving), and that is not what this changes. What was missing is the
// other half of the rule the mesh needed: nothing RE-CREATED it. applyPair was the only caller of
// StartForwarder, and the cloud sends a pair directive on demand rather than on registration -- so a
// HOST reboot ended the hop permanently and the restored `.res` named a witness address nothing was
// listening on ([V3b.16b]). A GUEST reboot was never affected: the guest's eth3 is baked
// (disk-image.nix), so its side of the private link comes back on its own.
//
// BEFORE WaitQuorate, which is why this is a step in bring-up rather than a call after it. With the
// peer anchor up the node reaches 2/3 without the witness and the placement would not matter; it
// matters in exactly the case the witness exists for -- peer also down -- where a forwarder started
// after bring-up returns is one started after WaitQuorate has already burned the whole
// BringUpBudget waiting for the quorum this hop was going to supply.
//
// LOUD BUT NEVER FATAL, and that trade is deliberately the opposite of [V3b.16a]'s. A node that
// cannot start its forwarder can still serve on 2/3 with its peer, and failing bring-up would take
// down a node that works -- with the promoter gated, it would not serve at all. Nor is the failure
// silent, which is what [V3b.16a] actually refused to accept: DRBD reports the witness connection
// down, so it lands in NodeStatus's connected count and the redundancy alerter fires on the channel
// that already exists.
//
// Idempotent: StartForwarder probes is-active first, so a hop already serving is left alone and this
// costs nothing on an agent restart or a re-adopted guest.
func (cfg Config) restoreWitnessHop(ctx context.Context, g guestMesher, w witnessStarter, logf func(string, ...any)) {
	if cfg.Mesh.Witness == nil || cfg.Diskless {
		return // no cloud witness in this mesh, or this node IS the diskless voter
	}
	if err := cfg.bringUpWitness(ctx, g, w, cfg.Mesh.Peers, cfg.Mesh.Witness, logf); err != nil {
		logf("WARNING: could not restore the witness hop (%v); this node comes up WITHOUT its cloud "+
			"witness vote -- it serves while its peer anchor is up and loses quorum if that peer "+
			"goes down. DRBD reports the witness connection down, so it also shows as a lost "+
			"replica connection", err)
	}
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
	if err := g.ConfigureNet(ctx, mw.Dev, mw.CIDR, "", ""); err != nil {
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
