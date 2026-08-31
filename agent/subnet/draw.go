// Package subnet draws the two private IPv4 ranges a briard node needs and cannot ask anyone
// for: the FLOCK SUBNET (DESIGN §4's system subnet -- the guests' node IPs and the DRBD mesh,
// which rides the household's own L2 by design) and the PRIVATE LINK (the point-to-point tap
// between a host and its own guest, the guest's eth3).
//
// WHY DRAW RATHER THAN HARDCODE. Both used to be constants -- 10.0.0.0/24 and 10.11.9.0/24 --
// and 10.0.0.0/24 is one of the most common real household subnets there is: every Xfinity
// gateway ships it. Two subnets with the same numbers on one L2 is not a degraded install, it is
// an install that cannot talk to half the house, and [V3b.26b] made it live rather than latent
// (before it, a lone node had no system address at all). The link's collision is quieter and
// just as real: it never touches the LAN, so it cannot collide at L2 -- but the HOST'S ROUTING
// TABLE is shared, so a household on 10.11.9.0/24 gives the host two identical on-link /24s and
// Linux picks between them by metric and insertion order. Both directions then break, and which
// one is not ours to say ([V3b.26f]).
//
// THE SHAPE OF THE ANSWER: draw with real entropy, exclude the ranges convention has already
// spoken for, and check the draw against what this host can actually see. That turns "unlikely to
// collide" into "observed not to" for the collisions we can observe, and leaves the rest to the
// ~59,000 candidates the pool holds. What it deliberately does NOT solve is the LATE collision --
// a user who joins a VPN or adds a router after us -- because renumbering a live node is a
// renumber, and the flock subnet is flock-scoped, so it moves the whole flock.
//
// Pick is PURE (a reader, an observation, a probe function in; two subnets out), so every
// rejection path is unit-tested against a fabricated host; Observe and LANProbe (observe.go) are
// the thin impure halves.
package subnet

import (
	"fmt"
	"io"
	"net/netip"
)

// Draw is the pair an install commits to, each as the first three octets ("10.173.94") -- the
// form install.sh writes and the only form anything downstream needs, because the addresses in a
// /24 are positional: guests take .1/.2/.3 by node-id and their hosts take the same index 128
// higher.
type Draw struct {
	System string // the flock subnet -- LAN-scoped, shared by every node in the flock
	Priv   string // the private host<->guest link -- node-private, never on the LAN
}

// attempts bounds each draw. Statistically absurd as a bound on bad luck -- the conventional
// exclusions reject under 10% of the pool, so sixteen consecutive rejections by chance is a
// number with more zeros than this comment -- which is the point: reaching it means the host
// really does have 10/8 carved up, and then we must REFUSE rather than invent. That is DESIGN
// §4's existing posture for the VIP ("no DHCP server and no BRIARD_VIP -> refuse, naming the
// variable"), and the alternative is a node that installs green and cannot serve half the house.
//
// It is also a wall-clock bound, because the flock draw ARP-probes each surviving candidate:
// ~750ms of unanswered probe each, so sixteen is ~12s worst case and one probe (~750ms) in every
// realistic case.
const attempts = 16

// Pick draws both subnets.
//
// rnd is the entropy source (crypto/rand.Reader in production, a fixed reader in tests). obs is
// what this host can already see of 10/8; answers asks the LAN whether a candidate flock subnet's
// .1 is already somebody's, and may be nil where nobody can be asked.
//
// The two draws are structurally disjoint rather than checked against each other: the flock pool
// excludes 10.11.x, which is the whole of the link pool. So a link can never land on its own
// flock's subnet, and no ordering between the two draws can make it.
func Pick(obs Observed, rnd io.Reader, answers func(netip.Addr) bool) (Draw, error) {
	sys, err := draw(rnd, systemCandidate, func(p netip.Prefix) string {
		if where := obs.Occupied(p); where != "" {
			return where
		}
		// The flock subnet is the one that rides the household's L2, so it is the one with a
		// second question to ask: is another flock already here? An existing node answers on its
		// node IP, and node IPs are positional -- .1 is node-id 0, which every flock has.
		//
		// ⚠️ This detects only a flock that is POWERED ON and on this L2. A node that is off
		// during our install collides silently later. It narrows the window; it does not close it.
		if answers != nil && answers(nodeIP(p)) {
			return "a machine already answers at " + nodeIP(p).String()
		}
		return ""
	})
	if err != nil {
		return Draw{}, fmt.Errorf("could not draw a system subnet: %w; set BRIARD_SYSTEM_SUBNET to one this machine can use", err)
	}
	priv, err := draw(rnd, privCandidate, obs.Occupied)
	if err != nil {
		return Draw{}, fmt.Errorf("could not draw a private-link subnet: %w; set BRIARD_PRIV_SUBNET to one this machine can use", err)
	}
	return Draw{System: three(sys), Priv: three(priv)}, nil
}

