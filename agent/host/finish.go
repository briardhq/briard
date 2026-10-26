package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"briard.io/agent/install"
	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
	"briard.io/shared/notify"
)

// THE FIRST START FINISHES THE INSTALL. An install that laid everything down but the guest image
// -- the Store's exe, which may fetch nothing while it runs, or any install whose image never
// landed -- leaves the rest to the agent, which does it before the guest, in this order:
//
//  1. the self-update to stable, FORCED and FIRST: nothing is running yet to interrupt, the agent
//     that fetches the image is then the one its manifest was cut for (an agent months older than
//     the image it fetches is a pairing the alpha never supports), and the one run a stranger
//     watches does not run bugs already fixed. A no-op when this agent is already current; when
//     it is not, the update replaces this agent, and the next one starts here again.
//  2. the install's own image lay-down (layDownImage), against the manifest of the agent now
//     installed: the vm release it names, fetched and verified, and nothing else.
//
// The trigger is a positive read -- the image is not there -- never a first-run flag, so a node
// whose image went missing finishes the same way. A failure is a waiting state with one alert,
// never terminal, and how long it waits is what the next attempt would cost:
//
//   - the channel could not be reached (no network, the channel down, a download cut short): the
//     attempt cost nothing, and is repeated after finishRetryNet;
//   - anything else -- a release that came down whole and was refused here, or swapped in and did
//     not start and was reverted: only a newer release fixes it, and every attempt downloads it
//     again, so it is repeated after finishRetryRefused.
//
// The wait is counted from the records the update leaves -- its verdict and a revert's failure --
// never from a timer alone: a candidate that does not start ends THIS process, and the agent the
// revert starts again is a new one, as is the one a restart of the service starts. Each reads
// the records and waits out what is left.

// finishRetryNet and finishRetryRefused space the retries of a finish that failed (above).
const (
	finishRetryNet     = 2 * time.Minute
	finishRetryRefused = time.Hour
)

// frozenFetchFailed opens the frozen update scripts' verdict when they could not download the
// target's agent: the one failure the run reports before any release code runs.
const frozenFetchFailed = "could not fetch a bootstrap agent from "

// updateVerdictBound bounds the wait for the update run's verdict, and imageFetchBound the image's
// download: past either, the step has failed and is retried.
const (
	updateVerdictBound = 30 * time.Minute
	imageFetchBound    = 2 * time.Hour
)

// finishAlertKey is the alert open while the finish waits.
const finishAlertKey = "install-finish"

