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
	if !cfg.judgeDress(&fakeDresser{bundle: "v3.20260907.abc1234"}, logf) {
		t.Error("the committed release read as a revert")
	}
	if cfg.judgeDress(&fakeDresser{bundle: ""}, logf) {
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
