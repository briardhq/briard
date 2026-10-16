package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeZoneGuest struct {
	old  bool
	err  error
	told []string
}

func (f *fakeZoneGuest) SetTimezone(_ context.Context, zone string) error {
	f.told = append(f.told, zone)
	return f.err
}
func (f *fakeZoneGuest) SupportsTimezone() bool { return !f.old }

// TestTheGuestIsToldTheHostsZone, and nothing else: no zone, an older guest and a guest that
// refuses all leave it on UTC with a log line, never a failed bring-up (pushTimezone has no
// error to return).
func TestTheGuestIsToldTheHostsZone(t *testing.T) {
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	g := &fakeZoneGuest{}
	pushTimezone(context.Background(), g, "Europe/Athens", logf)
	if len(g.told) != 1 || g.told[0] != "Europe/Athens" {
		t.Fatalf("told %v, want Europe/Athens", g.told)
	}
	for _, tc := range []struct {
		name string
		g    *fakeZoneGuest
		zone string
	}{
		{"no zone on the host", &fakeZoneGuest{}, ""},
		{"an older guest", &fakeZoneGuest{old: true}, "Europe/Athens"},
		{"the guest refuses", &fakeZoneGuest{err: errors.New("not in this image's database")}, "Europe/Athens"},
	} {
		lines = nil
		pushTimezone(context.Background(), tc.g, tc.zone, logf)
		if len(lines) != 1 || !strings.Contains(lines[0], "stays on UTC") {
			t.Errorf("%s: logged %v, want one line saying the guest stays on UTC", tc.name, lines)
		}
		if tc.zone == "" || tc.g.old {
			if len(tc.g.told) != 0 {
				t.Errorf("%s: the guest was told %v", tc.name, tc.g.told)
			}
		}
	}
}
