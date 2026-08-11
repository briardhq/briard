package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// notArtifacts are the channel files that describe or accompany the artifact set rather than
// belong to it. install.sh is fetched by the one-liner before any verification exists (so it
// cannot be under the signature it would be verifying), and the manifest cannot list itself.
var notArtifacts = map[string]bool{
	ManifestName:             true,
	ManifestName + sigSuffix: true,
	"install.sh":             true,
	"VERSION":                true,
}

// WriteManifest describes every artifact in dir and writes dir/manifest.json — the exact bytes
// the release then signs and FetchVerified later reads.
//
// ⚠️ THIS EXISTS SO THERE IS ONE IMPLEMENTATION OF THE FORMAT. It used to be a printf loop in
// scripts/publish-release.sh, hand-assembling JSON — including `"mode":493`, which is 0o755
// written in decimal by a human. That made the manifest a contract between a shell script and a
// Go struct with nothing holding them together: a renamed field, a mode that stopped being
// hand-converted, or a `Size` that drifted from what the reader bounds on would have shipped a
// channel no agent could install, and no test either side could catch. The writer and the reader
// now share this file's types, so the format cannot disagree with itself.
//
// Every regular file in dir is an artifact except the ones that describe the set (notArtifacts).
// Entries are sorted by name so the same directory always produces the same bytes — the manifest
// is signed, and a set that reordered itself would churn the signature for no reason.
func WriteManifest(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("install: read staging dir %s: %w", dir, err)
	}
	var man Manifest
	for _, e := range ents {
		if e.IsDir() || notArtifacts[e.Name()] {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return fmt.Errorf("install: stat %s: %w", e.Name(), err)
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		sum, err := sha256File(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		a := Entry{Name: e.Name(), SHA256: sum, Size: fi.Size()}
		// Mode is emitted only where it is not the 0644 default, matching `omitempty` on the
		// reading side — and taken from the file itself rather than from a table somebody has to
		// keep in step with what was actually installed.
		if perm := fi.Mode().Perm(); perm != 0o644 {
			a.Mode = uint32(perm)
		}
		man.Artifacts = append(man.Artifacts, a)
	}
	if len(man.Artifacts) == 0 {
		return fmt.Errorf("install: %s holds no artifacts to describe", dir)
	}
	sort.Slice(man.Artifacts, func(i, j int) bool { return man.Artifacts[i].Name < man.Artifacts[j].Name })

	b, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("install: marshal manifest: %w", err)
	}
	// Written whole, not streamed: the signature covers these exact bytes, so a half-written
	// manifest must never be signable.
	if err := os.WriteFile(filepath.Join(dir, ManifestName), b, 0o644); err != nil {
		return fmt.Errorf("install: write manifest: %w", err)
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("install: open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("install: hash %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
