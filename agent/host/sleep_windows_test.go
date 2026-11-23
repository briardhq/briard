package host

import (
	"testing"
	"time"
)

// TestColdBootSince asks the machine the test runs on when its kernel started, so it proves the
// call itself: a start in the past, and the two answers either side of it.
func TestColdBootSince(t *testing.T) {
	start, err := kernelStart()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the kernel started %s", start.UTC().Format(time.RFC3339Nano))
	if up := time.Since(start); up <= 0 || up > 10*365*24*time.Hour {
		t.Fatalf("the kernel started %s ago", up)
	}
	// A minute either side: two reads of the interrupt time differ by a clock tick at most.
	for since, want := range map[time.Duration]bool{-time.Minute: true, time.Minute: false} {
		got, err := coldBootSince(start.Add(since))
		if err != nil || got != want {
			t.Errorf("coldBootSince(the start %+v) = %v, %v, want %v", since, got, err, want)
		}
	}
}
