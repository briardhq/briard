package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// hostOSLiterals are the string constants that mean "this line only runs on Linux": the service
// manager, iproute2, and the pseudo-filesystems. They are what a per-OS backend REPLACES, so a
// file that is not tagged for one OS has no business naming them.
var hostOSLiterals = []string{"systemctl", "systemd-run", "ip", "drbdadm", "journalctl"}

var hostOSPrefixes = []string{"/proc/", "/sys/", "/run/systemd"}

// TestHostOSCallsStayBehindTheSeam is what keeps [V3b.27](d)'s cut from decaying back into a
// comment. `GOOS=windows go build ./...` cannot do this job alone: it catches a Linux-only
// SYMBOL (syscall.Statfs), but exec.Command("systemctl", ...) compiles perfectly on Windows and
// fails only when someone runs it there. So the shell-level dependency needs an assertion of its
// own, and this is it -- the seam's other half.
//
// It reads string LITERALS out of the AST rather than grepping, so the rationale comments above
// these calls (which necessarily name systemd and iproute2) do not trip it.
//
// Adding a host-OS call is not forbidden; putting it in a shared file is. The fix is always the
// same: the mechanism goes in unit_linux.go or route_linux.go with a _windows.go counterpart,
// and the policy above it stays here where both platforms read it and the tests reach it.
func TestHostOSCallsStayBehindTheSeam(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// _linux.go / _windows.go ARE the backends -- naming the mechanism is their whole job.
		if strings.HasSuffix(name, "_linux.go") || strings.HasSuffix(name, "_windows.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, bad := range hostOSLiterals {
				if s == bad {
					t.Errorf("%s: %q is a host-OS dependency in a shared file -- move it behind "+
						"the seam (unit_linux.go / route_linux.go) and stub it for Windows",
						fset.Position(lit.Pos()), s)
				}
			}
			for _, bad := range hostOSPrefixes {
				if strings.HasPrefix(s, bad) {
					t.Errorf("%s: %q is a Linux pseudo-filesystem path in a shared file -- move it "+
						"behind the seam and stub it for Windows", fset.Position(lit.Pos()), s)
				}
			}
			return true
		})
	}
	// The scan finding nothing must mean it LOOKED. A rename that emptied the file list would
	// otherwise leave a test that passes by inspecting nothing.
	if checked == 0 {
		t.Fatal("scanned no shared files: the guard is vacuous")
	}
}
