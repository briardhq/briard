package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoSecretMaterial fails on key material or credentials committed to the tree.
// This is the unrecoverable one: a live token in a public repo cannot be withdrawn
// once pushed, only rotated. GitHub's own push protection covers some of this, but
// only on plans and repo visibilities we may not always be on, and it does not run
// on a laptop before the push -- this does, as part of `go test ./...`.
//
// The tree was clean when this was written, so the guard is purely prospective.
//
// It replaced a wider "leak gate" that also banned references to the private
// design docs and to maintainer identifiers. Both were dropped once the repo split
// landed: a confidentiality check that runs *in* the public repo fires after the
// content is already published, so it could never do the job it was named for, and
// the identifier check had to name the identifiers it was hiding.
func TestNoSecretMaterial(t *testing.T) {
	patterns := map[string]*regexp.Regexp{
		"private key block": regexp.MustCompile(`BEGIN (RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY`),
		"certificate block": regexp.MustCompile(`BEGIN CERTIFICATE`),
		"github token":      regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),
		"gitlab token":      regexp.MustCompile(`glpat-[A-Za-z0-9_-]{16,}`),
		"aws access key":    regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		"slack token":       regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	}

	root := moduleRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Not source: VCS internals, build outputs, and the direnv cache.
			switch d.Name() {
			case ".git", ".direnv":
				return fs.SkipDir
			}
			if strings.HasPrefix(d.Name(), "result") {
				return fs.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".nix", ".sh", ".md", ".yml", ".yaml":
		default:
			return nil
		}
		// This file necessarily contains the very patterns it forbids.
		if filepath.Base(path) == "secrets_test.go" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(b), "\n") {
			for name, re := range patterns {
				if re.MatchString(line) {
					t.Errorf("%s:%d looks like committed %s; secrets belong in .env (gitignored), never the tree",
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
