package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"briard.io/agent/subnet"
	"briard.io/shared/atomicfile"
)

// THIS NODE NUMBERS ITSELF, AND THE AGENT IS WHAT DOES IT.
//
// Two of the three ranges a node needs cannot be asked for: the FLOCK SUBNET (the guests' node IPs
// and the DRBD mesh, which rides the household's own L2) and the PRIVATE LINK (the point-to-point
// tap between a host and its own guest). The third, the POD pool, is guest-internal and never
// configured on the host at all. All three are drawn against the collision landscape this machine
// can actually see (agent/subnet), rather than being constants somebody picked.
//
// THE DRAW IS PART OF CONVERGENCE, not part of the install. It has the same three reasons the rest
// of the network does ([B.150]): it is a decision made from what this host can see, what a host can
// see changes, and a decision frozen into a script at install time is one no release can reach. It
// also wants a LAN to look at -- the flock draw ARP-probes its candidate on the parent device -- so
// it belongs after a usable device has been selected, which is a moment only the agent has.
//
// ⚠️ IT HAPPENS ONCE. The record below is PET state: a node that has drawn keeps what it drew,
// because renumbering a live node breaks every peer that still holds the old address. The re-check
// on a node that moved networks is a READ (checkMovedLAN), never a fresh draw, and its remedy is a
// human's.

// subnetsName is the node's record of what it numbers itself from, beside the other pet caches.
const subnetsName = "subnets"

// drawBudget bounds a draw end to end. Generous because the flock draw ARP-probes the LAN and an
// unanswered probe costs ~750ms, and bounded because the wait loop this runs inside must keep
// ticking: past this the draw fails, the node says so, and the next tick tries again.
const drawBudget = 60 * time.Second

// subnetsPath derives the record from ASSIGNMENT_CACHE's directory, exactly as the LAN record
// does -- that directory IS the node's pet state dir, and deriving keeps a harness that points
// ASSIGNMENT_CACHE elsewhere working without being told twice.
func (cfg Config) subnetsPath() string {
	if cfg.AssignmentCache == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.AssignmentCache), subnetsName)
}

// recordedSubnets is what this node has already committed to: the environment first (SYSTEM_SUBNET
// / PRIV_SUBNET / POD_SUBNET, which is how an operator pins a range the draw would have to guess
// at), then the record. Missing fields come back empty and are drawn.
func (cfg Config) recordedSubnets() subnet.Draw {
	d := subnet.Draw{
		System: os.Getenv("SYSTEM_SUBNET"),
		Priv:   os.Getenv("PRIV_SUBNET"),
		Pod:    os.Getenv("POD_SUBNET"),
	}
	if d.Complete() {
		return d
	}
	path := cfg.subnetsPath()
	if path == "" {
		return d
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return d
	}
	rec := subnet.Parse(b)
	// Field by field rather than wholesale, so an environment override tops up a record rather
	// than replacing one -- and so a node that predates a pool gains only that pool.
	for _, f := range []struct{ dst, rec *string }{
		{&d.System, &rec.System}, {&d.Priv, &rec.Priv}, {&d.Pod, &rec.Pod},
	} {
		if *f.dst == "" {
			*f.dst = *f.rec
		}
	}
	return d
}

// numberThisNode resolves the three ranges and every address derived from them, drawing whatever
// is still missing against dev. It is idempotent: the pass after a draw finds the record and does
// no network work at all.
//
// ⚠️ A CONFIG THAT ALREADY NAMES THIS NODE'S ADDRESS IS NEVER RENUMBERED. SYSTEM_CIDR is this
// node's identity on the system subnet, so a config that states it has been numbered by somebody
// else -- a rig that owns its own substrate, an operator, a mesh directive -- and a draw there
// would be us overruling them. That is the whole guard, and it is one predicate on purpose.
func (cfg Config) numberThisNode(ctx context.Context, dev string, logf func(string, ...any)) (Config, error) {
	if cfg.SystemCIDR != "" {
		return cfg, nil
	}
	d := cfg.recordedSubnets()
	if !d.Complete() {
		// The watchdog rides the draw's own budget (beat.go): the flock draw ARP-probes the LAN,
		// which makes it a long operation on a path that must keep pinging.
		bctx, cancel := cfg.beat.budget(ctx, drawBudget)
		defer cancel()
		drawn, err := subnet.Pick(subnet.Observe(bctx), rand.Reader, subnet.LANProbe(bctx, dev))
		if err != nil {
			return cfg, fmt.Errorf("this node cannot number itself: %w", err)
		}
		// Only the missing fields, so a pinned range survives a draw that filled in the others.
		for _, f := range []struct{ dst, drawn *string }{
			{&d.System, &drawn.System}, {&d.Priv, &drawn.Priv}, {&d.Pod, &drawn.Pod},
		} {
			if *f.dst == "" {
				*f.dst = *f.drawn
			}
		}
		logf("network: this node numbers itself from %s.0/24 (its own address is %s.1)", d.System, d.System)
	}
	cfg.recordSubnets(d, logf)
	return cfg.applyDraw(d), nil
}

