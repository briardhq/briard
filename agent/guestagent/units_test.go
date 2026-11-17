package guestagent

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"briard.io/shared/chain"
)

// diskExec is fakeExec with a real WriteFile. The thing under test is a file systemd can load,
// so a fake that keeps writes in a map would let every assertion below pass over a renderer that
// wrote nothing at all.
type diskExec struct {
	fakeExec
	wrote []string // every path WriteFile was CALLED with, mask or not
}

func (d *diskExec) WriteFile(path string, data []byte) error {
	d.wrote = append(d.wrote, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// wroteTo reports whether the renderer attempted this path, the only way to see the mask guard
// work: os.WriteFile through a symlink to /dev/null opens the target, so it succeeds, writes
// nothing and leaves the symlink standing -- the on-disk state is identical with or without the
// guard, and asserting on the file alone would be vacuous.
func (d *diskExec) wroteTo(path string) bool {
	for _, p := range d.wrote {
		if p == path {
			return true
		}
	}
	return false
}

// guestWithTools points the renderer at a temporary unit directory and a temporary tool profile,
// and returns both, so every assertion is about a real file on disk: a unit systemd can load.
func guestWithTools(t *testing.T) (unitDir, tools string) {
	t.Helper()
	unitDir = t.TempDir()
	tools = filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	// The interpreter the scripts name: the profile WriteUnits checks for.
	if err := os.WriteFile(filepath.Join(tools, "bash"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIARD_UNIT_DIR", unitDir)
	t.Setenv("BRIARD_SCRIPT_DIR", t.TempDir())
	t.Setenv("BRIARD_TOOLS_BIN", tools)
	t.Setenv("BRIARD_BIN_DIR", "/var/lib/briard-bin")
	return unitDir, tools
}

// The unit exists, and it is the one the host is about to start. The property is "the unit can
// never be older than the binary that wrote it", so this binary's start must produce a
// briard-node-storage.service naming this binary's committed path and this image's tool profile --
// the two halves that, out of step, crash-loop the node.
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
	// No [Install]: this unit runs when the host says so and never at boot. A rendered [Install]
	// would be silently inert here (nothing enables it) and wrong the moment anything did.
	if strings.Contains(got, "[Install]") {
		t.Errorf("node storage must not be enablable:\n%s", got)
	}
}

// The scripts ride with the units: every embedded one is laid at the script directory, byte for
// byte, and made executable, and every script a unit runs is one of them. A unit naming a script
// the agent does not lay is the image-skew defect again, one level down.
func TestWriteUnitsLaysTheScripts(t *testing.T) {
	dir, _ := guestWithTools(t)
	sdir := scriptDir()
	f := &diskExec{}
	if err := WriteUnits(context.Background(), f); err != nil {
		t.Fatalf("WriteUnits: %v", err)
	}
	ents, err := scripts.ReadDir("scripts")
	if err != nil || len(ents) == 0 {
		t.Fatalf("no embedded scripts: %v", err)
	}
	var chmodded []string
	for _, r := range f.runs {
		if r[0] == "chmod" && r[1] == "0755" {
			chmodded = r[2:]
		}
	}
	for _, e := range ents {
		want, _ := scripts.ReadFile("scripts/" + e.Name())
		p := filepath.Join(sdir, e.Name())
		got, err := os.ReadFile(p)
		if err != nil || string(got) != string(want) {
			t.Errorf("%s was not laid as embedded: %v", e.Name(), err)
		}
		if !slices.Contains(chmodded, p) {
			t.Errorf("%s was not made executable (chmod ran on %v)", e.Name(), chmodded)
		}
	}
	units, _ := os.ReadDir(dir)
	named := 0
	for _, u := range units {
		b, _ := os.ReadFile(filepath.Join(dir, u.Name()))
		for _, m := range regexp.MustCompile(regexp.QuoteMeta(sdir)+`/([a-z-]+)`).FindAllStringSubmatch(string(b), -1) {
			named++
			if _, err := os.Stat(filepath.Join(sdir, m[1])); err != nil {
				t.Errorf("%s runs %s, which the agent does not lay", u.Name(), m[1])
			}
		}
	}
	if named == 0 {
		t.Fatal("the fixture is vacuous: no unit names a script")
	}
}

// A profile from before the scripts has no interpreter at the path their first line names, and
// the agent refuses it as it refuses a missing profile, rather than laying scripts that cannot
// exec on the promotion path.
func TestWriteUnitsRefusesAProfileWithNoBash(t *testing.T) {
	_, tools := guestWithTools(t)
	if err := os.Remove(filepath.Join(tools, "bash")); err != nil {
		t.Fatal(err)
	}
	err := WriteUnits(context.Background(), &diskExec{})
	if err == nil || !strings.Contains(err.Error(), "no bash") {
		t.Fatalf("rendered against a profile with no bash: %v", err)
	}
}

// A unit written and not reloaded is a unit systemctl says does not exist -- the same symptom as a
// unit nobody wrote. systemd reads unit files at load, so the reload is not housekeeping, and its
// absence would pass every content assertion above.
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

// An image that predates the tool profile has none, and the agent must refuse to serve on it
// rather than write units whose PATH resolves to nothing. That refusal makes a too-new agent safe:
// the trial fails, the picker restores the committed binary, and the node keeps running the
// release it had instead of promoting into units it cannot support.
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
	// Nor may it have written anything first: a half-rendered set that then fails is worse
	// than none: the next start's reload would load whatever landed before the error.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Errorf("refusing left %d file(s) behind in %s", len(ents), dir)
	}
}

// A tool profile that is a file, or a dangling symlink, is not a tool profile. Both are how a
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

// The render is idempotent, because it runs on every start of the agent -- a crash-restart, a
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

// The chain is ordered, as a dependency rather than a preference: each member Requires= and After=
// the previous one, so the reverse proxy starts only once briard-vip holds the address its names
// resolve to (shared/chain). Got wrong, a lone node would still start them all (the target Wants
// them), in whatever order systemd liked -- a reverse proxy up before its address.
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
	// The lone node's target carries the identical list, so a lone node and a flock run one
	// chain and the members cannot tell which target started them.
	tgt := u[chain.Target]
	for _, k := range []string{"Wants=", "After="} {
		if !strings.Contains(tgt, k+strings.Join(members, " ")+"\n") {
			t.Errorf("%s is missing %s over the exact member list:\n%s", chain.Target, k, tgt)
		}
	}
}

