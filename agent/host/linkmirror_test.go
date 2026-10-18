package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeLink records each set and can refuse them.
type fakeLink struct {
	calls []bool
	err   error
}

func (f *fakeLink) set(_ context.Context, up bool) error {
	f.calls = append(f.calls, up)
	return f.err
}

// One tick of the mirror, from each starting record (a fresh loop, or a link last set up or down)
// against each reading of the NIC. A set happens exactly when the record differs from the reading,
// and the record is then the reading.
func TestLinkMirrorSetsOnlyWhatDiffers(t *testing.T) {
	up, down := true, false
	for _, start := range []*bool{nil, &up, &down} {
		for _, member := range []string{"Ethernet", ""} {
			for _, carrier := range []bool{true, false} {
				name := fmt.Sprintf("start=%s member=%q carrier=%v", recordString(start), member, carrier)
				m := &linkMirror{}
				if start != nil {
					v := *start
					m.set = &v
				}
				f := &fakeLink{}
				m.tick(context.Background(), member, carrier, f.set, func(string, ...any) {})
				want := member != "" && carrier
				if start == nil || *start != want {
					if len(f.calls) != 1 || f.calls[0] != want {
						t.Errorf("%s: sets = %v, want [%v]", name, f.calls, want)
					}
				} else if len(f.calls) != 0 {
					t.Errorf("%s: sets = %v, want none (already %v)", name, f.calls, want)
				}
				if m.set == nil || *m.set != want {
					t.Errorf("%s: record = %s, want %v", name, recordString(m.set), want)
				}
			}
		}
	}
}

func recordString(b *bool) string {
	if b == nil {
		return "unset"
	}
	return fmt.Sprint(*b)
}

// THE RULE, over a whole run: whatever the NIC does, and whether or not sets fail on the way,
// no set ever asks for up while the NIC is down, and after any successful tick the link is the
// NIC's state.
func TestLinkMirrorNeverUpWhileTheNICIsDown(t *testing.T) {
	carrier := []bool{false, false, true, true, false, true, false, false, true}
	for _, failAt := range []int{-1, 0, 2, 4, 8} {
		m := &linkMirror{}
		for i, c := range carrier {
			f := &fakeLink{}
			if i == failAt {
				f.err = errors.New("qmp: gone")
			}
			m.tick(context.Background(), "Ethernet", c, f.set, func(string, ...any) {})
			for _, s := range f.calls {
				if s && !c {
					t.Fatalf("failAt=%d tick %d: set up while the NIC is down", failAt, i)
				}
			}
			if f.err == nil && (m.set == nil || *m.set != c) {
				t.Errorf("failAt=%d tick %d: link %s after a successful tick, NIC %v", failAt, i, recordString(m.set), c)
			}
		}
	}
}

// A refused set is tried again on the next tick, and said once rather than on every tick.
func TestLinkMirrorRetriesAFailedSetAndSaysItOnce(t *testing.T) {
	m := &linkMirror{}
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	f := &fakeLink{err: errors.New("qmp: gone")}
	for range 3 {
		m.tick(context.Background(), "Ethernet", false, f.set, logf)
	}
	if len(f.calls) != 3 {
		t.Errorf("sets = %v, want one per tick while it fails", f.calls)
	}
	if n := countContaining(lines, "could not set"); n != 1 {
		t.Errorf("failure said %d times, want once: %q", n, lines)
	}
	f.err = nil
	m.tick(context.Background(), "Ethernet", false, f.set, logf)
	if m.set == nil || *m.set {
		t.Errorf("record = %s after the set succeeded, want false", recordString(m.set))
	}
	f.err = errors.New("qmp: gone again")
	m.tick(context.Background(), "Ethernet", true, f.set, logf)
	if n := countContaining(lines, "could not set"); n != 2 {
		t.Errorf("a new failure after a success said %d times in all, want 2: %q", n, lines)
	}
}

// The first tick of a loop is silent when the NIC is up (the ordinary launch), and says so when it
// is down; later edges are always said.
func TestLinkMirrorSaysTheEdges(t *testing.T) {
	for _, tc := range []struct {
		carrier []bool
		lines   int
	}{
		{[]bool{true}, 0},
		{[]bool{false}, 1},
		{[]bool{true, false, true}, 2},
		{[]bool{true, true, true}, 0},
	} {
		m := &linkMirror{}
		var lines []string
		f := &fakeLink{}
		for _, c := range tc.carrier {
			m.tick(context.Background(), "Ethernet", c, f.set, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
		}
		if len(lines) != tc.lines {
			t.Errorf("carrier %v: %d lines, want %d: %q", tc.carrier, len(lines), tc.lines, lines)
		}
	}
}

func countContaining(lines []string, s string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, s) {
			n++
		}
	}
	return n
}
