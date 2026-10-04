package reportcard

import "testing"

// The host half of `briard doctor`: an installed machine's own disk and clock.
func TestAssessLive(t *testing.T) {
	cases := []struct {
		name  string
		f     LiveFacts
		check string
		want  Status
	}{
		{"plenty of disk", LiveFacts{DiskFreeMB: 40 * 1024, NTPSynced: "yes"}, "disk", Pass},
		{"low disk", LiveFacts{DiskFreeMB: 4 * 1024, NTPSynced: "yes"}, "disk", Warn}, // under the 4.5 GB an update needs
		{"full disk", LiveFacts{DiskFreeMB: 1024, NTPSynced: "yes"}, "disk", Refuse},
		// Could not measure is said, never passed.
		{"unmeasured disk", LiveFacts{NTPSynced: "yes"}, "disk", Warn},
		{"clock synced", LiveFacts{DiskFreeMB: 40 * 1024, NTPSynced: "yes"}, "clock", Pass},
		{"clock not synced", LiveFacts{DiskFreeMB: 40 * 1024, NTPSynced: "no"}, "clock", Warn},
		{"clock unreadable", LiveFacts{DiskFreeMB: 40 * 1024}, "clock", Warn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := find(t, Report{Checks: AssessLive(tc.f)}, tc.check)
			if c.Status != tc.want {
				t.Errorf("%s = %+v, want %s", tc.check, c, tc.want)
			}
			if c.Status == Refuse && c.Fix == "" {
				t.Errorf("%s fails without saying what to do", tc.check)
			}
		})
	}
}
