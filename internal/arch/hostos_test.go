package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hostOSPackages are the packages cut by build tag into a Linux arm and a Windows arm: a shared
// file in any of them runs on both, so it may name no mechanism that exists on one.
var hostOSPackages = []string{"agent/platform", "agent/nic", "agent/reportcard", "agent/host", "agent/subnet"}

// hostOSLiterals are the string constants that mean "this line only runs on Linux": the service
// manager, iproute2, and the pseudo-filesystems. They are what a per-OS backend REPLACES, so a
// file that is not tagged for one OS has no business naming them.
var hostOSLiterals = []string{"systemctl", "systemd-run", "ip", "drbdadm", "journalctl"}

var hostOSPrefixes = []string{"/proc/", "/sys/", "/run/systemd", "/etc/"}

// TestHostOSCallsStayBehindTheSeam is what keeps the host-OS seam's cut from decaying back into a
// comment. `GOOS=windows go build ./...` cannot do this job alone: it catches a Linux-only
// SYMBOL (syscall.Statfs), but exec.Command("systemctl", ...) and os.ReadFile("/proc/net/route")
// compile perfectly on Windows and fail -- or quietly read "" -- only when someone runs them
// there. So the shell-level and procfs-level dependency needs an assertion of its own, and this
// is it: the seam's other half, across every package that has a Windows arm.
//
// It reads string LITERALS out of the AST rather than grepping, so the rationale comments above
// these calls (which necessarily name systemd, iproute2 and /proc) do not trip it.
//
// Adding a host-OS call is not forbidden; putting it in a shared file is. The fix is always the
// same: the mechanism goes in a _linux.go file with a _windows.go counterpart, and the policy above
// it stays shared where both platforms read it and the tests reach it.
func TestHostOSCallsStayBehindTheSeam(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	for _, pkg := range hostOSPackages {
		dir := filepath.Join(root, pkg)
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", pkg, err)
		}
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
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatalf("parse %s/%s: %v", pkg, name, err)
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
							"the seam (a _linux.go file) and give it a _windows.go counterpart",
							fset.Position(lit.Pos()), s)
					}
				}
				for _, bad := range hostOSPrefixes {
					if strings.HasPrefix(s, bad) {
						t.Errorf("%s: %q is a Linux path in a shared file -- move it behind the seam "+
							"and give it a _windows.go counterpart", fset.Position(lit.Pos()), s)
					}
				}
				return true
			})
		}
		// The scan finding nothing must mean it LOOKED. A rename that emptied a package's file
		// list would otherwise leave a test that passes by inspecting nothing.
		if checked == 0 {
			t.Fatalf("%s: scanned no shared files: the guard is vacuous", pkg)
		}
	}
}