// recordSubnets writes what this node ACTUALLY numbers itself from, environment overrides
// included. Otherwise a SYSTEM_SUBNET set on one boot and forgotten on the next renumbers a live
// node back to the drawn value in silence -- the exact failure the record exists to prevent,
// arriving by the escape hatch instead of by the draw.
//
// Durable (tmp + fsync + rename, AGENTS §5) because its only copy is this file and a power cut
// between the draw and the guest's first boot must not lose it. 0644 rather than 0600: these
// addresses are on the wire the moment the guest comes up, so there is nothing here to keep from
// a reader, and a rig asserting about the node's addresses reads this file.
func (cfg Config) recordSubnets(d subnet.Draw, logf func(string, ...any)) {
	path := cfg.subnetsPath()
	if path == "" {
		return
	}
	var b bytes.Buffer
	if err := subnet.Report(&b, d); err != nil {
		logf("network: could not render this node's subnets: %v", err)
		return
	}
	// Called on every pass, and the overwhelmingly common one has nothing to say: a node that
	// already records exactly this writes nothing, so the steady state is one read.
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, b.Bytes()) {
		return
	}
	if err := atomicfile.Write(path, b.Bytes(), 0o644, 0o700); err != nil {
		// NOT FATAL, and the node keeps the addresses it just drew for this life. The cost is a
		// redraw at the next start, which renumbers a node whose guest may already be serving --
		// so it is said loudly rather than swallowed.
		logf("network: could not record this node's subnets at %s: %v -- the next start will redraw", path, err)
	}
}

// applyDraw derives every address the node holds from the three ranges. POSITIONAL on purpose: a
// /24 whose addresses are read off the node-id needs no allocator, no state and no agreement
// beyond the subnet itself. Guests take .1/.2/.3 by node-id and their hosts take the same index
// 128 higher, so the two never collide and a glance at an address says which side of a pair it is.
//
// Index 0, because a fresh install is a single-node flock. A node that later JOINS one renumbers
// into the adopter's subnet, and that address arrives in the pairing's mesh spec rather than from
// here (DESIGN §1.2).
//
// Only empty fields are filled, for the reason numberThisNode's guard exists: a config that states
// an address is a decision, not a gap.
func (cfg Config) applyDraw(d subnet.Draw) Config {
	set := func(dst *string, v string) {
		if *dst == "" {
			*dst = v
		}
	}
	if d.System != "" {
		set(&cfg.SystemCIDR, d.System+".1/24")
		// The HOST's own address on that subnet. It needs one because a standby has no LAN
		// presence and must still be dialable by its guest, and because the guest needs somewhere
		// to reply to when the host dials IT (the reboot gate).
		//
		// ⚠️ THE PREFIX IS THE MACVTAP VALUE. A /32, on the private tap: under macvtap the host is
		// isolated from its own guest on the LAN, so the tap is the only path, and a /24 there
		// would claim an on-link route for the whole system subnet over a wire that reaches
		// exactly one machine -- competing with the route the guest's own peers need. On a BRIDGE
		// the host is genuinely on that L2 and the honest prefix is the subnet's; applySubstrate
		// re-prefixes it, because that is where the substrate is known.
		set(&cfg.SystemHostCIDR, d.System+".129/32")
	}
	if d.Priv != "" {
		set(&cfg.PrivHostCIDR, d.Priv+".1/24")
		set(&cfg.WitnessCIDR, d.Priv+".2/24")
	}
	set(&cfg.PodSubnet, d.Pod)
	return cfg
}
