package guestagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/shared/chain"
)

// diskExec is fakeExec with a REAL WriteFile. The thing under test is a file systemd can load,
// so a fake that keeps writes in a map would let every assertion below pass over a renderer that
// wrote nothing at all -- which is exactly the vacuity this item was created by.
type diskExec struct{ fakeExec }

func (d *diskExec) WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// guestWithTools points the renderer at a temporary unit directory and a temporary tool profile,
// and returns both. Every assertion below is about a real file on disk, because the thing under
// test is a file on disk: a unit systemd can load.
func guestWithTools(t *testing.T) (unitDir, tools string) {
	t.Helper()
	unitDir = t.TempDir()
	tools = filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIARD_UNIT_DIR", unitDir)
	t.Setenv("BRIARD_TOOLS_BIN", tools)
	t.Setenv("BRIARD_BIN_DIR", "/var/lib/briard-bin")
	return unitDir, tools
}

// THE UNIT EXISTS, AND IT IS THE ONE THE HOST IS ABOUT TO START ([B.160]). The whole item is
// "the unit can never be older than the binary that wrote it", so what is asserted is that this
// binary's start produces a loadable briard-node-storage.service naming this binary's committed
// path and this image's tool profile -- the two halves the crash of [B.159](c) had disagree.
func TestWriteUnitsRendersNodeStorage(t *testing.T) {
	dir, tools := guestWithTools(t)
	f := &diskExec{}
	if err := WriteUnits(context.Background(), f); err != nil {
		t.Fatalf("WriteUnits: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "briard-node-storage.service"))
	if err != nil {
		t.Fatalf("the unit the host starts was not written: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=no",
		"Environment=PATH=" + tools,
		"ExecStart=/var/lib/briard-bin/briard-guest-agent --node-storage",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered unit is missing %q:\n%s", want, got)
		}
	}
	// NO [Install], which is the old `wantedBy = [ ]`: this unit runs when the host says so and
	// never at boot. A rendered [Install] would be silently inert here (nothing enables it) and
	// wrong the moment anything did.
	if strings.Contains(got, "[Install]") {
		t.Errorf("node storage must not be enablable:\n%s", got)
	}
}

// A UNIT WRITTEN AND NOT RELOADED IS A UNIT systemctl SAYS DOES NOT EXIST -- the same symptom
// [B.159](c) measured, reached by a different route. systemd reads unit files at load, so the
// reload is not housekeeping and its absence would pass every content assertion above.
func TestWriteUnitsReloadsSystemd(t *testing.T) {
	guestWithTools(t)
	f := &diskExec{}
	if err := WriteUnits(context.Background(), f); err != nil {
		t.Fatalf("WriteUnits: %v", err)
	}
	var reloaded bool
	for _, r := range f.runs {
		if len(r) == 2 && r[0] == "systemctl" && r[1] == "daemon-reload" {
			reloaded = true
		}
	}
	if !reloaded {
		t.Fatalf("no daemon-reload after writing the units; ran %v", f.runs)
	}
}

// AN IMAGE THAT PREDATES [B.160] HAS NO TOOL PROFILE, and the agent must refuse to serve on it
// rather than write units whose PATH resolves to nothing. That refusal is what makes a
// too-new agent safe: the trial fails, the picker restores the committed binary, and the node
// keeps running the release it had -- instead of promoting into units it cannot support.
func TestWriteUnitsRefusesAnImageWithNoToolProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIARD_UNIT_DIR", dir)
	t.Setenv("BRIARD_TOOLS_BIN", filepath.Join(t.TempDir(), "absent"))
	f := &diskExec{}
	err := WriteUnits(context.Background(), f)
	if err == nil {
		t.Fatal("rendered units against an image with no tool profile")
	}
	if !strings.Contains(err.Error(), "tool profile") {
		t.Errorf("the refusal must name what is missing, got %q", err)
	}
	// AND IT MUST NOT HAVE WRITTEN ANYTHING FIRST. A half-rendered set that then fails is worse
	// than none: the next start's reload would load whatever landed before the error.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Errorf("refusing left %d file(s) behind in %s", len(ents), dir)
	}
}

