package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"briard.io/internal/testsock"
	"briard.io/shared/api"
	"briard.io/shared/model"
)

// A SIGTERM IS A SHUTDOWN ONLY WHILE THE MACHINE IS STOPPING. Every other answer `systemctl
// is-system-running` gives -- the ordinary ones, the boot's, a failed query's empty line -- is a
// stop, which drains and leaves the guest serving: reading one of those as a shutdown would abort
// an update's act and power a serving guest off.
func TestASigtermIsAShutdownOnlyWhileTheMachineStops(t *testing.T) {
	for state, want := range map[string]error{
		"stopping":     ErrShutdown,
		"running":      nil,
		"degraded":     nil,
		"starting":     nil,
		"initializing": nil,
		"maintenance":  nil,
		"offline":      nil,
		"unknown":      nil,
		"":             nil,
	} {
		if got := StopCause(state); !errors.Is(got, want) || (want == nil) != (got == nil) {
			t.Errorf("StopCause(%q) = %v, want %v", state, got, want)
		}
	}
}

// THE STOP DIRECTIVE ABORTS THE ACT IN FLIGHT -- the other stop, through the real loop. With a
// sync held open, the directive is answered at once naming the act, the node's context ends as a
// shutdown, and the loop ends without waiting for the act: the drain TestAStopFinishesTheActInFlight
// proves is exactly what a shutdown must not do.
func TestTheStopDirectiveAbortsTheActInFlight(t *testing.T) {
	cfg := loopConfig(t)
	cfg.acts = newActLane()
	release := make(chan struct{})
	defer close(release)
	var handoffs, cycles int32
	g := actingGuest{fakeStatus: fakeStatus{qs: model.QuorumState{Primary: true, Quorate: true}}, release: release, handoffs: &handoffs, cycles: &cycles}
	local := make(chan localRequest)
	node, down := context.WithCancelCause(context.Background())
	defer down(nil)
	cfg.down = down
	var mu sync.Mutex
	var logged []string
	logf := func(f string, a ...any) { mu.Lock(); logged = append(logged, fmt.Sprintf(f, a...)); mu.Unlock() }
	ctx := cfg.acts.stopAfter(node, logf)
	done := make(chan error, 1)
	go func() {
		done <- cfg.observe(ctx, g, nil, nil, nil, nil, nil, nil, "", local, &[]api.DirectiveOutcome{}, logf)
	}()
	submit(local, api.DirectiveSync)
	for !cfg.acts.inFlight() {
		time.Sleep(time.Millisecond)
	}
	o := await(t, "the stop", submit(local, api.DirectiveStop), time.Second)
	if o.State != api.OutcomeDone || !strings.Contains(o.Detail, "aborted the sync in flight") {
		t.Fatalf("the stop = %+v, want done, naming the aborted sync", o)
	}
	if !shuttingDown(node) {
		t.Fatalf("the stop did not end the node's context as a shutdown (cause %v)", context.Cause(node))
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the loop waited on the act in flight: the shutdown drained")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(logged, func(l string) bool { return strings.Contains(l, "shutting down: aborting the sync in flight") }) {
		t.Errorf("no log line names the aborted act: %q", logged)
	}
}

// A shutdown with nothing in flight ends the loop at once, like a stop.
func TestAShutdownWithNothingInFlightIsImmediate(t *testing.T) {
	a := newActLane()
	node, down := context.WithCancelCause(context.Background())
	ctx := a.stopAfter(node, quiet)
	down(ErrShutdown)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("an idle lane held the shutdown")
	}
}

// A STOP DIRECTIVE DURING A DRAIN IS REFUSED, never quietly folded into it: a drain ends with the
// guest still running, so answering "stopping" would leave `briard stop` reporting a node down
// that is serving.
func TestAStopDirectiveDuringADrainIsRefused(t *testing.T) {
	a := newActLane()
	if _, ok := a.claim(api.DirectiveSync); !ok {
		t.Fatal("claim")
	}
	defer a.release()
	parent, stop := context.WithCancel(context.Background())
	a.stopAfter(parent, quiet)
	stop()
	for _, stopping := a.state(); !stopping; _, stopping = a.state() {
		time.Sleep(time.Millisecond)
	}
	cfg := Config{acts: a, down: func(error) { t.Error("a stop during a drain started a shutdown") }}
	if o := cfg.applyStop(api.Directive{ID: "s1", Kind: api.DirectiveStop}, quiet); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "already stopping") {
		t.Fatalf("a stop during a drain = %+v, want refused as already stopping", o)
	}
}

// THE STOP'S ANSWER OUTLIVES THE CONTEXT IT ENDS. The loop cancels the node's context and sends
// the outcome a moment later, so the door must still hand that outcome to the CLI rather than
// "agent shut down": otherwise `briard stop` could never name the act it aborted.
func TestTheDoorAnswersAStopThatEndedItsContext(t *testing.T) {
	sock := filepath.Join(testsock.Dir(t), "admin.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := make(chan localRequest)
	go serveLocal(ctx, sock, reqs, quiet)
	go func() {
		rq := <-reqs
		cancel()
		time.Sleep(50 * time.Millisecond)
		rq.resp <- api.DirectiveOutcome{ID: rq.d.ID, State: api.OutcomeDone, Detail: "aborted the sync in flight"}
	}()
	o := dialAdmin(t, sock, api.Directive{ID: "s1", Kind: api.DirectiveStop})
	if o.State != api.OutcomeDone || o.Detail != "aborted the sync in flight" {
		t.Fatalf("outcome = %+v, want the stop's own answer", o)
	}
}

// Submit tells the two ways of getting no answer apart, because `briard stop` acts on the
// difference: nobody listening is the fallback's case, an agent that hung up is a stop under way.
func TestSubmitNamesNoAgentAndNoOutcome(t *testing.T) {
	dir := testsock.Dir(t)
	if _, err := Submit(context.Background(), filepath.Join(dir, "absent.sock"), api.Directive{Kind: api.DirectiveStop}); !errors.Is(err, ErrNoAgent) {
		t.Errorf("no socket: %v, want ErrNoAgent", err)
	}
	sock := filepath.Join(dir, "hangup.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			var d api.Directive
			_ = json.NewDecoder(c).Decode(&d)
			c.Close()
		}
	}()
	if _, err := Submit(context.Background(), sock, api.Directive{Kind: api.DirectiveStop}); !errors.Is(err, ErrNoOutcome) {
		t.Errorf("a hang-up: %v, want ErrNoOutcome", err)
	}
}
