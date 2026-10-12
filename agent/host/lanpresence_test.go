package host

import (
	"testing"
	"time"
)

// The whole decision, boot by boot: an answer confirms once; a launch with none in the window is
// relaunched, twice at most per agent life, then given up on once; an adopted guest is never
// judged; and a decided boot is not asked again.
func TestLANPresence(t *testing.T) {
	t0 := time.Unix(1000, 0)
	type step struct {
		boot     string
		answered bool
		at       time.Duration
		ask      bool       // open's answer: is this boot still worth asking
		want     lanVerdict // judge's verdict, when asked
	}
	for _, tc := range []struct {
		name    string
		adopted bool
		steps   []step
	}{
		{"an answer confirms, once", false, []step{
			{"b1", false, 0, true, lanWait},
			{"b1", true, 10 * time.Second, true, lanConfirmed},
			{"b1", false, 5 * time.Minute, false, 0}, // decided: not asked again
		}},
		{"no answer: relaunch twice, then give up once", false, []step{
			{"b1", false, 0, true, lanWait},
			{"b1", false, lanWindow - time.Second, true, lanWait},
			{"b1", false, lanWindow, true, lanRelaunch},
			{"b1", false, lanWindow + time.Second, false, 0}, // the relaunch is in flight
			{"b2", false, 3 * time.Minute, true, lanWait},
			{"b2", false, 3*time.Minute + lanWindow, true, lanRelaunch},
			{"b3", false, 6 * time.Minute, true, lanWait},
			{"b3", false, 6*time.Minute + lanWindow, true, lanGiveUp},
			{"b3", false, 9 * time.Minute, false, 0},
			{"b4", false, 10 * time.Minute, true, lanWait},
			{"b4", false, 10*time.Minute + lanWindow, true, lanGiveUp}, // judged, but no cure left
		}},
		{"the relaunch cured it", false, []step{
			{"b1", false, 0, true, lanWait},
			{"b1", false, lanWindow, true, lanRelaunch},
			{"b2", false, 3 * time.Minute, true, lanWait},
			{"b2", true, 3*time.Minute + 30*time.Second, true, lanConfirmed},
		}},
		{"an adopted guest is not judged, its successor is", true, []step{
			{"b1", false, 0, false, 0},
			{"b1", false, time.Hour, false, 0},
			{"b2", false, 2 * time.Hour, true, lanWait},
			{"b2", false, 2*time.Hour + lanWindow, true, lanRelaunch},
		}},
		{"no boot id, nothing asked", false, []step{
			{"", false, 0, false, 0},
		}},
	} {
		p := newLANPresence(tc.adopted)
		for i, s := range tc.steps {
			now := t0.Add(s.at)
			ask := p.open(s.boot, now)
			if ask != s.ask {
				t.Fatalf("%s, step %d (%s at +%s): open = %v, want %v", tc.name, i, s.boot, s.at, ask, s.ask)
			}
			if !ask {
				continue
			}
			if got := p.judge(s.answered, now); got != s.want {
				t.Errorf("%s, step %d (%s, answered %v, +%s): judge = %d, want %d", tc.name, i, s.boot, s.answered, s.at, got, s.want)
			}
		}
	}
	var none *lanPresence
	if none.open("b1", t0) || none.judge(false, t0) != lanWait {
		t.Error("a nil lanPresence must ask and say nothing")
	}
}
