package install

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/agent/selfupdate"
)

func man(chain, platform, version string) Manifest {
	return Manifest{Chain: chain, Platform: platform, Version: version, Artifacts: []Entry{{Name: "briard-agent"}}}
}

// The comparison rules of [B.86a], one row each, including the ones that must REFUSE.
func TestDecide(t *testing.T) {
	host := func(v string) *Manifest { m := man(ChainHost, PlatformLinux, v); return &m }
	old, cur, next := "v3.20260901.aaaaaaa", "v3.20260905.bbbbbbb", "v3.20260910.ccccccc"
	sameDay := "v3.20260905.ddddddd"
	stable := host(cur)
	for _, tc := range []struct {
		name    string
		target  string
		want    Manifest
		have    *Manifest
		stable  *Manifest
		install bool
		err     error
	}{
		{"stable newer date installs", TargetStable, *host(next), host(cur), nil, true, nil},
		{"stable same date is a no-op (no same-date rule)", TargetStable, *host(sameDay), host(cur), nil, false, nil},
		{"stable older date is a no-op (pins survive the timer)", TargetStable, *host(old), host(cur), nil, false, nil},
		{"stable with no installed manifest installs", TargetStable, *host(cur), nil, nil, true, nil},
		{"latest equal id is a no-op", TargetLatest, *host(cur), host(cur), nil, false, nil},
		{"latest different id installs, hash included", TargetLatest, *host(sameDay), host(cur), nil, true, nil},
		{"exact newer than stable installs", next, *host(next), host(cur), stable, true, nil},
		{"exact equal to installed is a no-op", cur, *host(cur), host(cur), stable, false, nil},
		{"exact older than installed but at stable's date installs (downgrade to the floor)", sameDay, *host(sameDay), host(next), stable, true, nil},
		{"exact below stable is refused", old, *host(old), host(cur), stable, false, ErrBelowFloor},
		{"exact with no stable to floor against is refused", next, *host(next), host(cur), nil, false, ErrBelowFloor},
		{"exact whose manifest names another version is refused", next, *host(cur), host(old), stable, false, ErrManifest},
		{"a crossed chain is refused, not compared", TargetStable, *host(next), func() *Manifest { m := man(ChainGuest, "", "guest.20260901.x"); return &m }(), nil, false, ErrWrongChain},
		{"a non-numeric date field is refused", TargetStable, *host("v3.dirty"), host(cur), nil, false, ErrManifest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Decide(tc.target, tc.want, tc.have, tc.stable)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Install != tc.install {
				t.Errorf("install = %v (%s), want %v", d.Install, d.Reason, tc.install)
			}
			if d.Reason == "" {
				t.Error("a decision must say why")
			}
		})
	}
}

// updateFixture is a channel plus a layout to stage into: a fresh Base (same fs as the
// candidate) and RunDir, with the installed manifest optionally seeded.
func updateFixture(t *testing.T, c *channel, installed []byte) (*Update, selfupdate.Layout) {
	t.Helper()
	base := t.TempDir()
	l := selfupdate.New(base, filepath.Join(t.TempDir(), "run"))
	if installed != nil {
		if err := os.WriteFile(l.ManifestPath(), installed, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Update{Fetcher: c.fetcher(), Layout: l}, l
}

func assertNothingStaged(t *testing.T, l selfupdate.Layout) {
	t.Helper()
	if l.NextStaged() || l.Armed() {
		t.Error("a refused update staged or armed something — refuse-and-stay violated")
	}
	if _, err := os.Stat(l.NextManifestPath()); !os.IsNotExist(err) {
		t.Error("a refused update left a candidate manifest")
	}
	ents, _ := os.ReadDir(l.Base)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".update-") {
			t.Errorf("orphan temp dir %s", e.Name())
		}
	}
}

// The happy path: a node with no installed manifest asks for `latest`; the agent artifact is
// fetched from the versioned directory, verified, staged beside its manifest, and armed — and
// nothing is restarted (the verb has no such power).
func TestUpdateStagesAndArms(t *testing.T) {
	c := goodChannel(t)
	u, l := updateFixture(t, c, nil)
	line, err := u.Run(context.Background(), TargetLatest)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(line, "staged "+testVersion) || !strings.Contains(line, "armed") {
		t.Errorf("result line = %q", line)
	}
	got, err := os.ReadFile(l.NextPath())
	if err != nil || string(got) != "the briard-agent static binary" {
		t.Fatalf("agent.next = %q, %v", got, err)
	}
	fi, _ := os.Stat(l.NextPath())
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("agent.next mode = %o, want 0755", fi.Mode().Perm())
	}
	nm, err := os.ReadFile(l.NextManifestPath())
	if err != nil || !bytes.Equal(nm, c.bodies[pointerPath(ManifestName)]) {
		t.Errorf("manifest.json.next is not the verified bytes (%v)", err)
	}
	if !l.Armed() {
		t.Error("not armed")
	}
	// The other artifacts of the release were NOT fetched: the verb's scope is the agent alone.
	for _, n := range []string{"nixos.qcow2", "qemu-bundle.tar"} {
		if _, err := os.Stat(filepath.Join(l.Base, n)); !os.IsNotExist(err) {
			t.Errorf("%s was fetched by the agent-only verb", n)
		}
	}
	assertNoOrphans(t, l)
}

