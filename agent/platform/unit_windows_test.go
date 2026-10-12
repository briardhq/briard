package platform

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The property the Windows backend exists for, against the real kernel: a unit started by one
// process is still found BY NAME once that process holds no handle to it -- the agent restart.
// The first version of this file failed exactly here (the job's name died with the launcher's
// handle while the job and its process lived on), so this is the regression it would catch.
// Runs on a Windows host: the cross-compiled test binary, as an administrator.
func TestUnitIsFoundByNameAfterTheLauncherLetsGo(t *testing.T) {
	ctx := context.Background()
	unit := "briard-test-" + strconv.Itoa(os.Getpid()) + ".service"
	log := filepath.Join(t.TempDir(), "unit.log")
	ping := filepath.Join(os.Getenv("SystemRoot"), "System32", "PING.EXE")
	args := []string{"--unit=" + unit, "-p", "StandardOutput=append:" + log, "--", ping, "-n", "60", "127.0.0.1"}
	if _, err := startTransient(ctx, args); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unitKill(unit) })

	if !unitIsActive(ctx, unit) {
		t.Fatal("the unit is not found by name after its launcher closed every handle it held")
	}
	if s, err := unitShow(unit, "LoadState"); err != nil || s != "loaded" {
		t.Errorf("LoadState = %q, %v; want loaded", s, err)
	}
	if _, err := startTransient(ctx, args); err == nil {
		t.Error("a second start of a running unit was accepted")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(log)
		if strings.Contains(string(b), "127.0.0.1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the unit's output never reached its log; log holds %q", b)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if _, err := unitStop(unit); err != nil {
		t.Fatal(err)
	}
	if unitIsActive(ctx, unit) {
		t.Error("the unit is still active after unitStop")
	}
	// The name goes with the last handle, i.e. once the dead process's handle table is torn down --
	// a moment after its process count reads 0. waitUnitFree polls for exactly this.
	deadline = time.Now().Add(5 * time.Second)
	for {
		s, err := unitShow(unit, "LoadState")
		if err == nil && s == "not-found" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("5s after the stop LoadState = %q, %v; want not-found (the name goes with the unit)", s, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
