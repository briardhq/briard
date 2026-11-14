// Package testsock hands out short filesystem paths for unix sockets in tests.
//
// `t.TempDir()` is the obvious thing and a trap here. A unix socket path is capped at 108 bytes
// (sun_path in sockaddr_un) and the kernel reports an overflow as a bare EINVAL: the test prints
// "bind: invalid argument" or "connect: invalid argument", naming neither the limit nor the
// length, so it reads like a permissions or namespace problem, not a path that is too long.
//
// `t.TempDir()` builds its directory name from the test name. Descriptive Go test names run well
// past 60 characters and a CI runner's TMPDIR is commonly ~40 deep before the test adds anything,
// so the total clears 108 while a developer's `/tmp` never does. That asymmetry is the real
// hazard: the suite is green on every machine that would notice and red only where nobody is
// reading. Dir is therefore a per-test directory with a short, fixed prefix instead of the test
// name.
package testsock

import (
	"os"
	"testing"
)

// Dir returns a temporary directory whose path is short enough to hold a unix socket, cleaned
// up when the test finishes. Use it anywhere a `.sock` path is built; use t.TempDir() for
// everything else, where the test name in the path is a help rather than a hazard.
func Dir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "b")
	if err != nil {
		t.Fatalf("testsock: temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}
