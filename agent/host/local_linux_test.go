package host

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"briard.io/internal/testsock"
)

// TestServeLocalIsRootOnly: on Linux the door is 0600, so it is the agent's own uid (root on a
// real node) or nobody. An admin socket that any local user can drive is a privilege escalation
// wearing a convenience costume. On Windows the door is a DACL instead (local_windows_test.go).
func TestServeLocalIsRootOnly(t *testing.T) {
	sock := filepath.Join(testsock.Dir(t), "admin.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveLocal(ctx, sock, "", make(chan localRequest), quiet)

	var st os.FileInfo
	var err error
	for i := 0; i < 100; i++ {
		if st, err = os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("stat %s: %v", sock, err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %04o, want 0600", perm)
	}
	if st.Mode()&fs.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (mode %v)", sock, st.Mode())
	}
}
