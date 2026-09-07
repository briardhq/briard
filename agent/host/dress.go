package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/agent/selfupdate"
)

// DRESSING THE GUEST ([B.86j]). Every briard binary the guest runs rides the HOST bundle: the
// committed tree at <base>/guest -> guest-<release>/bin/{briard-guest-agent,briard-reverse-proxy}.
// The image bakes a FIRMWARE copy of each, and the guest's overlay is disposable, so every boot
// starts as firmware; the host compares the bundle the guest reports in its handshake with the
// tree it holds and pushes when they differ -- at bring-up, after a host commit, after any guest
// restart. A convergence the committed agent performs, the same way it pushes the hostname and
// the addresses: not a third update mechanism.
//
// The pivot commits the FILES, not the delivery. The host's own trial gate stays "the agent
// started"; a host update never waits for its guest to be dressed ([V3.32]: a host update may be
// what fixes the guest). What the guest does with the push is its own frozen pivot's business
// (guest-image/pivot.nix): trial -> pushed -> baked, READY at listen, a pushed binary that will
// not start reverts on its next start with no channel and no timer. The host learns the outcome
// from the next handshake: the new id means done, the old one means the trial reverted.

// dresser is what dressGuest needs of a guest: the handshake facts and the two push verbs.
type dresser interface {
	SupportsBinPush() bool
	Bundle() string
	BinStage(ctx context.Context, name string, r io.Reader) error
	BinActivate(ctx context.Context, release string, names []string) error
}

// dressOutcome is what one dressing attempt concluded, for the log and the status line.
type dressOutcome int

const (
	dressUnchanged dressOutcome = iota // the guest already runs the committed bundle (or there is none to push)
	dressPushed                        // the bundle was staged and activated; the channel is about to drop
	dressFirmware                      // the guest predates the push protocol; left on its firmware
	dressFailed                        // a push verb failed; the guest is untouched and serving
)

// guestBundleRelease is the bundle this host holds for its guest: the committed tree's release
// id, "" when the install predates the bundle (nothing to push; the firmware serves).
func (cfg Config) guestBundleRelease() string {
	return selfupdate.New(cfg.UpdateBase, cfg.UpdateRunDir).CommittedGuestRelease()
}

// dressGuest compares what the guest runs with what the host holds and pushes when they
// differ. It returns dressPushed when the activation was accepted -- the guest agent's unit
// is restarting, so the caller must reconnect and handshake again before using the channel --
// and never fails bring-up: a guest that cannot be dressed is a guest on its firmware, which
// serves, and the mismatch stays visible in the status line for the cloud to act on.
func (cfg Config) dressGuest(ctx context.Context, g dresser, logf func(string, ...any)) dressOutcome {
	want := cfg.guestBundleRelease()
	if want == "" {
		return dressUnchanged
	}
	have := g.Bundle()
	if have == want {
		return dressUnchanged
	}
	if !g.SupportsBinPush() {
		logf("guest bundle: the guest runs its firmware and cannot be dressed (no bin.stage/bin.activate); host holds %s", want)
		return dressFirmware
	}
	tree := selfupdate.New(cfg.UpdateBase, cfg.UpdateRunDir).GuestTree(want)
	if have == "" {
		logf("guest bundle: the guest runs its firmware; dressing it with %s", want)
	} else {
		logf("guest bundle: the guest runs %s, the host holds %s; dressing it", have, want)
	}
	started := time.Now()
	for _, name := range guestagent.BinNames {
		f, err := os.Open(filepath.Join(tree, "bin", name))
		if err != nil {
			logf("guest bundle: %s has no %s (%v); the guest stays on %s", want, name, err, orFirmware(have))
			return dressFailed
		}
		err = g.BinStage(ctx, name, f)
		f.Close()
		if err != nil {
			logf("guest bundle: staging %s failed (%v); the guest stays on %s", name, err, orFirmware(have))
			return dressFailed
		}
	}
	if err := g.BinActivate(ctx, want, guestagent.BinNames); err != nil {
		logf("guest bundle: activating %s failed (%v); the guest stays on %s", want, err, orFirmware(have))
		return dressFailed
	}
	logf("guest bundle: %s staged and activated in %s; the guest agent restarts on it", want, time.Since(started).Round(time.Millisecond))
	return dressPushed
}

func orFirmware(bundle string) string {
	if bundle == "" {
		return "its firmware"
	}
	return bundle
}

// judgeDress reads the handshake AFTER a push: the committed release means the guest is dressed,
// anything else means the trial reverted (the pushed binary never said READY) and the guest is
// back on what it ran before -- serving, but not on this release. Said once, loudly, because the
// cloud's convergence check is the only other thing that will notice.
func (cfg Config) judgeDress(g dresser, logf func(string, ...any)) (dressed bool) {
	want := cfg.guestBundleRelease()
	have := g.Bundle()
	if have == want {
		logf("guest bundle: the guest runs %s (dressed)", want)
		return true
	}
	logf("guest bundle: PUSH REVERTED -- the guest came back on %s, not %s: the pushed binary never reached READY and its own pivot fell back; the guest serves on what it ran before, and this node will not read as converged until a working bundle lands", orFirmware(have), want)
	return false
}

// String for the status line.
func (o dressOutcome) String() string {
	switch o {
	case dressPushed:
		return "pushed"
	case dressFirmware:
		return "firmware"
	case dressFailed:
		return "failed"
	}
	return "unchanged"
}

// dressAndRejoin runs one dressing attempt and, when the bundle was pushed, waits for the
// guest agent to come back on the pushed (or reverted) binary and reads the outcome from its
// handshake. The returned client is the one to keep using; on a push it is a new one.
func (cfg Config) dressAndRejoin(ctx context.Context, client *guestagent.Client, logf func(string, ...any)) (*guestagent.Client, error) {
	if cfg.dressGuest(ctx, client, logf) != dressPushed {
		return client, nil
	}
	_ = client.Close()
	nc, err := reconnect(ctx, cfg.ControlSock, logf)
	if err != nil {
		return nil, fmt.Errorf("guest bundle: the guest did not come back after the push: %w", err)
	}
	cfg.judgeDress(nc, logf)
	return nc, nil
}
