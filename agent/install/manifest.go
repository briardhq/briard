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
	"strings"
)

// notArtifacts are the channel files that describe the artifact set rather than belong to it:
// the manifest cannot list itself, nor its signature. Nothing else is excluded: a release
// directory holds exactly what its manifest names, the installers included (install.sh;
// install.ps1 and briard-setup.exe on the windows arm) as ordinary artifacts that `promote`
// byte-copies to the channel root. That copy is served unsigned (the one-liner fetches it before
// anything could verify a signature), so this manifest's hash is the only tie to a release.
var notArtifacts = map[string]bool{
	ManifestName:             true,
	ManifestName + sigSuffix: true,
}

// WriteManifest describes every artifact in dir (every regular file but notArtifacts) and writes
// dir/manifest.json, the exact bytes the release then signs and FetchVerified later reads. chain
// names the release line (ChainBriard, ChainVM), platform the arm within it (every chain has one),
// version the release id; all go inside the signed bytes, so a manifest served from a pointer
// path can say which release it is and where its artifacts live. Entries are sorted by name so
// the same directory always produces the same bytes; a signed set that reordered itself would
// churn the signature for no reason. system, minBriard and inputs are the vm chain's extra facts
// (Manifest.System / MinBriard / Inputs) and vm the briard chain's (the vm release it pairs
// with); each is refused on any other chain, because a briard manifest naming a closure would be
// a lie the reader has no way to catch.
//
// ⚠️ This exists so there is one implementation of the format: the release pipeline writes every
// manifest through it (`briard-agent --stage-manifest`), so writer and reader share this file's
// types and the format cannot disagree with itself. A manifest hand-assembled elsewhere (a
// renamed field, a mode written in decimal by a human, a `Size` that drifted from what the reader
// bounds on) would ship a channel no agent could install, with no test on either side to catch it.
func WriteManifest(dir, chain, platform, version, system, minBriard, vm, inputs string) error {
	if !validSegment(chain) {
		return fmt.Errorf("install: bad chain name %q", chain)
	}
	if !validSegment(platform) {
		return fmt.Errorf("install: bad platform name %q", platform)
	}
	// A version id that spells a pointer would make `<chain>/stable/` a release directory and a
	// pointer at once; the tree has no way to say which it is.
	if !validSegment(version) || IsPointer(version) {
		return fmt.Errorf("install: bad release version %q", version)
	}
	if (system != "" || minBriard != "") && chain != ChainVM {
		return fmt.Errorf("install: system/min_briard are vm-chain facts, not %s's", chain)
	}
	if system != "" && !strings.HasPrefix(system, "/nix/store/") {
		return fmt.Errorf("install: system %q is not a /nix/store path", system)
	}
	if vm != "" && chain != ChainBriard {
		return fmt.Errorf("install: vm names the briard chain's pair, not %s's", chain)
	}
	if vm != "" && (!validSegment(vm) || IsPointer(vm)) {
		return fmt.Errorf("install: bad vm release %q", vm)
	}
	if inputs != "" && chain != ChainVM {
		return fmt.Errorf("install: inputs is a vm-chain fact, not %s's", chain)
	}
	if inputs != "" && !validHex(inputs) {
		return fmt.Errorf("install: inputs %q is not a sha256 hex", inputs)
	}
	if minBriard != "" && !validSegment(minBriard) {
		return fmt.Errorf("install: bad min_briard %q", minBriard)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("install: read staging dir %s: %w", dir, err)
	}
	// The floor comes from the tree, never a parameter: every other fact here the pipeline knows
	// and passes in, the floor the code knows, so the binary writing the manifest is the right one
	// to answer it. Briard chain
	// only: the VM image is replaced whole, with no past of its own to be too old for, and a floor
	// there would be a second answer to a question nobody asked yet.
	floor := ""
	if chain == ChainBriard {
		floor = MinUpgradeFrom
	}
	man := Manifest{Chain: chain, Platform: platform, Version: version, System: system, MinBriard: minBriard, VM: vm, Inputs: inputs, MinUpgradeFrom: floor}
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

// validHex is a lowercase sha256 hex string, the shape Manifest.Inputs takes.
func validHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ReadManifest reads one manifest file back -- the bytes WriteManifest wrote, or the exact
// signed bytes FetchVerified kept beside the artifacts. It does not verify: a caller that needs
// the signature checked reads through the Fetcher; this is for a manifest that already did.
func ReadManifest(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("install: parse manifest %s: %w", path, err)
	}
	return m, nil
}