// Every chain member hands the resource on when it gives up. OnFailure= is the state hook that
// fires on the transition into failed, and the start limit decides when that is; a member rendered
// without either would crash quietly forever while the household had no apps running, the shape
// the budget exists to end.
// And every member restarts directly (RestartMode=direct): without it a member enters failed on
// its first failure, OnFailure= fires then, and the budget is never spent.
func TestWriteUnitsEveryMemberCanHandTheResourceOn(t *testing.T) {
	guestWithTools(t)
	u := units()
	for _, m := range chain.Members() {
		// Whole lines, not substrings: "StartLimitBurst=5" is a prefix of "StartLimitBurst=50", so
		// a substring check passes a budget ten times the intended one -- a reverse proxy that
		// keeps crash-looping instead of handing the house to a peer.
		for _, want := range []string{
			"OnFailure=" + holdUnit + "\n",
			"OnFailureJobMode=replace-irreversibly\n",
			"Restart=on-failure\n",
			"RestartMode=direct\n",
			"StartLimitIntervalSec=300\n",
			"StartLimitBurst=5\n",
		} {
			if !strings.Contains(u[m], want) {
				t.Errorf("%s is missing %q:\n%s", m, want, u[m])
			}
		}
	}
	// The hold itself is not a member: a hold that fired its own OnFailure would loop.
	if strings.Contains(u[holdUnit], "OnFailure=") {
		t.Errorf("the hold must not hand the resource on to itself:\n%s", u[holdUnit])
	}
	// Nor is the renewal timer's systemd service: a renewal that fails must never be able to
	// demote a serving node.
	if strings.Contains(u[vipRenewUnit], "OnFailure=") || strings.Contains(u[vipRenewUnit], "PartOf="+chain.Target) {
		t.Errorf("the lease renewal must not be able to demote a serving node:\n%s", u[vipRenewUnit])
	}
}

// The hold stops the whole chain on a lone node, in reverse. `systemctl stop` of a target returns
// as soon as the target is down -- measured, with the volume still mounted and the shim then
// escalating to a reboot -- so the stop has to name every member. This is the one place the
// agent's knowledge of the chain reaches an image-side tool, as arguments, so a wrong list or
// direction is silent until a member gives up in the field.
func TestWriteUnitsHoldStopsEveryMemberInReverse(t *testing.T) {
	guestWithTools(t)
	hold := units()[holdUnit]
	members := chain.Members()
	want := "briard-hold-stop " + chain.Target + " " + strings.Join(reversed(members), " ")
	if !strings.Contains(hold, want) {
		t.Errorf("the hold's stop does not name the chain in reverse; want %q in:\n%s", want, hold)
	}
	// The release resets the start limit of every member, forward order being irrelevant but
	// completeness not: a member left inside its StartLimitIntervalSec when the hold ends is one
	// systemd refuses to start on the next promotion, which would pin the hold to that window.
	if !strings.Contains(hold, "reset-failed "+strings.Join(members, " ")) {
		t.Errorf("the hold does not clear every member's start limit:\n%s", hold)
	}
	// The lone node's restart is handed the target it must start. The step takes `$1`; passed
	// nothing, `set -u` fails it with "unbound variable" on an ExecStopPost the unit deliberately
	// marks `-`, so systemd swallows it and the lone node never comes back. Hold-and-restart is
	// the lone node's whole answer to a member giving up; a silent no-op there is a dead house.
	if !strings.Contains(hold, "briard-hold-restart "+chain.Target+"\n") {
		t.Errorf("the hold's restart is not told which target to start:\n%s", hold)
	}
}

