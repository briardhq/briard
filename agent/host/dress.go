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
	"briard.io/shared/notify"
)

// DRESSING THE GUEST ([B.86j]). Every briard binary the guest runs rides the HOST bundle: the
// committed tree at <base>/guest -> guest-<release>/bin/{briard-guest-agent,briard-reverse-proxy}.
// The image bakes a FIRMWARE copy of each, and the guest's overlay is disposable, so every launch
// starts as firmware; the host compares the bundle the guest reports in its handshake with the
// tree it holds and pushes when they differ -- at bring-up, after a host commit, after any guest
// relaunch. A convergence the committed agent performs, the same way it pushes the hostname and
// the addresses: not a third update mechanism.
//
// The pivot commits the FILES, not the delivery. The host's own trial gate stays "the agent
// started"; a host update never waits for its guest to be dressed ([V3.32]: a host update may be
// what fixes the guest). What the guest does with the push is its own frozen pivot's business
// (guest-image/pivot.nix): trial -> pushed -> baked, READY at listen, a pushed binary that will
// not start reverts on its next start with no channel and no timer. The host learns the outcome
// from the next handshake: the new id means done, the old one means the trial reverted.
//
// THE REVERT IS PERMANENT, and the host is what makes it so (owner, 2026-09-07). The guest's own
// fallback only lasts until its next launch -- a fresh overlay knows nothing, so the next boot
// would trial the same bad bundle again and, failing, fall back to the image's FIRMWARE, which
// may be months older than what the guest ran yesterday. So the host keeps two facts beside the
// committed tree: `guest.good`, a link to the last tree a dress was JUDGED to have taken (the
// guest came back reporting it), and `guest.reverted`, the release id of a tree the guest refused.
// A refused release is never pushed again; the good tree is pushed instead, on every launch,
// until a new host commit brings a new tree. Host and guest are then on different releases, and
// the status line says so (GuestBundle names the good tree) -- the ordinary transient every
// rollout passes through, held for one release rather than papered over. No joint host+guest
// commit: that would have to answer what a host does when its guest is down, and the answer is
// this: it commits its own bundle and converges the guest when it can.

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
	dressUnchanged dressOutcome = iota // the guest already runs the bundle the host wants it on (or there is none)
	dressPushed                        // the bundle was staged and activated; the channel is about to drop
	dressFirmware                      // the guest predates the push protocol; left on its firmware
	dressFailed                        // a push verb failed; the guest is untouched and serving
)

func (cfg Config) layout() selfupdate.Layout {
	return selfupdate.New(cfg.UpdateBase, cfg.UpdateRunDir)
}

// guestBundleRelease is the bundle this host holds for its guest: the committed tree's release
// id, "" when the install predates the bundle (nothing to push; the firmware serves).
func (cfg Config) guestBundleRelease() string { return cfg.layout().CommittedGuestRelease() }

// guestWant is the tree the guest should be running: the committed one, unless the guest refused
// it -- then the last good one, if there is one. The second return says whether the committed
// release was skipped as reverted, so the caller can say so.
func (cfg Config) guestWant() (release string, skippedReverted bool) {
	l := cfg.layout()
	want := l.CommittedGuestRelease()
	if want == "" {
		return "", false
	}
	if rev, ok := l.GuestReverted(); ok && rev == want {
		return l.GoodGuestRelease(), true
	}
	return want, false
}