// A TOOL PROFILE THAT IS A FILE, OR A DANGLING SYMLINK, IS NOT A TOOL PROFILE. Both are how a
// half-applied image presents, and both must route to the same refusal as absence rather than to
// a unit that fails at exec time on the promotion path.
func TestWriteUnitsRefusesAToolProfileThatIsNotADirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, base string) string
	}{
		{"a plain file", func(t *testing.T, base string) string {
			p := filepath.Join(base, "bin")
			if err := os.WriteFile(p, []byte("not a profile"), 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"a dangling symlink", func(t *testing.T, base string) string {
			p := filepath.Join(base, "bin")
			if err := os.Symlink(filepath.Join(base, "gone"), p); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("BRIARD_UNIT_DIR", t.TempDir())
			t.Setenv("BRIARD_TOOLS_BIN", tc.make(t, base))
			if err := WriteUnits(context.Background(), &diskExec{}); err == nil {
				t.Fatal("rendered units against a tool profile that is not a directory")
			}
		})
	}
}

// THE RENDER IS IDEMPOTENT, because it runs on every start of the agent -- a crash-restart, a
// re-adopt, a trial that came back. Two renders must leave exactly the state one leaves, or the
// mechanism that removes the defect class introduces a new one.
func TestWriteUnitsIsIdempotent(t *testing.T) {
	dir, _ := guestWithTools(t)
	for i := 0; i < 2; i++ {
		if err := WriteUnits(context.Background(), &diskExec{}); err != nil {
			t.Fatalf("WriteUnits #%d: %v", i+1, err)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != len(units()) {
		t.Fatalf("two renders left %d files for %d units", len(ents), len(units()))
	}
}

// THE CHAIN IS AN ORDERED CHAIN, and the order is a dependency rather than a preference: each
// member Requires= and After= the PREVIOUS one, so the door starts only once briard-vip holds
// the address its names resolve to ([B.160], shared/chain). A rendered set that got this wrong
// would still start on a lone node -- the target Wants them all -- and would start them in
// whatever order systemd liked, which is how a door comes up before its address.
func TestWriteUnitsChainsTheMembersInOrder(t *testing.T) {
	guestWithTools(t)
	u := units()
	members := chain.Members()
	for i, m := range members {
		text, ok := u[m]
		if !ok {
			t.Fatalf("chain member %q is not rendered at all -- naming a unit the guest does not define fails the WHOLE promotion", m)
		}
		if !strings.Contains(text, "PartOf="+chain.Target+"\n") {
			t.Errorf("%s is not PartOf %s, so a lone node's chain would not stop with its target:\n%s", m, chain.Target, text)
		}
		if i == 0 {
			continue
		}
		for _, want := range []string{"Requires=" + members[i-1] + "\n", "After=" + members[i-1] + "\n"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s is missing %q -- the chain's order is its dependency:\n%s", m, strings.TrimSpace(want), text)
			}
		}
	}
	// The lone node's target carries the identical list, so a lone node and a flock run ONE
	// chain and the members cannot tell which target started them ([B.145c]).
	tgt := u[chain.Target]
	for _, k := range []string{"Wants=", "After="} {
		if !strings.Contains(tgt, k+strings.Join(members, " ")+"\n") {
			t.Errorf("%s is missing %s over the exact member list:\n%s", chain.Target, k, tgt)
		}
	}
}

// EVERY CHAIN MEMBER HANDS THE RESOURCE ON WHEN IT GIVES UP ([V3b.5](c)). OnFailure= is the
// STATE hook that fires on the transition into failed, and the start limit is what decides when
// that is -- a member rendered without either would crash quietly forever while the household
// had no service, which is the shape the budget exists to end.
func TestWriteUnitsEveryMemberCanHandTheResourceOn(t *testing.T) {
	guestWithTools(t)
	u := units()
	for _, m := range chain.Members() {
		// WHOLE LINES, not substrings: "StartLimitBurst=5" is a prefix of "StartLimitBurst=50",
		// so a substring check passes over a budget ten times the intended one -- which is a
		// door that keeps crash-looping instead of handing the house to a peer.
		for _, want := range []string{
			"OnFailure=" + holdUnit + "\n",
			"OnFailureJobMode=replace-irreversibly\n",
			"StartLimitIntervalSec=300\n",
			"StartLimitBurst=5\n",
		} {
			if !strings.Contains(u[m], want) {
				t.Errorf("%s is missing %q:\n%s", m, want, u[m])
			}
		}
	}
	// AND THE HOLD ITSELF IS NOT A MEMBER: a hold that fired its own OnFailure would loop.
	if strings.Contains(u[holdUnit], "OnFailure=") {
		t.Errorf("the hold must not hand the resource on to itself:\n%s", u[holdUnit])
	}
	// NOR IS THE RENEWAL TIMER'S SERVICE ([V3b.5c]): a renewal that fails must never be able to
	// demote a serving node.
	if strings.Contains(u[vipRenewUnit], "OnFailure=") || strings.Contains(u[vipRenewUnit], "PartOf="+chain.Target) {
		t.Errorf("the lease renewal must not be able to demote a serving node:\n%s", u[vipRenewUnit])
	}
}

// THE HOLD STOPS THE WHOLE CHAIN ON A LONE NODE, AND IN REVERSE. `systemctl stop` of a target
// returns as soon as the TARGET is down -- measured, with the volume still mounted and the shim
// then escalating to a reboot -- so the stop has to NAME every member. This is the one place the
// agent's knowledge of the chain reaches an image-side tool, as arguments, so getting the list
// or its direction wrong is silent until a member gives up in the field.
func TestWriteUnitsHoldStopsEveryMemberInReverse(t *testing.T) {
	guestWithTools(t)
	hold := units()[holdUnit]
	members := chain.Members()
	want := "briard-hold-stop " + chain.Target + " " + strings.Join(reversed(members), " ")
	if !strings.Contains(hold, want) {
		t.Errorf("the hold's stop does not name the chain in reverse; want %q in:\n%s", want, hold)
	}
	// The release resets the start limit of every member, FORWARD order being irrelevant but
	// completeness not: a member left inside its StartLimitIntervalSec when the hold ends is one
	// systemd refuses to start on the next promotion, which would pin the hold to that window.
	if !strings.Contains(hold, "reset-failed "+strings.Join(members, " ")) {
		t.Errorf("the hold does not clear every member's start limit:\n%s", hold)
	}
	// AND THE LONE NODE'S RESTART IS HANDED THE TARGET IT MUST START ([B.145c]). Measured on the
	// first single-node-chain run of [B.160]b: the step took `$1`, the unit passed nothing, and
	// `set -u` made it "unbound variable" -- on an ExecStopPost the unit deliberately marks `-`,
	// so systemd swallowed it and the lone node simply never came back. Hold-and-restart is the
	// lone node's whole answer to a member giving up; a silent no-op there is a dead house.
	if !strings.Contains(hold, "briard-hold-restart "+chain.Target+"\n") {
		t.Errorf("the hold's restart is not told which target to start:\n%s", hold)
	}
}

// THE HOLD LENGTH IS A CONSTANT WITH A TEST OVERRIDE, which is the whole reason the nix option
// could go: the contract rigs drive the full lifecycle and cannot wait five minutes.
func TestWriteUnitsHoldSecs(t *testing.T) {
	guestWithTools(t)
	if !strings.Contains(units()[holdUnit], "/sleep 300\n") {
		t.Errorf("a field node must hold for the default:\n%s", units()[holdUnit])
	}
	t.Setenv("BRIARD_PROMOTION_HOLD_SECS", "5")
	if !strings.Contains(units()[holdUnit], "/sleep 5\n") {
		t.Errorf("a rig must be able to shorten the hold:\n%s", units()[holdUnit])
	}
}

// EVERY Exec* LINE IS AN ABSOLUTE PATH, because systemd requires one for the first word and
// silently refuses the unit otherwise -- a failure that shows up as "unit not found"-shaped
// noise at promotion rather than at render. And every unit that shells out carries the profile
// on its PATH, which after [B.160] includes the doors: the picker they exec is shell, and it
// `rm`s its own trial flag.
func TestWriteUnitsExecLinesAreAbsoluteAndPathed(t *testing.T) {
	_, tools := guestWithTools(t)
	for name, text := range units() {
		for _, line := range strings.Split(text, "\n") {
			_, rest, ok := strings.Cut(line, "=")
			if !ok || !strings.HasPrefix(line, "Exec") {
				continue
			}
			rest = strings.TrimPrefix(strings.TrimPrefix(rest, "-"), "+")
			if rest == "" {
				continue
			}
			if !strings.HasPrefix(rest, "/") {
				t.Errorf("%s: %q does not start with an absolute path", name, line)
			}
		}
		if strings.Contains(text, "Exec") && !strings.Contains(text, "Environment=PATH="+tools) {
			t.Errorf("%s runs something but carries no tool profile on its PATH:\n%s", name, text)
		}
	}
}

// A DOWNGRADE INSIDE ONE BOOT MUST NOT LEAVE THE NEWER AGENT'S UNITS BEHIND ([B.160]). A
// refused release is reverted by pushing the previous bundle and restarting the agent, which is
// not a reboot -- so tmpfs does not clear it, and what would be left is a newer unit pointing at
// an older binary: this item's own defect with the ages swapped, and the harder direction to
// read, because the stale unit looks freshly written.
func TestWriteUnitsSweepsAPredecessorsUnits(t *testing.T) {
	dir, _ := guestWithTools(t)
	stale := filepath.Join(dir, "briard-from-the-future.service")
	for _, f := range []string{stale, filepath.Join(dir, "briard-also-gone.timer")} {
		if err := os.WriteFile(f, []byte("[Service]\nExecStart=/bin/false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteUnits(context.Background(), &diskExec{}); err != nil {
		t.Fatalf("WriteUnits: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a predecessor's unit survived the render: %v", err)
	}
	// AND EVERY UNIT THIS AGENT OWNS IS STILL THERE -- a sweep that took its own output would
	// be a node with no chain at all.
	for n := range units() {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("the sweep removed %s, which this agent owns: %v", n, err)
		}
	}
}

// ⚠️ AND IT TOUCHES NOTHING THAT IS NOT OURS. The sweep deletes by prefix, so the blast radius is
// the assertion: drbd-reactor's generated units and the hold's --runtime mask are named for the
// DRBD resource and live in this same directory, and removing one of those mid-promotion would
// be a far worse bug than the orphan the sweep exists to prevent.
func TestWriteUnitsSweepSparesEverythingElse(t *testing.T) {
	dir, _ := guestWithTools(t)
	keep := []string{
		"drbd-services@r0.target",      // the hold's own --runtime mask
		"drbd-promote@r0.service",      // upstream's, reactor-generated
		"briard-guest-agent.service.d", // a drop-in directory, not a unit file
		"briard-something.conf",        // not a unit suffix
		"multi-user.target.wants",      // systemd's own bookkeeping
	}
	for _, n := range keep {
		p := filepath.Join(dir, n)
		var err error
		if strings.Contains(n, ".d") || strings.HasSuffix(n, ".wants") {
			err = os.Mkdir(p, 0o755)
		} else {
			err = os.WriteFile(p, []byte("x"), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteUnits(context.Background(), &diskExec{}); err != nil {
		t.Fatalf("WriteUnits: %v", err)
	}
	for _, n := range keep {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("the sweep removed %s, which is not the agent's to remove: %v", n, err)
		}
	}
}
