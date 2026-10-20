package guestagent

import (
	"testing"
	"time"
)

// THE ANNOUNCER'S DECISION, tick by tick: a link-up owes a burst, an up link owes a beat, a down
// link owes nothing, and a link that comes back owes a fresh burst however recently the last
// announcement went out.
func TestAnnouncerStep(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	type tick struct {
		sec     int
		carrier bool
		want    bool
	}
	for _, tc := range []struct {
		name  string
		ticks []tick
	}{
		{"up at start: the claim's burst, then quiet until the beat", []tick{
			{0, true, true}, {1, true, true}, {2, true, true}, {3, true, false}, {31, true, false}, {32, true, true}, {33, true, false},
		}},
		{"down at start: nothing, then the link-up's burst", []tick{
			{0, false, false}, {1, false, false}, {2, true, true}, {3, true, true}, {4, true, true}, {5, true, false},
		}},
		{"a bounce owes a fresh burst, beat or no beat", []tick{
			{0, true, true}, {1, true, true}, {2, true, true}, {10, false, false}, {12, true, true}, {13, true, true}, {14, true, true}, {15, true, false},
		}},
		{"a bounce mid-burst restarts it", []tick{
			{0, true, true}, {1, false, false}, {2, true, true}, {3, true, true}, {4, true, true}, {5, true, false},
		}},
		{"a down link owes no beat", []tick{
			{0, true, true}, {1, true, true}, {2, true, true}, {3, false, false}, {60, false, false}, {120, false, false},
		}},
		{"the beat repeats while up", []tick{
			{0, true, true}, {1, true, true}, {2, true, true}, {32, true, true}, {61, true, false}, {62, true, true}, {92, true, true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a announcer
			for _, k := range tc.ticks {
				if got := a.step(at(k.sec), k.carrier); got != k.want {
					t.Errorf("+%ds carrier=%v: announce=%v, want %v", k.sec, k.carrier, got, k.want)
				}
			}
		})
	}
}
