// Package flockname mints and validates the one identifier in a briard install that a human is
// ever shown: the flock's NAME, as in `briard-amber-otter.local` and (once there is an account)
// `amber-otter.briard.casa`.
//
// # A name is a label, an identity is an id
//
// This package exists because one string used to do four jobs -- the node's identity in the API,
// the guest's hostname, DRBD's `on <name>`, and the mDNS label -- so nothing visible could be
// renamed without moving something load-bearing. The split is three identifiers with one job
// each, and this is the visible one:
//
//	node id     node-scoped,  hidden   DRBD `on <name>`, guest hostname, cloud key
//	flock id    flock-scoped, hidden   service MAC -> DHCP client-id -> THE LEASE
//	flock name  flock-scoped, VISIBLE  mDNS + the future briard.casa label   <- this package
//
// The property that buys: renaming from the UI changes what humans see and touches no MAC, no
// client-id, no DRBD metadata, so A RENAME NEVER MOVES THE ADDRESS. That is also what makes the
// cloud's answer to a taken name cheap -- see Valid.
//
// # Why random words and not the user's login name
//
// The name is not local. By creating an account the household gets `<name>.briard.casa` as the
// canonical way to reach the node (OSS §10.2), so the offline name and the domain name want to
// be the same string -- one identity, whether or not there is an account. A curated random pair
// gets three things that `$SUDO_USER` did not: collisions stop mattering (178,928 names, versus
// every household in the world being `briard-kostas`), the sanitiser disappears entirely (no
// lowercase/strip-dots/truncate/fallback chain, because a curated word is a valid DNS label by
// construction), and the user's login name does not leak onto their LAN and into their router's
// client list.
//
// # The lists are APPEND-ONLY, and that is a contract
//
// A name, once chosen, is written into pet state and -- after a claim -- into the cloud, which
// never reissues a released name. So a word may be ADDED to these lists and may never be removed
// or reordered: removing one orphans every install that already chose it, and reordering one
// changes nothing about validity but makes the freeze impossible to check. TestListsAreAppendOnly
// asserts exactly that against a frozen checksum.
//
// The cloud validates a claimed name with Valid, which is what makes the anti-abuse check work
// without a blocklist: no one can self-assign `login.briard.casa` or squat a brand, because a
// name that is not two words from these lists is not a name. Node and cloud compile the SAME
// package, so they cannot disagree within a release; append-only is what keeps them agreeing
// ACROSS releases, when an older installer's word meets a newer cloud.
package flockname

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"slices"
	"strings"
)

// Separator joins the two words, and separates them from the `briard-` prefix in an mDNS label.
// A hyphen is the only choice that is legal in a DNS label and reads as a word break.
const Separator = "-"

// Generate mints a fresh flock name, e.g. "amber-otter".
//
// crypto/rand rather than math/rand, for a reason that is not cryptographic: this runs once, at
// install, on a machine that may have booted seconds ago, and two nodes installed from the same
// image in the same minute must not draw the same name. A CSPRNG has no seeding question to get
// wrong. The cost is one syscall, once, ever.
func Generate() (string, error) {
	adj, err := pick(adjectives)
	if err != nil {
		return "", err
	}
	animal, err := pick(animals)
	if err != nil {
		return "", err
	}
	return adj + Separator + animal, nil
}

// Valid reports whether name is one Generate could have produced: exactly two words, the first
// from the adjective list and the second from the animal list.
//
// This is the cloud's admission check on a claimed name. It is deliberately strict about SHAPE
// and deliberately silent about UNIQUENESS -- whether `amber-otter` is already taken (or
// tombstoned) is the cloud's own question, answered against its own records. A node's local
// choice is a PROPOSAL, not a reservation: a claim may come back with a different name, and the
// node adopts it, which this design makes free because adopting a name moves no address, no MAC,
// no client-id and no DRBD state.
func Valid(name string) bool {
	adj, animal, ok := strings.Cut(name, Separator)
	if !ok {
		return false
	}
	return inList(adjectives, adj) && inList(animals, animal)
}

// Count returns how many distinct names Generate can produce. Exported because it is the number
// the collision argument rests on, and a number in a comment is a number that goes stale.
func Count() int { return len(adjectives) * len(animals) }

func pick(list []string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		return "", fmt.Errorf("flockname: draw from a %d-word list: %w", len(list), err)
	}
	return list[n.Int64()], nil
}

// inList is a linear scan over ~420 short strings, called once per claim. A map would be faster
// and would need building at init on every node that never validates anything.
func inList(list []string, w string) bool { return slices.Contains(list, w) }
