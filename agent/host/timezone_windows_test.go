package host

import (
	"testing"

	// The zone database, for the tests alone: Windows has none Go can read, and the recorder
	// check's fixtures are in a named zone. The agent itself never loads one there (localTimezone).
	_ "time/tzdata"
)

// TestWindowsZonesMapToIANA asks the machine's own icu.dll, so it proves the call itself: the
// argument order, the UTF-16 in and out, and the status check. A wrong length or capacity reads
// as "" or garbage here, never as a plausible zone.
func TestWindowsZonesMapToIANA(t *testing.T) {
	for win, want := range map[string]string{
		"GTB Standard Time":   "Europe/Bucharest", // Athens's Windows zone: same clock, CLDR's city
		"Tokyo Standard Time": "Asia/Tokyo",
		"UTC":                 "Etc/UTC",
		"Mars Standard Time":  "", // no such zone
		"":                    "",
	} {
		if got := ianaZone(win); got != want {
			t.Errorf("ianaZone(%q) = %q, want %q", win, got, want)
		}
	}
}

// TestThisMachinesZoneHasAName: whatever this machine is set to, it maps to some IANA name.
func TestThisMachinesZoneHasAName(t *testing.T) {
	got := localTimezone("")
	if got == "" {
		t.Fatal("localTimezone = \"\", want this machine's zone as an IANA name")
	}
	t.Logf("this machine runs on %s", got)
}
