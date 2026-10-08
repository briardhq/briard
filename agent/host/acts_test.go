package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"briard.io/agent/install"
	"briard.io/shared/api"
	"briard.io/shared/dashboard"
	"briard.io/shared/model"
)

// actingGuest is a fakeStatus that can also sync (an act: FsSync blocks until released) and hand
// off a dashboard code (a pull), so a test can hold an act open and see what the loop does meanwhile.
type actingGuest struct {
	fakeStatus
	release  chan struct{}
	handoffs *int32
	cycles   *int32 // the cluster reads, one per cycle; atomic, because the test reads it mid-loop
}

func (g actingGuest) Cluster(ctx context.Context, res string) (model.Cluster, error) {
	atomic.AddInt32(g.cycles, 1)
	return g.fakeStatus.Cluster(ctx, res)
}
func (g actingGuest) ReactorEvict(context.Context, bool, bool) error { return nil }
func (g actingGuest) FsSync(ctx context.Context) (string, error) {
	select {
	case <-g.release:
		return "flushed", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func (g actingGuest) DashboardHandoff(context.Context, dashboard.Handoff) error {
	atomic.AddInt32(g.handoffs, 1)
	return nil
}

// submit hands the loop a local directive and returns the channel its outcome comes back on.
func submit(local chan<- localRequest, kind string) <-chan api.DirectiveOutcome {
	resp := make(chan api.DirectiveOutcome, 1)
	local <- localRequest{d: api.Directive{ID: kind + "-1", Kind: kind}, resp: resp}
	return resp
}

func await(t *testing.T, what string, ch <-chan api.DirectiveOutcome, within time.Duration) api.DirectiveOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(within):
		t.Fatalf("%s: no outcome within %s", what, within)
		return api.DirectiveOutcome{}
	}
}

