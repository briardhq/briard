package host

import (
	"context"
	"errors"
	"testing"

	"briard.io/shared/api"
)

// The planned-handover directive. What is worth asserting here is the MODE
// mapping and the refusals -- the eviction itself is a real mechanism and is proven against a
// real drbd-reactor in nixosTest/reactor-evict.nix, not against a fake that would agree with
// whatever this file happens to send.

type fakeEvictor struct {
	calls      int
	keepMasked bool
	unmask     bool
	err        error
}

func (f *fakeEvictor) ReactorEvict(_ context.Context, keepMasked, unmask bool) error {
	f.calls++
	f.keepMasked, f.unmask = keepMasked, unmask
	return f.err
}

func TestApplyHandoverModes(t *testing.T) {
	for _, c := range []struct {
		name           string
		payload        string
		keepMasked     bool
		unmask         bool
		wantEvictCalls int
		wantState      string
	}{
		{
			name:    "plain -- hand over and stay eligible, so a hand-back can follow",
			payload: "", wantEvictCalls: 1, wantState: api.OutcomeDone,
		},
		{
			name:    "keep-masked -- the reboot path, the node must not reclaim the house",
			payload: "keep-masked", keepMasked: true, wantEvictCalls: 1, wantState: api.OutcomeDone,
		},
		{
			name:    "unmask -- release a masked node; evicts nothing itself",
			payload: "unmask", unmask: true, wantEvictCalls: 1, wantState: api.OutcomeDone,
		},
		{
			// The three modes differ in whether the node may take the house back, so an
			// unrecognised word must not fall through to the mildest one.
			name:    "an unknown mode is refused, not guessed",
			payload: "maybe", wantEvictCalls: 0, wantState: api.OutcomeFailed,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeEvictor{}
			o := Config{}.applyHandover(context.Background(),
				f, api.Directive{ID: "h1", Kind: api.DirectiveHandover, Payload: c.payload},
				func(string, ...any) {})

			if o.State != c.wantState {
				t.Errorf("outcome = %s (%s), want %s", o.State, o.Detail, c.wantState)
			}
			if f.calls != c.wantEvictCalls {
				t.Fatalf("evicted %d times, want %d", f.calls, c.wantEvictCalls)
			}
			if f.calls > 0 && (f.keepMasked != c.keepMasked || f.unmask != c.unmask) {
				t.Errorf("evict(keepMasked=%v, unmask=%v), want (%v, %v)",
					f.keepMasked, f.unmask, c.keepMasked, c.unmask)
			}
		})
	}
}

// A failed eviction is `failed`, never `rolled-back`: nothing was staged, quiesced or switched,
// so there is nothing to have rolled back and the node is exactly where it was.
func TestApplyHandoverReportsAFailedEviction(t *testing.T) {
	f := &fakeEvictor{err: errors.New("drbd-reactorctl: boom")}
	o := Config{}.applyHandover(context.Background(),
		f, api.Directive{ID: "h1", Kind: api.DirectiveHandover}, func(string, ...any) {})

	if o.State != api.OutcomeFailed {
		t.Fatalf("outcome = %s, want failed", o.State)
	}
	if o.Detail == "" {
		t.Error("a failed handover must carry why")
	}
}
