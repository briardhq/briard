package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot returns the repo root (the dir containing go.mod).
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test file")
		}
		dir = parent
	}
}

// TestHostSeamDiscipline enforces CONTRIBUTING.md invariant 1 (host seam
// discipline): the agent's orchestration lives in agent/host, which reaches every
// provider only through its seam interface. The concrete provider implementations
// (netbird, libvirt) are wired in at main and injected — host must never
// import them, directly or transitively. It may import agent/drbd, the
// observe-and-report package (guarded separately by invariant 2).
func TestHostSeamDiscipline(t *testing.T) {
	forbidden := []string{"netbird", "libvirt"}
	cmd := exec.Command("go", "list", "-deps", "briard.io/agent/host")
	cmd.Dir = moduleRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range forbidden {
			if strings.Contains(dep, bad) {
				t.Errorf("agent/host must not depend on concrete provider %q (matched %q); reach it through its seam interface", bad, dep)
			}
		}
	}
}

// TestDrbdHasNoLifecycleAPI enforces CONTRIBUTING.md invariant 2 (failover stays
// out of the agent): the agent's drbd package observes and reports; it never
// exposes an API that drives the failover lifecycle.
func TestDrbdHasNoLifecycleAPI(t *testing.T) {
	driving := []string{"promote", "demote", "claimvip", "releasevip", "fence", "stonith"}
	dir := filepath.Join(moduleRoot(t), "agent", "drbd")
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() {
				continue
			}
			low := strings.ToLower(fn.Name.Name)
			for _, v := range driving {
				if strings.Contains(low, v) {
					t.Errorf("drbd package must not expose lifecycle API %q (matched %q)", fn.Name.Name, v)
				}
			}
		}
	}
}

// TestNoForcePromote enforces CONTRIBUTING.md invariant 3 (no force-promotion):
// DRBD is the sole write-authority, so the codebase never force-promotes. Scans
// source files (docs excluded).
func TestNoForcePromote(t *testing.T) {
	patterns := []string{"primary --force", "--overwrite-data-of-peer", "--force-primary"}
	root := moduleRoot(t)
	skipDir := map[string]bool{".git": true, ".direnv": true, "docs": true, "result": true}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// Only scan regular files: symlinks like `result` (nix build output) or
		// `.direnv`'s store links point at directories and aren't source.
		if !d.Type().IsRegular() {
			return nil
		}
		// Scan code, not prose; skip the guard file itself (it names the patterns).
		switch filepath.Ext(path) {
		case ".md", ".txt":
			return nil
		}
		if strings.Contains(path, filepath.Join("internal", "arch")) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(b)
		for _, p := range patterns {
			if strings.Contains(content, p) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("force-promotion is forbidden; found %q in %s", p, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestNoDirectPromoteDemote enforces CONTRIBUTING.md invariant 2 at the exec
// surface: the agent observes failover, never drives it — drbd-reactor runs the
// lifecycle. The text
// scan above catches `primary --force` written as one token, but the surface that
// actually matters is a call shelling out to `drbdadm`/`drbdsetup` with a
// `primary`/`secondary` verb as *separate* string arguments (force or not — any
// direct promote/demote is drbd-reactor's job). Parse every non-test .go file and
// flag any single call that passes both a command literal and a role-verb literal.
// It passes today (guestagent only ever calls create-md / new-current-uuid /
// status) and fails the day someone adds a direct promotion at the exec surface.
func TestNoDirectPromoteDemote(t *testing.T) {
	cmds := map[string]bool{"drbdadm": true, "drbdsetup": true}
	roles := map[string]bool{"primary": true, "secondary": true}
	root := moduleRoot(t)
	skipDir := map[string]bool{".git": true, ".direnv": true, "result": true}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip the guard file itself (it names the verbs).
		if strings.Contains(path, filepath.Join("internal", "arch")) {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var hasCmd, hasRole bool
			for _, a := range call.Args {
				lit, ok := a.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				if cmds[v] {
					hasCmd = true
				}
				if roles[v] {
					hasRole = true
				}
			}
			if hasCmd && hasRole {
				rel, _ := filepath.Rel(root, path)
				line := fset.Position(call.Pos()).Line
				t.Errorf("direct DRBD promote/demote is forbidden (CONTRIBUTING.md invariant 2 — drbd-reactor drives the lifecycle); found a drbdadm/drbdsetup primary/secondary call in %s:%d", rel, line)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
