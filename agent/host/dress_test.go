package host

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"briard.io/agent/guestagent"
)

// fakeDresser is a guest as dressGuest sees it: what it reports, what it accepts.
type fakeDresser struct {
	bundle   string
	push     bool
	staged   map[string][]byte
	activate []string
	release  string
	stageErr error
}

func (f *fakeDresser) SupportsBinPush() bool { return f.push }
func (f *fakeDresser) Bundle() string        { return f.bundle }
func (f *fakeDresser) BinStage(_ context.Context, name string, r io.Reader) error {
	if f.stageErr != nil {
		return f.stageErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if f.staged == nil {
		f.staged = map[string][]byte{}
	}
	f.staged[name] = b
	return nil
}
func (f *fakeDresser) BinActivate(_ context.Context, release string, names []string) error {
	f.release, f.activate = release, names
	return nil
}

// A committed guest tree under the layout: guest -> guest-<release>/bin/{...}.
func guestTree(t *testing.T, base, release string) {
	t.Helper()
	tree := filepath.Join(base, "guest-"+release)
	if err := os.MkdirAll(filepath.Join(tree, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range guestagent.BinNames {
		if err := os.WriteFile(filepath.Join(tree, "bin", n), []byte("#!/bin/sh\n# "+n+" "+release+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("guest-"+release, filepath.Join(base, "guest")); err != nil {
		t.Fatal(err)
	}
}

func TestDressGuestPushesWhenTheBundleDiffers(t *testing.T) {
	base := t.TempDir()
	guestTree(t, base, "v3.20260907.abc1234")
	cfg := Config{UpdateBase: base, UpdateRunDir: t.TempDir()}
	logf := func(string, ...any) {}

	// A firmware guest is dressed: every binary staged in order, then activated with the release.
	g := &fakeDresser{push: true}
	if got := cfg.dressGuest(context.Background(), g, logf); got != dressPushed {
		t.Fatalf("firmware guest: outcome %v, want pushed", got)
	}
	for _, n := range guestagent.BinNames {
		if string(g.staged[n]) != "#!/bin/sh\n# "+n+" v3.20260907.abc1234\n" {
			t.Errorf("%s staged as %q", n, g.staged[n])
		}
	}
	if g.release != "v3.20260907.abc1234" || len(g.activate) != len(guestagent.BinNames) || g.activate[len(g.activate)-1] != "briard-guest-agent" {
		t.Errorf("activated %v as %q; the guest agent must be last", g.activate, g.release)
	}

	// A guest already on the bundle is left alone.
	g = &fakeDresser{push: true, bundle: "v3.20260907.abc1234"}
	if got := cfg.dressGuest(context.Background(), g, logf); got != dressUnchanged || g.staged != nil {
		t.Errorf("dressed guest: outcome %v, staged %v", got, g.staged)
	}

	// A firmware that predates the push protocol is left on it, and says so.
	g = &fakeDresser{push: false}
	if got := cfg.dressGuest(context.Background(), g, logf); got != dressFirmware || g.staged != nil {
		t.Errorf("old firmware: outcome %v, staged %v", got, g.staged)
	}

	// A failed stage leaves the guest untouched: nothing is activated.
	g = &fakeDresser{push: true, bundle: "v3.20260901.old0000", stageErr: io.ErrUnexpectedEOF}
	if got := cfg.dressGuest(context.Background(), g, logf); got != dressFailed || g.activate != nil {
		t.Errorf("failed stage: outcome %v, activated %v", got, g.activate)
	}

	// The verdict after a push: the new id is dressed, anything else is a revert.
	if !cfg.judgeDress(&fakeDresser{bundle: "v3.20260907.abc1234"}, "v3.20260907.abc1234", logf) {
		t.Error("the committed release read as a revert")
	}
	if cfg.judgeDress(&fakeDresser{bundle: ""}, "v3.20260907.abc1234", logf) {
		t.Error("a guest back on its firmware read as dressed")
	}
}

// No committed guest tree (an install that predates the bundle): nothing to push, never a failure.
func TestDressGuestWithoutABundleIsANoop(t *testing.T) {
	cfg := Config{UpdateBase: t.TempDir(), UpdateRunDir: t.TempDir()}
	g := &fakeDresser{push: true}
	if got := cfg.dressGuest(context.Background(), g, func(string, ...any) {}); got != dressUnchanged || g.staged != nil {
		t.Errorf("outcome %v, staged %v", got, g.staged)
	}
}

// THE PERMANENT REVERT: a refused release is never pushed again; the last good tree is pushed
// instead, on this launch and every later one, until a new host commit names a different tree.
func TestDressGuestFallsBackToTheLastGoodBundleForGood(t *testing.T) {
	base := t.TempDir()
	cfg := Config{UpdateBase: base, UpdateRunDir: t.TempDir()}
	logf := func(string, ...any) {}
	good, bad, next := "v3.20260901.good0000", "v3.20260907.bad00000", "v3.20260908.next0000"

	// Release `good` was pushed and taken: recorded as the good tree.
	guestTree(t, base, good)
	if got := cfg.dressGuest(context.Background(), &fakeDresser{push: true}, logf); got != dressPushed {
		t.Fatalf("outcome %v", got)
	}
	if !cfg.judgeDress(&fakeDresser{bundle: good}, good, logf) {
		t.Fatal("a taken push read as a revert")
	}

	// The host commits `bad` (the link moves; the good tree stays on disk) and pushes it; the
	// guest comes back on `good` -- a refusal.
	os.Remove(filepath.Join(base, "guest"))
	guestTree(t, base, bad)
	g := &fakeDresser{push: true, bundle: good}
	if got := cfg.dressGuest(context.Background(), g, logf); got != dressPushed || g.release != bad {
		t.Fatalf("the committed bundle was not pushed: outcome %v, release %q", got, g.release)
	}
	if cfg.judgeDress(&fakeDresser{bundle: good}, bad, logf) {
		t.Fatal("a refused push read as dressed")
	}
	if rev, ok := cfg.layout().GuestReverted(); !ok || rev != bad {
		t.Fatalf("refusal not recorded: %q %v", rev, ok)
	}

	// From now on: a fresh launch (firmware) is dressed with `good`, never `bad`...
	g = &fakeDresser{push: true}
	if got := cfg.dressGuest(context.Background(), g, logf); got != dressPushed || g.release != good {
		t.Fatalf("a fresh launch after the refusal: outcome %v, pushed %q, want %q", got, g.release, good)
	}
	// ...a guest already on `good` is left alone...
	g = &fakeDresser{push: true, bundle: good}
	if got := cfg.dressGuest(context.Background(), g, logf); got != dressUnchanged {
		t.Fatalf("a guest on the good bundle: outcome %v", got)
	}
	// ...the pruner keeps the good tree beside the refused one...
	if _, err := cfg.layout().PruneGuestTrees(); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{good, bad} {
		if _, err := os.Stat(cfg.layout().GuestTree(r)); err != nil {
			t.Errorf("tree %s was pruned: %v", r, err)
		}
	}
	// ...and a NEW host commit is a new attempt: the refusal no longer names the committed tree.
	os.Remove(filepath.Join(base, "guest"))
	guestTree(t, base, next)
	g = &fakeDresser{push: true, bundle: good}
	if got := cfg.dressGuest(context.Background(), g, logf); got != dressPushed || g.release != next {
		t.Fatalf("after a new commit: outcome %v, pushed %q, want %q", got, g.release, next)
	}

	// With no good tree at all, a refusal leaves the guest on its firmware, and says so once.
	base2 := t.TempDir()
	cfg2 := Config{UpdateBase: base2, UpdateRunDir: t.TempDir()}
	guestTree(t, base2, bad)
	if !cfg2.judgeDress(&fakeDresser{bundle: ""}, bad, logf) == false {
		t.Fatal("expected a revert")
	}
	if got := cfg2.dressGuest(context.Background(), &fakeDresser{push: true}, logf); got != dressUnchanged {
		t.Fatalf("no good tree: outcome %v", got)
	}
}
