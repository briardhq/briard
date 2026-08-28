package host

import (
	"errors"
	"sync"

	"briard.io/shared/api"
	"context"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func quietf(string, ...any) {}

// A beat with a fast tick and a counting send, for the timing assertions below.
func countingBeat(every time.Duration) (*beat, *atomic.Int64) {
	var n atomic.Int64
	return &beat{every: every, send: func() error { n.Add(1); return nil }, logf: quietf}, &n
}

// Outside systemd there is no watchdog to feed, and the agent must run exactly as before --
// no goroutines, no sends, no caller needing to ask which kind of beat it holds.
func TestNewBeatDisabledWithoutWatchdogEnv(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	if b := newBeat(quietf); b != nil {
		t.Fatalf("no WATCHDOG_USEC should mean no watchdog, got %+v", b)
	}
	t.Setenv("WATCHDOG_USEC", "not-a-number")
	if b := newBeat(quietf); b != nil {
		t.Fatal("an unparseable WATCHDOG_USEC should disable rather than guess")
	}
	// WATCHDOG_PID names the one process systemd wants the pings from. Answering for another
	// process would report liveness we cannot vouch for.
	t.Setenv("WATCHDOG_USEC", "30000000")
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()+1))
	if b := newBeat(quietf); b != nil {
		t.Fatal("a WATCHDOG_PID naming another process should disable the beat")
	}
}

// The interval is derived from the unit rather than declared here, so WatchdogSec has one
// definition. Asserted because a constant creeping back in would drift silently.
func TestNewBeatDerivesIntervalFromUnit(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "30000000") // WatchdogSec=30
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))
	b := newBeat(quietf)
	if b == nil {
		t.Fatal("WATCHDOG_USEC set for this pid should arm the beat")
	}
	if want := 10 * time.Second; b.every != want {
		t.Fatalf("ping interval = %s, want %s (a third of the period)", b.every, want)
	}
	if b.every*2 >= 30*time.Second {
		t.Fatalf("interval %s leaves no margin under a 30s period", b.every)
	}
}

// A nil beat is the ordinary state outside systemd, so every method has to tolerate it --
// otherwise the lab fleet and every dev run panic on the first ping.
func TestNilBeatIsSafe(t *testing.T) {
	var b *beat
	b.Beat()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b.Lease(ctx) // must not panic and must not spawn anything
}

// THE ORDERING PROPERTY. Lease must reject an unbounded context even when there is no watchdog
// running, because that is the only place the mistake is visible: in tests and dev runs the
// goroutine would never spawn, so a guard placed after the nil-check would stay silent until
// production -- where the symptom is a watchdog that never fires again.
func TestLeaseRejectsUnboundedContextEvenWhenDisabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    *beat
	}{
		{"nil beat (no watchdog on this unit)", nil},
		{"armed beat", &beat{every: time.Second, send: func() error { return nil }, logf: quietf}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("leasing a context with no deadline must panic: it would ping forever, " +
						"which is the permanent-disable bug the lease exists to prevent")
				}
			}()
			tc.b.Lease(context.Background())
		})
	}
}

// THE PROPERTY THAT MAKES IT A LEASE RATHER THAN A FLAG: it stops on its own.
//
// A flag with an enable/disable pair is never disabled by a goroutine that never returns, so a
// wedge inside the leased operation would leave the pings running forever -- disabling the
// watchdog permanently, and the more completely the agent wedged the longer it would stay off.
// This asserts the expiry directly: pings while the budget is live, silence once it is spent.
func TestLeaseExpiresWithItsBudget(t *testing.T) {
	b, sent := countingBeat(5 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	b.Lease(ctx)
	time.Sleep(40 * time.Millisecond)
	if mid := sent.Load(); mid == 0 {
		t.Fatal("a live lease must ping; got none while the budget was still open")
	}
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond) // let the goroutine observe Done and return
	after := sent.Load()
	time.Sleep(60 * time.Millisecond) // several tick intervals past the deadline
	if grew := sent.Load() - after; grew != 0 {
		t.Fatalf("the lease kept pinging %d times past its deadline; an expired lease must stop "+
			"on its own, or a wedged operation disables the watchdog forever", grew)
	}
}