// dressGuest compares what the guest runs with what the host wants it on and pushes when they
// differ. It returns dressPushed when the activation was accepted -- the guest agent's unit is
// restarting, so the caller must reconnect and handshake again before using the channel -- and
// never fails bring-up: a guest that cannot be dressed is a guest on its firmware, which serves,
// and the mismatch stays visible in the status line for the cloud to act on.
func (cfg Config) dressGuest(ctx context.Context, g dresser, logf func(string, ...any)) dressOutcome {
	want, skipped := cfg.guestWant()
	have := g.Bundle()
	if skipped {
		committed := cfg.guestBundleRelease()
		if want == "" {
			if have == "" {
				logf("guest bundle: %s reverted before and there is no earlier good bundle; the guest stays on its firmware", committed)
			}
			return dressUnchanged
		}
		if have != want {
			logf("guest bundle: %s reverted before; dressing the guest with the last good bundle %s instead", committed, want)
		}
	}
	if want == "" || have == want {
		return dressUnchanged
	}
	if !g.SupportsBinPush() {
		logf("guest bundle: the guest runs its firmware and cannot be dressed (no bin.stage/bin.activate); host holds %s", want)
		return dressFirmware
	}
	tree := cfg.layout().GuestTree(want)
	if !skipped {
		if have == "" {
			logf("guest bundle: the guest runs its firmware; dressing it with %s", want)
		} else {
			logf("guest bundle: the guest runs %s, the host holds %s; dressing it", have, want)
		}
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

// judgeDress reads the handshake AFTER a push of `pushed`: that release means the guest took it
// -- recorded as the good tree, so a later refusal has something to fall back to -- and anything
// else means the trial reverted: the pushed binary never said READY and the guest's own pivot
// fell back. The refusal is recorded durably so the release is never pushed again, and said once
// here; the observe loop raises the alert, which is where a notifier is in scope.
func (cfg Config) judgeDress(g dresser, pushed string, logf func(string, ...any)) (dressed bool) {
	have := g.Bundle()
	l := cfg.layout()
	if have == pushed {
		if err := l.MarkGuestGood(pushed); err != nil {
			logf("guest bundle: the guest runs %s (dressed), but recording it as the good bundle failed: %v", pushed, err)
		} else {
			logf("guest bundle: the guest runs %s (dressed)", pushed)
		}
		return true
	}
	if err := l.MarkGuestReverted(pushed); err != nil {
		logf("guest bundle: recording the refusal of %s failed: %v", pushed, err)
	}
	logf("guest bundle: PUSH REVERTED -- the guest came back on %s, not %s: the pushed binary never reached READY and its own pivot fell back; %s will not be pushed again, and the guest is dressed with the last good bundle from here on", orFirmware(have), pushed, pushed)
	return false
}

// dressAndRejoin runs one dressing attempt and, when a bundle was pushed, waits for the guest
// agent to come back on the pushed (or reverted) binary, judges the handshake, and -- if the
// push was refused -- dresses the guest with the last good bundle in the same breath, so the
// house never runs its firmware for want of a good bundle it has. The returned client is the
// one to keep using; after a push it is a new one.
func (cfg Config) dressAndRejoin(ctx context.Context, client *guestagent.Client, logf func(string, ...any)) (*guestagent.Client, error) {
	for attempt := 0; attempt < 2; attempt++ {
		want, _ := cfg.guestWant()
		if cfg.dressGuest(ctx, client, logf) != dressPushed {
			return client, nil
		}
		_ = client.Close()
		nc, err := reconnect(ctx, cfg.ControlSock, logf)
		if err != nil {
			return nil, fmt.Errorf("guest bundle: the guest did not come back after the push: %w", err)
		}
		client = nc
		if cfg.judgeDress(client, want, logf) {
			return client, nil
		}
		// Refused: the next iteration wants the good tree (if any) and pushes it.
	}
	return client, nil
}

// alertGuestRevert raises the alert for a refused bundle, once per release id, from the observe
// loop. The record outlives the alert: the refusal stays in force until a new host commit.
func (cfg Config) alertGuestRevert(ctx context.Context, n notify.Notifier, logf func(string, ...any), alerted *string) {
	rev, ok := cfg.layout().GuestReverted()
	if !ok || rev == *alerted {
		return
	}
	*alerted = rev
	good := cfg.layout().GoodGuestRelease()
	cause := fmt.Errorf("the guest refused %s's bundle (its pushed binary never reached READY); the guest runs %s and the node will not read as converged until the next host release", rev, orFirmware(good))
	escalate(ctx, n, logf, "this node", "guest bundle push", rev, cause)
}