// draw is the loop both pools share: propose, reject with a reason, stop at the bound. reject
// returns "" for a candidate it accepts and the reason otherwise -- a string rather than a bool
// so the refusal can say what the machine actually looked like, which is the difference between
// a user who can act on it and one who files a bug.
func draw(rnd io.Reader, propose func(io.Reader) (netip.Prefix, error), reject func(netip.Prefix) string) (netip.Prefix, error) {
	var last string
	for range attempts {
		p, err := propose(rnd)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("no entropy: %w", err)
		}
		if why := reject(p); why != "" {
			last = fmt.Sprintf("%s (%s)", p, why)
			continue
		}
		return p, nil
	}
	return netip.Prefix{}, fmt.Errorf("%d candidates all collided, the last being %s", attempts, last)
}

// systemCandidate proposes a flock subnet: 10.R.R.0/24, both octets random, minus the ranges
// convention has spoken for.
//
// THE POOL IS 10.0.0.0/8 AND NOWHERE ELSE. 172.16.0.0/12 is Docker's in practice (default bridge
// 172.17.0.0/16, compose networks upward from 172.18); 192.168.0.0/16 is the most CPE-collided
// space there is; and 100.64.0.0/10 (RFC 6598) is actively hostile -- ISP CGNAT on the DS-Lite
// market DESIGN §4.3 names, Tailscale, and our own NetBird-class overlay all live there. The
// benchmarking and documentation ranges (198.18.0.0/15, 192.0.2.0/24, 198.51.100.0/24,
// 203.0.113.0/24) are unrouted and therefore tempting, but squatting non-private space breaks the
// day a user legitimately needs to reach it.
func systemCandidate(rnd io.Reader) (netip.Prefix, error) {
	var o [2]byte
	for range 4 * attempts {
		if _, err := io.ReadFull(rnd, o[:]); err != nil {
			return netip.Prefix{}, err
		}
		p := slash24(10, o[0], o[1])
		if conventional(p) == "" {
			return p, nil
		}
	}
	// Only reachable with a reader that keeps handing back excluded octets -- i.e. a test's, or a
	// broken one. Real entropy clears the ~9% exclusion in a handful of reads.
	return netip.Prefix{}, fmt.Errorf("entropy kept proposing reserved ranges")
}

// privCandidate proposes a private link: 10.11.R.0/24, one octet random.
//
// 256 candidates is plenty here, and ~65,000 is wanted for the flock, because the two have
// different scopes. The link is node-private -- a point-to-point wire on one host -- so two nodes
// in one flock may draw the SAME 10.11.R without conflict; they are separate wires that never
// meet. The flock subnet must miss every OTHER flock on the same LAN as well as the LAN itself.
//
// 10.11.x DELIBERATELY, and not the 10.9.x the code used all along: renaming the pool off its
// historical value means any surviving `10.9.` in either tree is provably a leftover rather than
// a judgement call, which turns a class of rogue use into one grep.
//
// ⚠️ THE POOL IS L2 SUBSTRATE AND NO BRIARD CODE MAY REFERENCE IT. On Linux, macvtap leaves host
// and guest unable to route each other's subnets directly, and these addresses on eth3 are what
// both routing tables hang that routing off -- so the pool is permanent, whatever else stops using
// it (scripts/install.sh's PRIV_HOST_CIDR note). It does not exist at all on a Windows host, which
// is what makes the rule a rule rather than a preference: anything that dials this range cannot
// run where the link was never built. A product network that needs addresses of its own draws its
// own pool and excludes it below, exactly as this one is excluded from the flock's.
func privCandidate(rnd io.Reader) (netip.Prefix, error) {
	var o [1]byte
	if _, err := io.ReadFull(rnd, o[:]); err != nil {
		return netip.Prefix{}, err
	}
	return slash24(10, 11, o[0]), nil
}