// finishInstall is the first start's half of the install: a no-op on a node whose image is there.
// It returns only once the image is laid down, or with ctx's error when the agent is stopped --
// by the update it started, among others.
func (cfg Config) finishInstall(ctx context.Context, local <-chan localRequest, n notify.Notifier, logf func(string, ...any)) error {
	if cfg.GuestImage == "" {
		return nil
	}
	if _, err := os.Stat(cfg.GuestImage); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	logf("install: no guest image at %s; finishing the install -- the self-update to stable, then the image", cfg.GuestImage)
	wait, cause := finishHold(selfupdate.New(cfg.UpdateBase, cfg.UpdateRunDir), time.Now())
	for {
		if wait > 0 {
			if err := cfg.finishHoldOff(ctx, local, n, wait, cause, logf); err != nil {
				return err
			}
		}
		err := cfg.finishStep(ctx, local, updateVerdictBound, "updating briard before its first start", cfg.awaitStableUpdate, logf)
		if err == nil {
			err = cfg.finishStep(ctx, local, imageFetchBound, "fetching the guest image", cfg.fetchImage, logf)
		}
		if err == nil {
			fireAlert(ctx, n, logf, notify.Alert{Key: finishAlertKey, Kind: notify.Resolved,
				Title: "Briard: the install is finished", Body: "the guest image is in place, and Briard is starting."})
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait, cause = retryAfter(err), err
	}
}

// finishHoldOff waits out one hold with the alert open, saying why and when the next try is.
func (cfg Config) finishHoldOff(ctx context.Context, local <-chan localRequest, n notify.Notifier, wait time.Duration, cause error, logf func(string, ...any)) error {
	wait = wait.Round(time.Second)
	logf("install: could not finish the install (%v); retrying in %s", cause, wait)
	body := "Briard's first start downloads the rest of the install from get.briard.io and could not reach it: " + cause.Error() +
		". It tries again in " + wait.String() + "; check this computer's internet connection."
	if retryAfter(cause) == finishRetryRefused {
		body = "Briard's first start downloaded the current release, and it could not be installed or started on this computer: " +
			cause.Error() + ". It tries again in " + wait.String() + ", in case a fixed release has been published."
	}
	fireAlert(ctx, n, logf, notify.Alert{Key: finishAlertKey, Kind: notify.Open, Severity: notify.Warning,
		Title: "Briard: waiting to finish the install", Body: body})
	wake := make(chan struct{})
	retry := time.AfterFunc(wait, func() { close(wake) })
	defer retry.Stop()
	return cfg.finishWait(ctx, local, "waiting to retry: "+cause.Error(), wake, logf)
}

// retryAfter is the wait a failed finish asks for: finishRetryNet when the channel could not be
// reached -- this agent's own fetch, or the update run's verdict saying so -- and
// finishRetryRefused for anything else.
func retryAfter(err error) time.Duration {
	s := err.Error()
	if install.Unreachable(err) || strings.Contains(s, install.UnreachablePrefix) || strings.Contains(s, frozenFetchFailed) {
		return finishRetryNet
	}
	return finishRetryRefused
}

// finishHold is what is left of the wait the update's records ask for, and why: a reverted
// release's failure asks finishRetryRefused from when it was left, a failed verdict its own
// class's wait from when it was written. Zero when neither asks for one.
func finishHold(lay selfupdate.Layout, now time.Time) (time.Duration, error) {
	var wait time.Duration
	var cause error
	if fi, err := os.Stat(lay.FailurePath()); err == nil {
		if b, err := os.ReadFile(lay.FailurePath()); err == nil {
			release, reason, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
			wait = left(finishRetryRefused, fi.ModTime(), now)
			cause = fmt.Errorf("release %s did not start on this computer and was reverted: %s", release, reason)
		}
	}
	if r, ok := lay.ReadResult(); ok && !r.OK {
		err := fmt.Errorf("the update to stable failed: %s", r.Line)
		if w := left(retryAfter(err), r.At, now); w > wait {
			wait, cause = w, err
		}
	}
	if wait == 0 {
		return 0, nil
	}
	return wait, cause
}

// left is what remains of d counted from at: never below zero, nor above d when the clock was set
// back past at.
func left(d time.Duration, at, now time.Time) time.Duration {
	return min(d, max(0, d-now.Sub(at)))
}

// finishStep runs one step OFF the loop, bounded, while the loop answers the local door: a stranger
// running `briard status` during a download gets the reason, not a CLI that hangs.
func (cfg Config) finishStep(ctx context.Context, local <-chan localRequest, bound time.Duration, why string,
	step func(context.Context, func(string, ...any)) error, logf func(string, ...any)) error {
	sctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	var err error
	done := make(chan struct{})
	go func() { err = step(sctx, logf); close(done) }()
	if werr := cfg.finishWait(ctx, local, why, done, logf); werr != nil {
		return werr
	}
	return err
}

// finishWait answers the local door until until fires or ctx ends, beating as it goes. A stop is
// the one directive taken, as in the network's wait (waitTick); anything else is refused at once
// with the reason.
func (cfg Config) finishWait(ctx context.Context, local <-chan localRequest, why string, until <-chan struct{}, logf func(string, ...any)) error {
	tick := time.NewTicker(netTick)
	defer tick.Stop()
	for {
		cfg.beat.Beat()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-until:
			return nil
		case <-tick.C:
		case req := <-local:
			if req.d.Kind == api.DirectiveStop {
				req.resp <- cfg.applyStop(req.d, logf)
				continue
			}
			logf("admin socket: refusing %s -- the install is finishing", req.d.Kind)
			req.resp <- api.DirectiveOutcome{ID: req.d.ID, State: api.OutcomeFailed,
				Detail: "briard is finishing its install: " + why}
		}
	}
}

// awaitStableUpdate starts the update run against stable -- the trigger every update takes -- and
// waits for its verdict. A run that replaces this agent stops it first, which ends ctx.
func (cfg Config) awaitStableUpdate(ctx context.Context, logf func(string, ...any)) error {
	lay := selfupdate.New(cfg.UpdateBase, cfg.UpdateRunDir)
	started := time.Now().Add(-time.Second) // as Trigger's own: a result older than this is an earlier run's
	line, err := cfg.newSelfUpdater().Trigger(ctx, install.TargetStable)
	if err != nil {
		return err
	}
	logf("install: %s", line)
	for {
		if r, ok := lay.ReadResult(); ok && r.At.After(started) {
			if !r.OK {
				return fmt.Errorf("the update to stable failed: %s", r.Line)
			}
			logf("install: the update to stable: %s", r.Line)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no verdict from the update to stable: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// fetchImage fetches the vm release the installed agent's manifest names, verified against the
// release keyring, and lays it down the install's way.
func (cfg Config) fetchImage(ctx context.Context, logf func(string, ...any)) error {
	bm, err := install.ReadManifest(selfupdate.New(cfg.UpdateBase, cfg.UpdateRunDir).ManifestPath())
	if err != nil {
		return fmt.Errorf("the installed release's manifest: %w", err)
	}
	if bm.VM == "" {
		return fmt.Errorf("the installed release %s names no guest image", bm.Version)
	}
	kr, err := selfupdate.NewKeyring(cfg.UpdateKeyring)
	if err != nil {
		return err
	}
	r := installRoots{prefix: prefixDir, state: stateDir, run: runDir}.osRoots()
	staging := filepath.Join(r.prefix, "staging")
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	logf("install: fetching the guest image %s from %s", bm.VM, cfg.ChannelURL)
	f := &install.Fetcher{BaseURL: cfg.ChannelURL, Chain: install.ChainVM, Platform: install.PlatformVM, Keyring: kr, Logf: logf}
	if err := f.FetchVerified(ctx, bm.VM, staging); err != nil {
		return err
	}
	if err := r.layDownImage(staging, logf); err != nil {
		return err
	}
	return flushInstall(ctx, logf)
}
