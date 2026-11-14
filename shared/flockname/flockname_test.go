package flockname

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The lists as frozen when the first installer able to mint a name was built: words may be
// appended after these prefixes; nothing before them may change. Two update rules:
//   - Adding words: leave these four constants alone. The test checks the frozen prefix, so a
//     longer list still passes.
//   - Changing or removing a word: don't. If this test failed, the failure is the point -- see
//     TestListsAreAppendOnly for what breaks.
const (
	frozenAdjectives = 422
	frozenAnimals    = 424
	frozenAdjSHA     = "085265a5c9b000044ac2032718556dfddf14672b4a7dd3ec0e827f62328c5569"
	frozenAnimalSHA  = "1489c2827c11c4e0fbb42186c0de73a58f548d72c12e2fa39a1a1c974138c1cd"
)

func digest(list []string) string {
	sum := sha256.Sum256([]byte(strings.Join(list, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

// TestListsAreAppendOnly is the whole contract in one assertion. A flock name is written into pet
// state at install and, after a claim, into the cloud, which holds it until the household's
// certificate has long lapsed, so the lists can only ever grow. Remove a word and every household
// that drew it fails Valid at its next claim: the cloud stops recognising a name published on
// their LAN, printed in their installer output and (once claimed) their domain. Reorder one and
// nothing breaks today, but the freeze becomes uncheckable, which is how it would break tomorrow.
// Hence the lists are vendored, not imported from petname upstream, whose header says its release
// process regenerates them: a dependency allowed to shrink cannot back an append-only contract.
func TestListsAreAppendOnly(t *testing.T) {
	for _, c := range []struct {
		what      string
		list      []string
		frozenN   int
		frozenSHA string
	}{
		{"adjectives", adjectives, frozenAdjectives, frozenAdjSHA},
		{"animals", animals, frozenAnimals, frozenAnimalSHA},
	} {
		if len(c.list) < c.frozenN {
			t.Fatalf("%s SHRANK: %d words, frozen at %d. Every install that drew one of the %d "+
				"missing words now fails Valid -- its name is published on a LAN, printed in its "+
				"installer output, and is its briard.casa label. Words are APPEND-ONLY.",
				c.what, len(c.list), c.frozenN, c.frozenN-len(c.list))
		}
		if got := digest(c.list[:c.frozenN]); got != c.frozenSHA {
			t.Errorf("%s: the first %d words CHANGED (sha %s, frozen %s). Appending is fine and "+
				"leaves this prefix alone; editing, reordering or replacing a word inside it is "+
				"the thing this test exists to stop.", c.what, c.frozenN, got, c.frozenSHA)
		}
	}
}

// Every word must be a legal DNS label alone and inside the two labels built from it:
// `briard-<adj>-<animal>.local` on the LAN, `<adj>-<animal>.briard.casa` once claimed. The
// curated list is what lets Generate have no sanitiser; this keeps that true as words are appended.
func TestWordsAreValidDNSLabels(t *testing.T) {
	longestAdj, longestAnimal := "", ""
	for _, c := range []struct {
		what    string
		list    []string
		longest *string
	}{
		{"adjective", adjectives, &longestAdj},
		{"animal", animals, &longestAnimal},
	} {
		seen := make(map[string]bool, len(c.list))
		for _, w := range c.list {
			if w == "" {
				t.Errorf("%s list contains an empty word", c.what)
				continue
			}
			for _, r := range w {
				if r < 'a' || r > 'z' {
					t.Errorf("%s %q contains %q -- only lowercase a-z, so no case folding, no "+
						"punycode, and no sanitiser is ever needed", c.what, w, r)
					break
				}
			}
			if seen[w] {
				t.Errorf("%s %q appears twice -- it is not wrong, but it silently skews the draw "+
					"and inflates Count()", c.what, w)
			}
			seen[w] = true
			if len(w) > len(*c.longest) {
				*c.longest = w
			}
		}
	}
	// 63 is the DNS label limit (RFC 1035 2.3.4), and the mDNS form is the longer of the two.
	label := "briard-" + longestAdj + Separator + longestAnimal
	if len(label) > 63 {
		t.Errorf("the longest possible mDNS label is %q (%d chars), over the 63-char DNS limit",
			label, len(label))
	}
	t.Logf("longest label: %s.local (%d chars, limit 63); %d names", label, len(label), Count())
}

func TestGenerateProducesValidNames(t *testing.T) {
	seen := make(map[string]bool)
	for range 500 {
		n, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if !Valid(n) {
			t.Fatalf("Generate produced %q, which Valid rejects -- the cloud would refuse a name "+
				"the installer had already printed and published", n)
		}
		seen[n] = true
	}
	// Not a randomness test, a wiring test: a constant Generate (a mis-seeded source, a list
	// indexed at 0) would return one name 500 times and every assertion above would still pass.
	if len(seen) < 400 {
		t.Errorf("500 draws produced only %d distinct names -- Generate is not drawing from the "+
			"whole list", len(seen))
	}
}

func TestValidRejectsWhatIsNotAName(t *testing.T) {
	// The cloud's admission check: each is something someone would try or an older node might
	// send, and none may be accepted as a briard.casa label.
	for _, c := range []struct{ name, why string }{
		{"", "empty"},
		{"otter", "one word -- no separator"},
		{"amber", "one word, and it is an adjective"},
		{"otter-amber", "right words, wrong order -- adjective must come first"},
		{"amber-otter-2", "a numeric suffix is not a shape this package mints"},
		{"login", "the squat this check exists to refuse"},
		{"briard-amber-otter", "the mDNS label, not the name -- the prefix is ours, not theirs"},
		{"Amber-Otter", "uppercase; no case folding, so this is simply not the name"},
		{"amber_otter", "underscore is not legal in a DNS label"},
		{"amber-otter.briard.casa", "the domain, not the label"},
		{"amber otter", "space"},
		{"amber-", "trailing separator, empty animal"},
		{"-otter", "leading separator, empty adjective"},
	} {
		if Valid(c.name) {
			t.Errorf("Valid(%q) = true, but %s", c.name, c.why)
		}
	}
	// ...and the shape it must accept, including the two words the owner kept deliberately.
	// `amber-otter`, the name the flock-name design was drawn around, is not a name these lists
	// can produce: petname's adjectives are qualities rather than colours, and it has no otter
	// (real names read
	// `brave-elf`, `curious-alien`, `sacred-cardinal`). Adding `amber` and `otter` later is free --
	// append-only forbids removals and permits additions forever -- so this is a note, not a blocker.
	for _, n := range []string{"gentle-badger", "able-ox", "brave-elf", "curious-alien", "happy-corgi"} {
		if !Valid(n) {
			t.Errorf("Valid(%q) = false, but that is a name Generate can produce", n)
		}
	}
}

// The curation is part of the frozen contract, so it gets an assertion rather than a comment:
// these are the words whose removal was the point, and re-importing upstream would bring them
// back silently.
func TestCurationHeld(t *testing.T) {
	for _, w := range []string{"in", "on", "up", "one", "boss", "pet", "top", "tops", "master"} {
		if inList(adjectives, w) {
			t.Errorf("adjective %q is back -- it produces names like briard-%s-dog.local", w, w)
		}
	}
	for _, w := range []string{"man", "kid", "lab", "stud", "imp", "ghoul", "monster", "molly", "mammal"} {
		if inList(animals, w) {
			t.Errorf("animal %q is back -- the curation dropped it deliberately", w)
		}
	}
	for _, w := range []string{"elf", "alien", "dane", "akita", "corgi", "bengal", "kodiak"} {
		if !inList(animals, w) {
			t.Errorf("animal %q was KEPT deliberately and is now missing", w)
		}
	}
}