func assertNoOrphans(t *testing.T, l selfupdate.Layout) {
	t.Helper()
	ents, _ := os.ReadDir(l.Base)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".update-") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("orphan %s left in the base dir", e.Name())
		}
	}
}

// An installed manifest equal to the target is a no-op: nothing staged, nothing armed, and the
// line says so — `briard update host` on an up-to-date node must not bounce the agent.
func TestUpdateIsANoOpAtTheTarget(t *testing.T) {
	c := goodChannel(t)
	u, l := updateFixture(t, c, c.bodies[pointerPath(ManifestName)])
	line, err := u.Run(context.Background(), TargetLatest)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(line, "already at "+testVersion) {
		t.Errorf("result line = %q", line)
	}
	assertNothingStaged(t, l)
}

// The stable path orders on the date: an installed release NEWER than stable (a pin) is left
// alone, an older one is moved forward.
func TestUpdateStableOrdersOnTheDate(t *testing.T) {
	c := goodChannel(t)
	// Serve the same signed manifest under `stable`.
	for _, n := range []string{ManifestName, ManifestName + sigSuffix} {
		c.bodies[ChainHost+"/"+TargetStable+"/"+PlatformLinux+"/"+n] = c.bodies[pointerPath(n)]
	}
	pinned, _ := json.Marshal(man(ChainHost, PlatformLinux, "v3.20260930.fffffff"))
	u, l := updateFixture(t, c, pinned)
	line, err := u.Run(context.Background(), TargetStable)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(line, "nothing to do") {
		t.Errorf("a node ahead of stable was moved: %q", line)
	}
	assertNothingStaged(t, l)

	older, _ := json.Marshal(man(ChainHost, PlatformLinux, "v3.20260101.0000000"))
	u2, l2 := updateFixture(t, c, older)
	if _, err := u2.Run(context.Background(), TargetStable); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !l2.Armed() {
		t.Error("a node behind stable was not moved forward")
	}
}

// An exact pin below stable is refused loudly, with nothing staged — the floor.
func TestUpdateRefusesAPinBelowStable(t *testing.T) {
	c := goodChannel(t)
	// stable = the fixture's release; publish an OLDER exact version beside it.
	for _, n := range []string{ManifestName, ManifestName + sigSuffix} {
		c.bodies[ChainHost+"/"+TargetStable+"/"+PlatformLinux+"/"+n] = c.bodies[pointerPath(n)]
	}
	oldV := "v3.20250101.0ld0ld0"
	agent := []byte("an old agent")
	mb, _ := json.Marshal(Manifest{Chain: ChainHost, Platform: PlatformLinux, Version: oldV,
		Artifacts: []Entry{{Name: "briard-agent", SHA256: sha(agent), Size: int64(len(agent)), Mode: 0o755}}})
	c.bodies[ChainHost+"/"+oldV+"/"+PlatformLinux+"/"+ManifestName] = mb
	c.bodies[ChainHost+"/"+oldV+"/"+PlatformLinux+"/"+ManifestName+sigSuffix] = ed25519.Sign(c.priv, mb)
	c.bodies[ChainHost+"/"+oldV+"/"+PlatformLinux+"/briard-agent"] = agent
	u, l := updateFixture(t, c, c.bodies[pointerPath(ManifestName)])
	_, err := u.Run(context.Background(), oldV)
	if !errors.Is(err, ErrBelowFloor) {
		t.Fatalf("err = %v, want ErrBelowFloor", err)
	}
	assertNothingStaged(t, l)

	// The failable control: with stable MOVED to the old version, the same pin is accepted.
	c.bodies[ChainHost+"/"+TargetStable+"/"+PlatformLinux+"/"+ManifestName] = mb
	c.bodies[ChainHost+"/"+TargetStable+"/"+PlatformLinux+"/"+ManifestName+sigSuffix] = ed25519.Sign(c.priv, mb)
	if _, err := u.Run(context.Background(), oldV); err != nil {
		t.Fatalf("a pin at the moved floor was refused: %v", err)
	}
	if !l.Armed() {
		t.Error("downgrade to the floor did not arm")
	}
}

// A tampered agent artifact is refused after the manifest verified: nothing staged, nothing
// armed. [[verification-assertions-must-fail]]
func TestUpdateRefusesATamperedAgent(t *testing.T) {
	c := goodChannel(t)
	c.bodies[releasePath("briard-agent")] = []byte("not the signed bytes")
	u, l := updateFixture(t, c, nil)
	_, err := u.Run(context.Background(), TargetLatest)
	if !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("err = %v, want ErrArtifactMismatch", err)
	}
	assertNothingStaged(t, l)
}

// An id the channel does not carry fails the run loudly (a 404 is not a no-op).
func TestUpdateRefusesAnUnknownPin(t *testing.T) {
	c := goodChannel(t)
	u, l := updateFixture(t, c, nil)
	if _, err := u.Run(context.Background(), "v3.20990101.nothere"); err == nil {
		t.Fatal("an unknown pin was accepted")
	}
	assertNothingStaged(t, l)
}
