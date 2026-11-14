package arch

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoPrivateReferences fails on a comment, docstring or message that points a reader of this
// repo at something they cannot open: the maintainers' private design corpus, or a backlog/epoch
// item in the private tracker. Public code documents its current state and may cite only the
// public docs (README, ARCHITECTURE, CONTRIBUTING, THIRD-PARTY) and public external sources; a
// comment's rationale stands on its own words, not on a pointer a contributor cannot follow.
//
// Three shapes are refused: a private document named by name or section (`DESIGN §4`,
// `AGENTS.md`, `OSS §10.2`, `V3b.md`, `farm docs`, or a bare section sign with a number, which
// only our private docs use); a tracker item id in any form (`[B.126]`, `B.126`, `[V3b.31a](e)`,
// `V3c.4`, `[DRBD.2]`, `M0.2b`); and a link to a maintainer note (`[[kebab-case-name]]`; a TOML
// table header such as `[[promoter]]` has no dash and passes). The ordinary word "design" is not
// a document: the patterns are anchored on the capitalised name plus a section mark, file suffix
// or possessive, so "by design" and "THE ORDER IS THE DESIGN" pass.
//
// It runs in `go test ./...`, so it fires on the laptop before a push and again in CI. It reads
// every text file under the module root, not only Go: the same comments live in the nix tests,
// the installer, the guest image and the HA integration.
func TestNoPrivateReferences(t *testing.T) {
	patterns := map[string]*regexp.Regexp{
		"private document": regexp.MustCompile(
			`DESIGN(\.md|'s| §|§| →| Foundations)|(^|[^A-Za-z])DESIGN [0-9§]` +
				`|(in|per|see|from|to|of) DESIGN\b|DESIGN (says|calls|names|decides|puts|draws|owns)` +
				`|AGENTS(\.md| §)|(^|[^A-Za-z])OSS(\.md| §)|BUSINESS(\.md| §)` +
				`|(^|[^A-Za-z])§ ?[0-9]|INVARIANTS(\.md| §)|IDEAS\.md|(^|[^A-Za-z])V[0-9][a-z]?\.md\b|farm docs|farm/docs|docs/V[0-9]`),
		"tracker item id": regexp.MustCompile(
			`\[(B|M|S|V[0-9][a-z]?|DRBD)\.[0-9]+[a-z]?\]` +
				`|(^|[^A-Za-z0-9_./-])(B|V[0-9][a-z]?|M[0-9]|S[0-9]|DRBD)\.[0-9]+[a-z]?` +
				// What may follow an id: anything that cannot continue a name, a sentence-final dot,
				// or a sub-item range (`B.22b-c`, `B.22b/c`); a dot or slash before more of a name
				// (a version, a path) is not an id.
				`([^0-9A-Za-z_./-]|$|\.([^0-9A-Za-z_]|$)|[-/]([a-z]|[0-9]+[a-z]?)([^0-9A-Za-z_./-]|$))`),
		"maintainer note link": regexp.MustCompile(`\[\[[a-z0-9]+(-[a-z0-9]+)+\]\]`),
	}

	root := moduleRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// Not source: VCS internals, vendored deps, caches and the release staging tree.
			case ".git", ".direnv", "vendor", ".release", ".ruff_cache", ".secrets":
				return fs.SkipDir
			}
			if strings.HasPrefix(d.Name(), "result") {
				return fs.SkipDir
			}
			return nil
		}
		// This file necessarily spells out the shapes it forbids.
		if filepath.Base(path) == "privaterefs_test.go" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			return nil // binary
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(b), "\n") {
			for name, re := range patterns {
				if re.MatchString(line) {
					t.Errorf("%s:%d cites a %s a reader of this repo cannot open; state the rationale in the comment and cite only the public docs",
						rel, i+1, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}