// The hold length is a constant with a test override: the contract rigs drive the full lifecycle
// and cannot wait five minutes.
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

// Every Exec* line is an absolute path: systemd requires one for the first word and silently
// refuses the unit otherwise, which shows up as "unit not found"-shaped noise at promotion rather
// than at render. And every unit that shells out carries the profile on its PATH, the web units
// included: the picker they exec is shell, and it `rm`s its own trial flag.
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

// A downgrade inside one boot must not leave the newer agent's units behind. A refused release is
// reverted by pushing the previous bundle and restarting the agent, not a reboot, so tmpfs does
// not clear it: a newer unit would point at an older binary -- the defect the render removes, with
// the ages swapped, and harder to read because the stale unit looks freshly written.
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
	// Every unit this agent owns is still there: a sweep that took its own output would
	// be a node with no chain at all.
	for n := range units() {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("the sweep removed %s, which this agent owns: %v", n, err)
		}
	}
}

// ⚠️ The sweep touches nothing that is not ours. It deletes by prefix, so the blast radius is
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

// A mask must survive a render. `handover -keep-masked` exists so a machine about to reboot for
// its own upgrade cannot take the house back before anyone has verified its new generation, and
// the agent restarts on that path: a render that overwrote the mask would end the refusal at the
// next agent start, precisely when it is needed. Mask and unit are one path because systemd
// defines a mask as a symlink to /dev/null at the unit's own location, which the renderer writes.
func TestWriteUnitsLeavesAMaskAlone(t *testing.T) {
	dir, _ := guestWithTools(t)
	f := &diskExec{}
	if err := WriteUnits(context.Background(), f); err != nil {
		t.Fatalf("WriteUnits: %v", err)
	}
	if err := MaskRendered(context.Background(), f, chain.Target); err != nil {
		t.Fatalf("MaskRendered: %v", err)
	}
	// MaskRendered clears the file so `systemctl mask` is not refused over it ("File
	// '/run/systemd/system/briard-chain.target' already exists"). The fake systemctl writes no
	// symlink, so stand one in as systemd would.
	p := filepath.Join(dir, chain.Target)
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("masking did not clear the rendered file first: %v", err)
	}
	if err := os.Symlink(os.DevNull, p); err != nil {
		t.Fatal(err)
	}
	f.wrote = nil // only the render AFTER the mask is the one under test
	if err := WriteUnits(context.Background(), f); err != nil {
		t.Fatalf("WriteUnits over a mask: %v", err)
	}
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the render replaced a mask with a unit file, un-refusing a node that was told to stay out (mode=%v, err=%v)", fi.Mode(), err)
	}
	// And it did not even try. See wroteTo: a write through the mask would have succeeded
	// silently into /dev/null, so the surviving symlink above proves nothing on its own.
	if f.wroteTo(p) {
		t.Errorf("the render wrote through the mask at %s -- into /dev/null, which looks like success", p)
	}
	// The rest of the chain is still rendered: a mask on one unit must not stop the others
	// being written, or a masked target would take the whole node's units with it.
	if _, err := os.Stat(filepath.Join(dir, vipUnit)); err != nil {
		t.Errorf("a mask on %s stopped %s being rendered: %v", chain.Target, vipUnit, err)
	}
}

// Unmasking puts the unit back. Dropping the symlink leaves no unit file, since every render while
// the mask stood skipped it, so an unmask that did not re-render would hand `systemctl start` a
// unit that does not exist -- the crash-loop by another road. The fake systemctl models the one
// thing that matters, `unmask --runtime` removing the symlink: a fake that did nothing would leave
// the mask, the re-render would skip it for the right reason, and the test would assert nothing.
func TestUnmaskRenderedRestoresTheUnit(t *testing.T) {
	dir, _ := guestWithTools(t)
	p := filepath.Join(dir, chain.Target)
	f := &diskExec{}
	f.runFn = func(name string, args []string) ([]byte, error) {
		if name == "systemctl" && len(args) > 1 && args[0] == "unmask" {
			if err := os.Remove(filepath.Join(dir, args[len(args)-1])); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}
		return nil, nil
	}
	if err := WriteUnits(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(os.DevNull, p); err != nil {
		t.Fatal(err)
	}
	if err := UnmaskRendered(context.Background(), f, chain.Target); err != nil {
		t.Fatalf("UnmaskRendered: %v", err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("unmasking left no unit for `systemctl start` to find: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is still a mask after unmasking", chain.Target)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Wants="+strings.Join(chain.Members(), " ")) {
		t.Errorf("the restored target is not the rendered one:\n%s", b)
	}
}