// Releasing early is an optimisation, not a correctness requirement -- but it must work, or the
// watchdog stays loose after every completed operation instead of tightening back up.
func TestLeaseReleasesOnCancel(t *testing.T) {
	b, sent := countingBeat(5 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	b.Lease(ctx)
	time.Sleep(30 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)
	after := sent.Load()
	time.Sleep(60 * time.Millisecond)
	if grew := sent.Load() - after; grew != 0 {
		t.Fatalf("the lease pinged %d times after its context was cancelled", grew)
	}
}

// ============================================================================================
// THE GUARD AGAINST THE NEXT [V3b.15], and it is here rather than in report_test.go because
// what it defends is a property of the BEAT: a directive that declares a budget must lease it.
//
// The defect it exists to catch is not subtle in hindsight and was invisible in practice.
// dispatch runs synchronously on the observe loop, which is the only thing that pings systemd,
// so nothing pings for as long as a directive runs. Every forward path declared a budget and
// leased none of it, and a `service install` measured at 48.8 s was SIGABRTed at 20 s on every
// shipped node for a week. Nothing failed except the product.
//
// HOW THESE ASSERT IT, and why there is no sleep anywhere. A test that slept past a ping
// interval and counted pings would be a timing race on a loaded runner. Instead the fake BLOCKS
// until the watchdog is fed: if the budget was leased, a ping arrives within a millisecond and
// the directive completes; if it was not, no ping can ever arrive, the hold gives up, and the
// failure surfaces as the directive's own outcome detail. The handler cannot finish without the
// property being true, which is the strongest shape available here.
//
// WHAT IS NOT COVERED, so a green is not read as more than it is. `cert-request` (a 30 s budget)
// and `agent-update` (10 m) go through the same beat.budget call and are NOT driven here: the
// first needs a real keypair and guest, the second a real signed update, and neither earns that
// setup for a property the other four already pin. And no test can force a SIXTH directive to be
// added to this table -- what it does is make the pattern obvious enough to copy, and fail loudly
// for the five paths that exist.
// ============================================================================================

// pingingBeat is a beat whose first ping closes the returned channel. Interval is a millisecond
// so a leased budget feeds it effectively immediately; an unleased one never does, at any speed.
func pingingBeat() (*beat, <-chan struct{}) {
	pinged := make(chan struct{})
	var once sync.Once
	return &beat{
		every: time.Millisecond,
		send:  func() error { once.Do(func() { close(pinged) }); return nil },
		logf:  quietf,
	}, pinged
}

// heldUntilPinged is the fakes' hold hook: block until the watchdog is fed, or report that it
// never was. The wait is long enough that a busy runner cannot fail it and short enough that an
// unleased path fails the test rather than the whole package's timeout.
func heldUntilPinged(pinged <-chan struct{}) func() error {
	return func() error {
		select {
		case <-pinged:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("no watchdog ping arrived while this directive ran -- its budget " +
				"was never leased, so systemd would kill the agent mid-work (V3b.15)")
		}
	}
}

func TestLongDirectivesKeepTheWatchdogFed(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    api.Directive
	}{
		{"rescue", api.Directive{Kind: api.DirectiveRescue}},
		{"os upgrade", api.Directive{Kind: api.DirectiveUpgradeSystem, Payload: "/nix/store/abc-nixos-system"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, pinged := pingingBeat()
			up := &fakeUpgrader{hold: heldUntilPinged(pinged)}
			o := applyDirective(context.Background(), tc.d, up,
				&fakeNotifier{}, nil, nil, quietf, testUpgradeBudget, b)
			if o.State != api.OutcomeDone {
				t.Fatalf("%s did not complete: %s", tc.name, o.Detail)
			}
		})
	}
}

// The verb the field report was written about, driven end to end against a real signed catalog.
func TestServiceInstallKeepsTheWatchdogFed(t *testing.T) {
	cfg := catalogFor(t, testManifest())
	b, pinged := pingingBeat()
	cfg.beat = b
	f := &fakeInstaller{primary: true, active: true, healthy: true, hold: heldUntilPinged(pinged)}
	if o := install(cfg, f); o.State != api.OutcomeDone {
		t.Fatalf("service install did not complete: %s", o.Detail)
	}
}
