package host

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// A stall is reported once, with every goroutine in it, and a resumed beat re-arms the report.
// Run on a Windows host (the cross-compiled test binary), since this is Windows' only diagnosis.
func TestStaleBeatDumpsEveryGoroutineOnce(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	logf := func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	count := func(sub string) (n int) {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range lines {
			if strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}
	var beaten atomic.Bool // no beat arrives
	go dumpWhenStale(&beaten, 50*time.Millisecond, logf)
	time.Sleep(time.Second)
	if n := count("every goroutine follows"); n != 1 {
		t.Fatalf("%d dumps for one stall, want exactly 1", n)
	}
	if count("goroutine ") == 0 || !strings.Contains(strings.Join(lines, ""), "TestStaleBeatDumpsEveryGoroutineOnce") {
		t.Errorf("the dump does not carry the stacks (want this test's own frame in it)")
	}
	beaten.Store(true)
	time.Sleep(200 * time.Millisecond)
	if count("beats resumed") != 1 {
		t.Errorf("a resumed beat was not said")
	}
}

// The beat event is one bit across processes: a set is seen by exactly one look, and a second look
// with no set in between sees nothing -- the warden's tick question.
func TestBeatEventIsConsumedByOneLook(t *testing.T) {
	a, err := beatEvent()
	if err != nil {
		t.Fatalf("creating the beat event: %v", err)
	}
	b, err := beatEvent() // the other side's open of the same name
	if err != nil {
		t.Fatalf("opening the beat event again: %v", err)
	}
	if beatSince(b) {
		t.Fatal("a fresh event read as a beat")
	}
	_ = windows.SetEvent(a)
	_ = windows.SetEvent(a) // two beats between looks are one bit
	if !beatSince(b) {
		t.Fatal("a set on one handle was not seen through the other")
	}
	if beatSince(b) {
		t.Fatal("the look did not clear the event")
	}
}