// AN ACT LEAVES THE LOOP TICKING. While a sync (an act) is held open: the cycles keep coming, a
// second act is refused busy at once, a dashboard code (a pull) is handed off beside it, and the
// act's own outcome lands once it is released. The 2026-09-01 shape -- a guest frozen inside an
// install, unnoticed until the budget expired -- cannot recur, because the tick that would notice
// is still running.
func TestAnActOffTheLoopLeavesItTicking(t *testing.T) {
	cfg := armedConfig(t, false)
	cfg.FlockName = "brave-elf" // the dashboard pull names the page's address
	cfg.acts = newActLane()
	var cycles, handoffs int32
	release := make(chan struct{})
	g := actingGuest{fakeStatus: fakeStatus{qs: model.QuorumState{Primary: true, Quorate: true}}, release: release, handoffs: &handoffs, cycles: &cycles}
	local := make(chan localRequest)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- cfg.observe(ctx, g, nil, nil, nil, nil, nil, nil, "", local, &[]api.DirectiveOutcome{}, func(string, ...any) {})
	}()

	first := submit(local, api.DirectiveSync)
	time.Sleep(20 * time.Millisecond)
	select {
	case o := <-first:
		t.Fatalf("the held sync answered before it was released: %+v", o)
	default:
	}
	before := atomic.LoadInt32(&cycles)
	time.Sleep(20 * time.Millisecond)
	if now := atomic.LoadInt32(&cycles); now <= before {
		t.Fatalf("the loop stopped ticking behind a held act: %d cycles then, %d now", before, now)
	}

	if o := await(t, "a second act", submit(local, api.DirectiveSync), time.Second); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "busy") {
		t.Fatalf("a second act while one runs = %+v, want refused busy", o)
	}
	if o := await(t, "a pull beside the act", submit(local, api.DirectiveDashboard), time.Second); o.State != api.OutcomeDone {
		t.Fatalf("dashboard beside a held act = %+v, want done", o)
	}
	if atomic.LoadInt32(&handoffs) != 1 {
		t.Fatalf("handoffs = %d, want the pull to have reached the guest", handoffs)
	}

	close(release)
	if o := await(t, "the released act", first, time.Second); o.State != api.OutcomeDone || o.Detail != "flushed" {
		t.Fatalf("the released sync = %+v, want done/flushed", o)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// THE SAFE POINT ASKS THE LANE. An armed self-update candidate is not trialled while an act runs
// off the loop -- the loop being free is no longer the proof that nothing is in flight -- and is
// trialled once the act ends.
func TestTheSafePointWaitsForAnActOffTheLoop(t *testing.T) {
	cfg := armedConfig(t, false) // armed below, once the act is in flight: an armed loop trials on its first cycle
	cfg.acts = newActLane()
	release := make(chan struct{})
	var handoffs, cycles int32
	g := actingGuest{fakeStatus: fakeStatus{qs: model.QuorumState{Primary: true, Quorate: true}}, release: release, handoffs: &handoffs, cycles: &cycles}
	local := make(chan localRequest)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var log []string
	var mu chan struct{} = make(chan struct{}, 1)
	logf := func(f string, a ...any) {
		mu <- struct{}{}
		log = append(log, strings.TrimSpace(f))
		<-mu
	}
	snapshot := func() []string { mu <- struct{}{}; defer func() { <-mu }(); return append([]string(nil), log...) }
	done := make(chan error, 1)
	go func() {
		done <- cfg.observe(ctx, g, nil, nil, nil, nil, nil, nil, "", local, &[]api.DirectiveOutcome{}, logf)
	}()

	first := submit(local, api.DirectiveSync)
	if err := os.WriteFile(filepath.Join(cfg.UpdateRunDir, "update"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if trialled(snapshot()) {
		t.Fatal("the candidate was trialled with an act in flight off the loop")
	}
	close(release)
	await(t, "the released act", first, time.Second)
	deadline := time.Now().Add(time.Second)
	for !trialled(snapshot()) {
		if time.Now().After(deadline) {
			t.Fatal("the candidate was never trialled once the act ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// heldUpgrader is an upgrader whose rescue blocks until released -- a relauncher in flight.
type heldUpgrader struct{ release <-chan struct{} }

func (u heldUpgrader) ImageUpgrade(context.Context, install.Manifest) (bool, error) {
	return false, nil
}
func (u heldUpgrader) WriteCert(context.Context, string, string) error { return nil }
func (u heldUpgrader) RescueGuest(ctx context.Context) error {
	select {
	case <-u.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A RELAUNCHER LEAVES THE LOOP TOO, and tells Run so. While a rescue is held open: the loop keeps
// ticking, the lane reports the relaunch in flight with something to wait on, another act is
// refused busy, and the wait ends when the rescue does. Run's channel-down path reads exactly
// this to wait instead of climbing the ladder on a channel the rescue is replacing.
func TestARelauncherOffTheLoopIsWhatRunWaitsFor(t *testing.T) {
	cfg := armedConfig(t, false)
	cfg.acts = newActLane()
	cfg.UpgradeBudget = time.Minute
	release := make(chan struct{})
	var handoffs, cycles int32
	g := actingGuest{fakeStatus: fakeStatus{qs: model.QuorumState{Primary: true, Quorate: true}}, release: make(chan struct{}), handoffs: &handoffs, cycles: &cycles}
	local := make(chan localRequest)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- cfg.observe(ctx, g, heldUpgrader{release: release}, nil, nil, nil, nil, nil, "", local, &[]api.DirectiveOutcome{}, func(string, ...any) {})
	}()

	rescue := submit(local, api.DirectiveRescue)
	time.Sleep(20 * time.Millisecond)
	kind, wait, ok := cfg.acts.relaunching()
	if !ok || kind != api.DirectiveRescue {
		t.Fatalf("relaunching() = (%q, %v) with a rescue held open; want the rescue", kind, ok)
	}
	before := atomic.LoadInt32(&cycles)
	time.Sleep(20 * time.Millisecond)
	if now := atomic.LoadInt32(&cycles); now <= before {
		t.Fatalf("the loop stopped ticking behind a held rescue: %d cycles then, %d now", before, now)
	}
	if o := await(t, "an act beside the rescue", submit(local, api.DirectiveSync), time.Second); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "busy") {
		t.Fatalf("an act while a rescue runs = %+v, want refused busy", o)
	}
	select {
	case <-wait:
		t.Fatal("the relaunch's done closed before the rescue ended")
	default:
	}
	close(release)
	if o := await(t, "the released rescue", rescue, time.Second); o.State != api.OutcomeDone {
		t.Fatalf("the released rescue = %+v, want done", o)
	}
	select {
	case <-wait:
	case <-time.After(time.Second):
		t.Fatal("the relaunch's done never closed after the rescue ended")
	}
	if _, _, ok := cfg.acts.relaunching(); ok {
		t.Fatal("relaunching() still reports one after the rescue ended")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
