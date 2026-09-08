package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"briard.io/agent/guestagent"
	"briard.io/agent/guestfirmware"
	"briard.io/agent/selfupdate"
	"briard.io/shared/notify"
)

// DRESSING THE GUEST ([B.86j], re-cut by [B.138] and [B.139]). Every briard binary the guest runs
// rides the HOST bundle: the committed tree at <base>/guest ->
// guest-<release>/bin/{briard-dashboard,briard-reverse-proxy,briard-guest-agent}. The image bakes
// ONE binary, briard-guest-firmware -- the push protocol alone, which is what receives the first
// push; the guest AGENT has no baked copy either. The guest's overlay is disposable, so every
// launch starts as firmware with no door and no agent at all; the host compares the bundle the
// guest reports in its handshake with the tree it holds and pushes when they differ -- at
// bring-up (BEFORE rejoin, so nothing can promote an undressed node), after a host commit, after
// any guest relaunch. A convergence
// the committed HOST agent performs, the same way it pushes the hostname and the addresses: not a
// third update mechanism.
//
// THE SET COMMITS AS ONE, OR NOT AT ALL (owner, 2026-09-08). The push is stage, prove, arm:
// every file staged, then `bin.test` runs each staged copy's --test-launch in the guest -- the
// cheap gate; a failure names the binary, the guest discards the whole staged set, nothing is
// armed, and the dress is REFUSED here and now -- then `bin.activate` arms every name and
// restarts the guest agent's unit alone. That agent's start is the verdict on the rest: where a
// door is running (the primary; a single node is always the primary, which is where there is no
// peer to fall back on) it try-restarts it onto the staged copy and reads READY or failure; on a
// secondary the doors are not running and the cheap gate was the only one. Only a passing
// verdict opens the port, and its ExecStartPost commits the set plus the release id together.
// A failed door has already reverted itself by its own auto-restart (flag consumed), one start
// out of its budget -- a failed upgrade never demotes; the refused agent exits without opening
// the port, the committed agent comes back, discards the staged set and puts both doors on the
// committed files. (guest-image/pivot.nix, agent/guestfirmware/bin.go.)
//
// The host's own trial gate stays "the agent started"; a host update never waits for its guest
// to be dressed ([V3.32]: a host update may be what fixes the guest). The host learns the outcome
// from the next handshake: the new id means the set took, the old one means it was refused --
// and because the port opens only after the verdict, there is no handshake in between.
//
// THE REVERT IS PERMANENT, and the host is what makes it so (owner, 2026-09-07). The guest's own
// fallback only lasts until its next launch -- a fresh overlay knows nothing, so the next boot
// would trial the same bad bundle again. So the host keeps two facts beside the committed tree:
// `guest.good`, a link to the last tree a dress was JUDGED to have taken (the guest came back
// reporting it), and `guest.reverted`, the release id of a tree the guest refused -- at the
// cheap gate or at the trial, the record is the same. A refused release is never pushed again;
// the good tree is pushed instead, on every launch, until a new host commit brings a new tree.
// Host and guest are then on different releases, and the status line says so (GuestBundle names
// the good tree) -- the ordinary transient every rollout passes through, held for one release
// rather than papered over. No joint host+guest commit: that would have to answer what a host
// does when its guest is down, and the answer is this: it commits its own bundle and converges
// the guest when it can. Since the guest commits as a whole, a host tree and the guest's set
// always correspond.

// dresser is what dressGuest needs of a guest: the handshake facts and the three push verbs.
type dresser interface {
	SupportsBinPush() bool
	Bundle() string
	BinStage(ctx context.Context, name string, r io.Reader) error
	BinTest(ctx context.Context, names []string) error
	BinActivate(ctx context.Context, release string, names []string) error
}

// dressOutcome is what one dressing attempt concluded, for the log and the status line.
type dressOutcome int

const (
	dressUnchanged dressOutcome = iota // the guest already runs the bundle the host wants it on (or there is none)
	dressPushed                        // the set was staged, proven and activated; the channel is about to drop
	dressRefused                       // a staged copy failed its test launch; nothing armed, the guest untouched, the release recorded as refused
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
// dressRefused when the guest's cheap gate rejected a staged copy (recorded by the caller). It
// never fails bring-up: a guest that cannot be dressed is a guest on what it ran before, and the
// mismatch stays visible in the status line for the cloud to act on.
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
		logf("guest bundle: the guest runs its firmware and cannot be dressed (no bin.stage/bin.test/bin.activate); host holds %s", want)
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
	for _, name := range guestfirmware.BinNames {
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
	if err := g.BinTest(ctx, guestfirmware.BinNames); err != nil {
		logf("guest bundle: PUSH REFUSED -- %s's set failed the guest's test launch (%v); nothing was armed, the guest stays on %s, and %s will not be pushed again", want, err, orFirmware(have), want)
		return dressRefused
	}
	if err := g.BinActivate(ctx, want, guestfirmware.BinNames); err != nil {
		logf("guest bundle: activating %s failed (%v); the guest stays on %s", want, err, orFirmware(have))
		return dressFailed
	}
	logf("guest bundle: %s staged, proven and activated in %s; the guest agent restarts on it and its start is the verdict", want, time.Since(started).Round(time.Millisecond))
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
// else means the trial was refused: a door failed its real launch or the agent never reached
// READY, and the guest's own pivot fell back on the whole set. The refusal is recorded durably so
// the release is never pushed again, and said once here; the observe loop raises the alert, which
// is where a notifier is in scope.
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
	cfg.recordRefusal(pushed, logf)
	logf("guest bundle: PUSH REVERTED -- the guest came back on %s, not %s: the trial did not pass (a door failed its real launch, or the pushed agent never reached READY) and the guest's own pivot fell back on the whole set; %s will not be pushed again, and the guest is dressed with the last good bundle from here on", orFirmware(have), pushed, pushed)
	return false
}

// recordRefusal writes the durable record that makes a refusal permanent ([B.86j]).
func (cfg Config) recordRefusal(release string, logf func(string, ...any)) {
	if err := cfg.layout().MarkGuestReverted(release); err != nil {
		logf("guest bundle: recording the refusal of %s failed: %v", release, err)
	}
}

// dressAndRejoin runs one dressing attempt and, when a set was pushed, waits for the guest
// agent to come back on the pushed (or reverted) binary, judges the handshake, and -- if the
// push was refused, at the cheap gate or at the trial -- dresses the guest with the last good
// bundle in the same breath, so the house never runs its firmware for want of a good bundle it
// has. The returned client is the one to keep using; after a push it is a new one.
func (cfg Config) dressAndRejoin(ctx context.Context, client *guestagent.Client, logf func(string, ...any)) (*guestagent.Client, error) {
	for attempt := 0; attempt < 2; attempt++ {
		want, _ := cfg.guestWant()
		switch cfg.dressGuest(ctx, client, logf) {
		case dressRefused:
			// The guest discarded the set and is untouched; the channel is intact. Record it,
			// and the next iteration wants the good tree (if any) and pushes it.
			cfg.recordRefusal(want, logf)
			continue
		case dressPushed:
		default:
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
		// Refused at the trial: the next iteration wants the good tree (if any) and pushes it.
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
	cause := fmt.Errorf("the guest refused %s's bundle (a staged copy failed its test launch, or the trial did not pass); the guest runs %s and the node will not read as converged until the next host release", rev, orFirmware(good))
	escalate(ctx, n, logf, "this node", "guest bundle push", rev, cause)
}
