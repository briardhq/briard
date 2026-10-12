package host

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	var last atomic.Int64
	last.Store(time.Now().Add(-time.Minute).UnixNano()) // already stale
	go dumpWhenStale(&last, 150*time.Millisecond, logf)
	time.Sleep(time.Second)
	if n := count("every goroutine follows"); n != 1 {
		t.Fatalf("%d dumps for one stall, want exactly 1", n)
	}
	if count("goroutine ") == 0 || !strings.Contains(strings.Join(lines, ""), "TestStaleBeatDumpsEveryGoroutineOnce") {
		t.Errorf("the dump does not carry the stacks (want this test's own frame in it)")
	}
	last.Store(time.Now().UnixNano())
	time.Sleep(200 * time.Millisecond)
	if count("beats resumed") != 1 {
		t.Errorf("a resumed beat was not said")
	}
}