// occupied is the conventional-occupant table: ranges inside 10/8 that are already somebody's by
// convention, so a draw that lands on one is a collision waiting for the user to install the
// other thing.
//
// The collisions cluster on ROUND, LOW, MEMORABLE numbers, because that is what humans and vendor
// defaults pick -- which is also why a random pair like 10.173.94.0/24 is nearly safe by
// construction, and why the belt at the end (10.1/10.10/10.100/10.200) is hand-picked rather than
// sourced: nobody documents "operators like round numbers", but every network engineer has met it.
//
// Each sourced entry was verified rather than recalled, because a table of remembered constants is
// a table of plausible wrong numbers.
var occupied = []struct {
	prefix netip.Prefix
	who    string
}{
	{netip.MustParsePrefix("10.0.0.0/24"), "Comcast/Xfinity gateways (10.0.0.1) -- and our own worn-out example"},
	{netip.MustParsePrefix("10.0.1.0/24"), "Apple AirPort/Time Capsule (10.0.1.1, DHCP from .2)"},
	{netip.MustParsePrefix("10.0.2.0/24"), "qemu's SLIRP -- WE SHIP IT: the guest's eth0 is 10.0.2.15"},
	{netip.MustParsePrefix("10.8.0.0/24"), "OpenVPN's sample server.conf, copied everywhere"},
	{netip.MustParsePrefix("10.96.0.0/12"), "Kubernetes' default service CIDR (kubeadm)"},
	{netip.MustParsePrefix("10.244.0.0/16"), "Flannel's default pod network"},
	{netip.MustParsePrefix("10.147.0.0/16"), "ZeroTier's default managed ranges"},
	{netip.MustParsePrefix("10.1.0.0/16"), "a round number operators pick by hand"},
	{netip.MustParsePrefix("10.10.0.0/16"), "a round number operators pick by hand"},
	{netip.MustParsePrefix("10.100.0.0/16"), "a round number operators pick by hand"}, // already inside the k8s /12; kept for the day that moves
	{netip.MustParsePrefix("10.200.0.0/16"), "a round number operators pick by hand"},
	// OURS, and the entry that does the structural work: excluding the whole link pool from the
	// flock pool is what keeps the two disjoint without either draw knowing about the other.
	{netip.MustParsePrefix("10.11.0.0/16"), "briard's own private-link pool -- L2 substrate, permanent, referenced by no code"},
}

// conventional names the occupant of p, or "" if the range is free of them.
func conventional(p netip.Prefix) string {
	for _, o := range occupied {
		if o.prefix.Overlaps(p) {
			return o.who
		}
	}
	return ""
}

// Observed is what this host can already see of the address space around it: its own addresses
// and the routes it holds.
//
// AN EMPTY Observed DRAWS FREELY, and that asymmetry is deliberate -- the same one the report
// card's VIP probe uses. This check turns evidence-of-use into a redraw and must never turn
// absence-of-evidence into a refusal: a host we could not read is a host that installs, exactly
// as it did before this existed.
type Observed struct {
	// Prefixes is every range this host has an address in or a route to, already cut to the
	// prefix lengths worth honouring (see Observe).
	Prefixes []netip.Prefix
	// Where names each prefix's origin ("the address on eth0", "a route on tun0"), so a refusal
	// can describe the machine rather than assert about it.
	Where map[netip.Prefix]string
}

// Occupied names what already holds p, or "" if nothing this host can see does.
//
// It knows nothing of the conventional table on purpose: that exclusion belongs to the flock
// POOL (systemCandidate applies it when proposing) and not to the link's, whose whole pool is one
// of the table's entries. Folding the two together here would reject every link candidate.
func (o Observed) Occupied(p netip.Prefix) string {
	for _, q := range o.Prefixes {
		if q.Overlaps(p) {
			if w := o.Where[q]; w != "" {
				return fmt.Sprintf("%s is %s", q, w)
			}
			return q.String() + " is already on this machine"
		}
	}
	return ""
}

// slash24 builds 10.a.b.0/24 -- the only shape either pool produces.
func slash24(a, b, c byte) netip.Prefix {
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{a, b, c, 0}), 24)
}

// nodeIP is the address node-id 0 takes in p: the one an existing flock answers on, and the one
// this node will take if the draw stands.
func nodeIP(p netip.Prefix) netip.Addr { return p.Addr().Next() }

// three renders a /24 the way install.sh spells it -- first three octets, no prefix.
func three(p netip.Prefix) string {
	o := p.Addr().As4()
	return fmt.Sprintf("%d.%d.%d", o[0], o[1], o[2])
}
