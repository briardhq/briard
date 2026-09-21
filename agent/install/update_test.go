package install

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"briard.io/agent/selfupdate"
)

func man(chain, platform, version string) Manifest {
	return Manifest{Chain: chain, Platform: platform, Version: version, Artifacts: []Entry{{Name: "briard-agent"}}}
}

// The comparison rules of [B.86a], one row each, including the ones that must REFUSE.
func TestDecide(t *testing.T) {
	host := func(v string) *Manifest { m := man(ChainBriard, PlatformLinux, v); return &m }
	hostFloor := func(v, floor string) *Manifest {
		m := man(ChainBriard, PlatformLinux, v)
		m.MinUpgradeFrom = floor
		return &m
	}
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
		{"a crossed chain is refused, not compared", TargetStable, *host(next), func() *Manifest { m := man(ChainVM, "", "vm.20260901.x"); return &m }(), nil, false, ErrWrongChain},
		{"a non-numeric date field is refused", TargetStable, *host("v3.dirty"), host(cur), nil, false, ErrManifest},
		// THE UPGRADE FLOOR ([B.159](e)). The floor is a fact about (installed, offered), not
		// about the target word, so all three targets are floored -- the release cannot complete
		// the upgrade whichever word asked for it. A fresh install has no past to be too old for.
		{"the floor refuses an older installed release on stable", TargetStable, *hostFloor(next, cur), host(old), nil, false, ErrTooOldToUpgrade},
		{"the floor refuses on latest too -- no target crosses it", TargetLatest, *hostFloor(next, cur), host(old), nil, false, ErrTooOldToUpgrade},
		{"the floor refuses an exact pin too", next, *hostFloor(next, cur), host(old), stable, false, ErrTooOldToUpgrade},
		{"at the floor's own date is allowed (dates, not ids -- the accepted blind spot)", TargetStable, *hostFloor(next, cur), host(sameDay), nil, true, nil},
		{"above the floor is allowed", TargetStable, *hostFloor(next, old), host(cur), nil, true, nil},
		{"a fresh install is never floored", TargetStable, *hostFloor(next, cur), nil, nil, true, nil},
		{"no floor declared is the normal state", TargetStable, *host(next), host(old), nil, true, nil},
		{"a floor with no date field is a malformed manifest, not a refusal", TargetStable, *hostFloor(next, "v3.dirty"), host(old), nil, false, ErrManifest},
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
	// Artifacts the verb does not know (a guest image here, a plain tarball) are left alone.
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
// line says so — `briard update` on an up-to-date node must not bounce the agent.
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
		c.bodies[ChainBriard+"/"+TargetStable+"/"+PlatformLinux+"/"+n] = c.bodies[pointerPath(n)]
	}
	pinned, _ := json.Marshal(man(ChainBriard, PlatformLinux, "v3.20260930.fffffff"))
	u, l := updateFixture(t, c, pinned)
	line, err := u.Run(context.Background(), TargetStable)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(line, "nothing to do") {
		t.Errorf("a node ahead of stable was moved: %q", line)
	}
	assertNothingStaged(t, l)

	older, _ := json.Marshal(man(ChainBriard, PlatformLinux, "v3.20260101.0000000"))
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
		c.bodies[ChainBriard+"/"+TargetStable+"/"+PlatformLinux+"/"+n] = c.bodies[pointerPath(n)]
	}
	oldV := "v3.20250101.0ld0ld0"
	agent := []byte("an old agent")
	mb, _ := json.Marshal(Manifest{Chain: ChainBriard, Platform: PlatformLinux, Version: oldV,
		Artifacts: []Entry{{Name: "briard-agent", SHA256: sha(agent), Size: int64(len(agent)), Mode: 0o755}}})
	c.bodies[ChainBriard+"/"+oldV+"/"+PlatformLinux+"/"+ManifestName] = mb
	c.bodies[ChainBriard+"/"+oldV+"/"+PlatformLinux+"/"+ManifestName+sigSuffix] = ed25519.Sign(c.priv, mb)
	c.bodies[ChainBriard+"/"+oldV+"/"+PlatformLinux+"/briard-agent"] = agent
	u, l := updateFixture(t, c, c.bodies[pointerPath(ManifestName)])
	_, err := u.Run(context.Background(), oldV)
	if !errors.Is(err, ErrBelowFloor) {
		t.Fatalf("err = %v, want ErrBelowFloor", err)
	}
	assertNothingStaged(t, l)

	// The failable control: with stable MOVED to the old version, the same pin is accepted.
	c.bodies[ChainBriard+"/"+TargetStable+"/"+PlatformLinux+"/"+ManifestName] = mb
	c.bodies[ChainBriard+"/"+TargetStable+"/"+PlatformLinux+"/"+ManifestName+sigSuffix] = ed25519.Sign(c.priv, mb)
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

// bundleChannel is a release of the WHOLE host bundle: an agent, a net-wrap, and a real (tiny)
// qemu-bundle.tar.zst whose tree holds bin/qemu-system-x86_64 and PROVENANCE.
func bundleChannel(t *testing.T, provenance string) *channel {
	t.Helper()
	agent := []byte("the briard-agent static binary")
	wrap := []byte("#!/bin/sh\nexec \"$@\"\n")
	qemu := tarZst(t, map[string]string{"bin/qemu-system-x86_64": "#!/bin/sh\necho QEMU\n", "PROVENANCE": provenance})
	arts := []Entry{
		{Name: artifactAgent, SHA256: sha(agent), Size: int64(len(agent)), Mode: 0o755},
		{Name: artifactNetWrap, SHA256: sha(wrap), Size: int64(len(wrap)), Mode: 0o755},
		{Name: artifactQEMU, SHA256: sha(qemu), Size: int64(len(qemu))},
	}
	return newChannel(t, arts, map[string][]byte{artifactAgent: agent, artifactNetWrap: wrap, artifactQEMU: qemu})
}

// tarZst builds a `tar -C bundle .`-shaped archive (./bin/..., the way publish-release.sh lays
// it) and compresses it the way the channel ships it.
func tarZst(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "./bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	for _, name := range []string{"bin/qemu-system-x86_64", "PROVENANCE"} {
		body := files[name]
		mode := int64(0o644)
		if strings.HasPrefix(name, "bin/") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: mode, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var zb bytes.Buffer
	zw, err := zstd.NewWriter(&zb)
	if err != nil {
		t.Fatal(err)
	}
	zw.Write(tb.Bytes())
	zw.Close()
	return zb.Bytes()
}

func assertBundleNotStaged(t *testing.T, l selfupdate.Layout) {
	t.Helper()
	if l.NextQEMUStaged() {
		t.Error("qemu.next is staged")
	}
	if _, err := os.Lstat(l.NextNetWrapPath()); !os.IsNotExist(err) {
		t.Error("briard-net-wrap.next is staged")
	}
}

// The whole bundle ([B.86b]): with no installed manifest every artifact is fetched; the
// net-wrap stages beside the agent, the qemu tarball is verified, expanded and extracted into
// qemu-<version>/ and qemu.next links to it (relative), the manifest rides along, and the
// result line names what was staged.
func TestUpdateStagesTheWholeBundle(t *testing.T) {
	c := bundleChannel(t, "prefix=/opt/briard/qemu")
	u, l := updateFixture(t, c, nil)
	line, err := u.Run(context.Background(), TargetLatest)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(line, "staged "+testVersion+" (agent, net-wrap, qemu), armed") {
		t.Errorf("result line = %q", line)
	}
	if b, err := os.ReadFile(l.NextNetWrapPath()); err != nil || !strings.HasPrefix(string(b), "#!/bin/sh") {
		t.Errorf("net-wrap.next = %q, %v", b, err)
	}
	if fi, _ := os.Stat(l.NextNetWrapPath()); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("net-wrap.next is not 0755: %v", fi)
	}
	tree, ok := l.NextQEMUTree()
	if !ok || tree != l.QEMUTree(testVersion) {
		t.Fatalf("qemu.next -> %q, %v; want %s", tree, ok, l.QEMUTree(testVersion))
	}
	if target, _ := os.Readlink(l.NextQEMUPath()); target != "qemu-"+testVersion {
		t.Errorf("qemu.next target = %q, want the relative qemu-%s", target, testVersion)
	}
	if b, err := os.ReadFile(filepath.Join(tree, "PROVENANCE")); err != nil || string(b) != "prefix=/opt/briard/qemu" {
		t.Errorf("extracted PROVENANCE = %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(tree, "bin", "qemu-system-x86_64")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("extracted qemu binary: %v, %v", fi, err)
	}
	if fi, _ := os.Stat(tree); fi.Mode().Perm() != 0o755 {
		t.Errorf("tree mode = %o, want 0755", fi.Mode().Perm())
	}
	if !l.NextStaged() || !l.Armed() {
		t.Error("agent not staged or not armed")
	}
	// The tarball itself does not linger anywhere under Base.
	for _, n := range []string{artifactQEMU, strings.TrimSuffix(artifactQEMU, ".zst")} {
		if _, err := os.Stat(filepath.Join(l.Base, n)); !os.IsNotExist(err) {
			t.Errorf("%s left under Base", n)
		}
	}
	assertNoOrphans(t, l)
}

// Download only what changed: an installed manifest pinning net-wrap and qemu at the SAME
// hashes as the target means neither is fetched -- proven by making both 404, which the run
// must never notice -- and neither is staged; the agent alone moves.
func TestUpdateFetchesOnlyWhatChanged(t *testing.T) {
	c := bundleChannel(t, "same")
	var target Manifest
	json.Unmarshal(c.bodies[pointerPath(ManifestName)], &target)
	installed := target
	installed.Version = "v3.20260101.0000000"
	ib, _ := json.Marshal(installed)
	c.missing[releasePath(artifactNetWrap)] = true
	c.missing[releasePath(artifactQEMU)] = true
	u, l := updateFixture(t, c, ib)
	line, err := u.Run(context.Background(), TargetLatest)
	if err != nil {
		t.Fatalf("an unchanged net-wrap/qemu was fetched (or worse): %v", err)
	}
	if !strings.Contains(line, "staged "+testVersion+" (agent), armed") {
		t.Errorf("result line = %q", line)
	}
	if !l.NextStaged() {
		t.Error("agent not staged")
	}
	assertBundleNotStaged(t, l)
	if _, err := os.Stat(l.QEMUTree(testVersion)); !os.IsNotExist(err) {
		t.Error("a qemu tree was extracted for an unchanged qemu")
	}
}

// A stale bundle candidate -- qemu.next and net-wrap.next left by a release whose trial
// failed -- is dropped before a release that does NOT change them is staged; otherwise
// briard-commit would pair this agent with that qemu, a combination nothing tested.
func TestUpdateDropsAStaleBundleCandidate(t *testing.T) {
	c := bundleChannel(t, "same")
	var target Manifest
	json.Unmarshal(c.bodies[pointerPath(ManifestName)], &target)
	installed := target
	installed.Version = "v3.20260101.0000000"
	ib, _ := json.Marshal(installed)
	u, l := updateFixture(t, c, ib)
	stale := l.QEMUTree("v3.20260201.5ta1e00")
	os.MkdirAll(stale, 0o755)
	if err := l.StageNextQEMU(stale); err != nil {
		t.Fatal(err)
	}
	if err := l.StageNextNetWrap(strings.NewReader("stale")); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Run(context.Background(), TargetLatest); err != nil {
		t.Fatalf("update: %v", err)
	}
	assertBundleNotStaged(t, l)
	if _, err := os.Stat(stale); err != nil {
		t.Error("the stale TREE was removed by the verb; pruning belongs to the agent at launch")
	}
}

// A tree already extracted for this version (a failed trial's) is reused without a fetch, and
// a qemu link already committed at this version stages nothing for qemu.
func TestUpdateReusesAnExtractedQEMUTree(t *testing.T) {
	c := bundleChannel(t, "fresh")
	c.missing[releasePath(artifactQEMU)] = true // a fetch would fail loudly
	u, l := updateFixture(t, c, nil)
	tree := l.QEMUTree(testVersion)
	os.MkdirAll(tree, 0o755)
	os.WriteFile(filepath.Join(tree, "PROVENANCE"), []byte("from the failed trial"), 0o644)
	line, err := u.Run(context.Background(), TargetLatest)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(line, "(agent, net-wrap, qemu)") {
		t.Errorf("result line = %q", line)
	}
	if got, _ := l.NextQEMUTree(); got != tree {
		t.Errorf("qemu.next -> %q, want the reused %s", got, tree)
	}
	if b, _ := os.ReadFile(filepath.Join(tree, "PROVENANCE")); string(b) != "from the failed trial" {
		t.Error("the existing tree was replaced rather than reused")
	}

	// Committed at this version already (a node whose manifest was lost): qemu is left alone.
	u2, l2 := updateFixture(t, c, nil)
	os.MkdirAll(l2.QEMUTree(testVersion), 0o755)
	os.Symlink("qemu-"+testVersion, l2.QEMUPath())
	line, err = u2.Run(context.Background(), TargetLatest)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(line, "(agent, net-wrap)") || l2.NextQEMUStaged() {
		t.Errorf("a committed qemu was re-staged: %q", line)
	}
}

// Refuse-and-stay covers the bundle: a tampered qemu tarball (fetched AFTER the agent) leaves
// no agent.next, no net-wrap.next, no qemu.next, no tree and no arm. And a tarball that
// verifies but will not unpack is refused the same way.
func TestUpdateRefusesATamperedOrBrokenBundle(t *testing.T) {
	c := bundleChannel(t, "x")
	c.bodies[releasePath(artifactQEMU)] = []byte("not the signed bytes")
	u, l := updateFixture(t, c, nil)
	if _, err := u.Run(context.Background(), TargetLatest); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("err = %v, want ErrArtifactMismatch", err)
	}
	assertNothingStaged(t, l)
	assertBundleNotStaged(t, l)
	if _, err := os.Stat(l.QEMUTree(testVersion)); !os.IsNotExist(err) {
		t.Error("a tree was extracted from a tampered tarball")
	}

	// Signed but not a tarball: refused at unpack, nothing staged.
	junk := []byte("this is not a tar")
	var zb bytes.Buffer
	zw, _ := zstd.NewWriter(&zb)
	zw.Write(junk)
	zw.Close()
	c2 := bundleChannel(t, "y")
	var m Manifest
	json.Unmarshal(c2.bodies[pointerPath(ManifestName)], &m)
	for i := range m.Artifacts {
		if m.Artifacts[i].Name == artifactQEMU {
			m.Artifacts[i].SHA256, m.Artifacts[i].Size = sha(zb.Bytes()), int64(zb.Len())
		}
	}
	mb, _ := json.Marshal(m)
	for _, p := range []string{pointerPath(ManifestName), releasePath(ManifestName)} {
		c2.bodies[p] = mb
		c2.bodies[p+sigSuffix] = ed25519.Sign(c2.priv, mb)
	}
	c2.bodies[releasePath(artifactQEMU)] = zb.Bytes()
	u2, l2 := updateFixture(t, c2, nil)
	_, err := u2.Run(context.Background(), TargetLatest)
	if err == nil || !strings.Contains(err.Error(), "unpack") {
		t.Fatalf("err = %v, want an unpack refusal", err)
	}
	assertNothingStaged(t, l2)
	assertBundleNotStaged(t, l2)
	if _, err := os.Stat(l2.QEMUTree(testVersion)); !os.IsNotExist(err) {
		t.Error("a tree appeared from a tarball that would not unpack")
	}
}

// min_briard ([B.86d]): the vm chain's one-directional compatibility promise, ordered on the
// date like everything else; a host id with no date cannot satisfy any requirement.
func TestBriardSatisfies(t *testing.T) {
	for _, tc := range []struct {
		minHost, host string
		ok            bool
	}{
		{"", "v3.20260906.a", true},
		{"v3.20260906.a", "v3.20260906.b", true},
		{"v3.20260906.a", "v3.20260907.b", true},
		{"v3.20260907.a", "v3.20260906.b", false},
		{"v3.20260906.a", "dev", false},
		{"v3.20260906.a", "", false},
	} {
		err := BriardSatisfies(tc.minHost, tc.host)
		if (err == nil) != tc.ok {
			t.Errorf("BriardSatisfies(%q, %q) = %v, want ok=%v", tc.minHost, tc.host, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrBriardTooOld) {
			t.Errorf("BriardSatisfies(%q, %q) = %v, want ErrBriardTooOld", tc.minHost, tc.host, err)
		}
	}
}

// The vm manifest names its closure and min_briard, round-tripped through the one writer and
// reader; the host chain refuses them.
func TestWriteManifestCarriesTheGuestFacts(t *testing.T) {
	stage := t.TempDir()
	os.WriteFile(filepath.Join(stage, "nixos.qcow2.zst"), []byte("img"), 0o644)
	if err := WriteManifest(stage, ChainVM, "", "vm.20260906.abc1234", "/nix/store/abc-nixos-system", "v3.20260906.abc1234", "", ""); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	b, _ := os.ReadFile(filepath.Join(stage, ManifestName))
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.System != "/nix/store/abc-nixos-system" || m.MinBriard != "v3.20260906.abc1234" {
		t.Errorf("system/min_briard = %q/%q", m.System, m.MinBriard)
	}
	for _, bad := range [][]string{
		{ChainBriard, "/nix/store/x", ""}, // a host manifest naming a closure
		{ChainVM, "/tmp/not-store", ""},   // not a store path
		{ChainVM, "", "not a segment/"},   // an unusable min_briard
	} {
		if err := WriteManifest(stage, bad[0], "", "vm.20260906.abc1234", bad[1], bad[2], "", ""); err == nil {
			t.Errorf("WriteManifest(%v) accepted", bad)
		}
	}
	// Omitted when empty, so a host manifest's bytes are unchanged by the fields' existence.
	if err := WriteManifest(stage, ChainVM, "", "vm.20260906.abc1234", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(stage, ManifestName))
	if bytes.Contains(b, []byte("system")) || bytes.Contains(b, []byte("min_briard")) {
		t.Errorf("empty guest facts were emitted: %s", b)
	}
}

// The pairing fields ([B.86i]): a host manifest names its guest release, a guest manifest its
// inputs hash, and neither is accepted on the other chain -- a guest manifest naming a guest, or a
// host manifest carrying an inputs hash, would be a lie the reader has no way to catch.
func TestWriteManifestPairingFields(t *testing.T) {
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "briard-agent"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	inputs := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := WriteManifest(stage, ChainBriard, PlatformLinux, "v3.20260907.abc1234", "", "", "vm.20260901.def5678", ""); err != nil {
		t.Fatalf("host manifest naming its guest refused: %v", err)
	}
	m, err := ReadManifest(filepath.Join(stage, ManifestName))
	if err != nil || m.VM != "vm.20260901.def5678" || m.Inputs != "" {
		t.Fatalf("host manifest read back as %+v (%v)", m, err)
	}
	if err := WriteManifest(stage, ChainVM, "", "vm.20260901.def5678", "/nix/store/abc-sys", "v3.20260907.abc1234", "", inputs); err != nil {
		t.Fatalf("guest manifest carrying its inputs refused: %v", err)
	}
	if m, err = ReadManifest(filepath.Join(stage, ManifestName)); err != nil || m.Inputs != inputs || m.VM != "" {
		t.Fatalf("guest manifest read back as %+v (%v)", m, err)
	}
	for _, bad := range [][]string{
		{ChainVM, "vm.20260901.def5678", ""}, // a guest naming a guest
		{ChainBriard, "", inputs},            // a host carrying inputs
		{ChainBriard, "stable", ""},          // a pointer word as the pair
		{ChainVM, "", "not-a-hash"},          // inputs that are not a sha256
	} {
		if err := WriteManifest(stage, bad[0], "", "v3.20260907.abc1234", "", "", bad[1], bad[2]); err == nil {
			t.Errorf("WriteManifest(%v) accepted", bad)
		}
	}
}

// [B.159](a) install.sh is an ORDINARY ARTIFACT of the host chain's linux arm, hashed and
// mode-recorded like every other file in the directory.
//
// This looks tautological -- WriteManifest walks the directory, so of course it is included --
// and that is exactly why it is worth pinning. The excluded set is a hand-written map, and the
// comment beside it used to say install.sh lived outside every chain; an agent re-reading that
// sentence and "tidying" the installer back out of the manifest would break the one property the
// unsigned root copy has. `verify` asserts the root equals these bytes, so no hash here means
// nothing at all ties what a stranger curls to a release we signed.
func TestWriteManifestCarriesTheInstaller(t *testing.T) {
	stage := t.TempDir()
	os.WriteFile(filepath.Join(stage, "briard-agent"), []byte("agent"), 0o755)
	os.WriteFile(filepath.Join(stage, "install.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	if err := WriteManifest(stage, ChainBriard, PlatformLinux, "v3.20260920.abc1234", "", "", "vm.20260920.def5678", ""); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	b, _ := os.ReadFile(filepath.Join(stage, ManifestName))
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var got *Entry
	for i := range m.Artifacts {
		if m.Artifacts[i].Name == "install.sh" {
			got = &m.Artifacts[i]
		}
	}
	if got == nil {
		t.Fatalf("install.sh is not in the host manifest, so the root copy is pinned by nothing: %+v", m.Artifacts)
	}
	// The mode matters as much as the hash: a 0644 installer is one a node cannot run, and the
	// manifest records a mode only when it is not the default, so an omitted one is a real bug.
	if got.Mode != 0o755 {
		t.Errorf("install.sh mode = %#o, want 0755 (the manifest records the executable bit)", got.Mode)
	}
	if got.SHA256 == "" || got.Size == 0 {
		t.Errorf("install.sh entry carries no hash or size: %+v", got)
	}
}

// THE KIND IS A STRING THE JOURNAL AND A HUMAN BOTH READ ([B.159](i)), so its VALUE is pinned
// here and not only its identifier. Every Go call site names the constant, which means a changed
// value compiles, passes every table above, and breaks exactly two things this suite cannot see:
// `briard directive <kind>` typed by hand at a node, and the fleet tests that wait on
// `directive update-vm …` lines by text (lab/tests/integration/os-reboot.sh, os-rollback.sh).
// Those waits fail by TIMEOUT, so without this the cheapest signal for a one-character slip is a
// fleet run measured in tens of minutes.
func TestUpdateVMDirectiveKindIsWhatTheJournalSays(t *testing.T) {
	if DirectiveUpdateVM != "update-vm" {
		t.Fatalf("DirectiveUpdateVM = %q — the fleet tests wait on `directive update-vm` by text", DirectiveUpdateVM)
	}
}

// THE FLOOR IS A FACT ABOUT THE TREE, so the binary that writes a manifest is the one that
// answers it ([B.159](e)) -- there is no flag and nothing for a publish to remember. Host chain
// only: the guest image is replaced whole and has no past of its own to be too old for.
//
// ⚠️ The floor is EMPTY in this tree, which is the normal state and also why this test sets it:
// a wiring assertion against a value that never varies could not fail, and the failure it exists
// to catch is exactly the silent one -- a manifest published with no floor on a release that
// declared one, which refuses nothing and is indistinguishable from a release that declared
// nothing.
func TestWriteManifestCarriesTheTreesFloor(t *testing.T) {
	was := MinUpgradeFrom
	t.Cleanup(func() { MinUpgradeFrom = was })
	MinUpgradeFrom = "v3.20260920.abc1234"

	read := func(t *testing.T, chain, platform, version, system, minHost string) Manifest {
		t.Helper()
		stage := t.TempDir()
		os.WriteFile(filepath.Join(stage, "artifact"), []byte("x"), 0o644)
		if err := WriteManifest(stage, chain, platform, version, system, minHost, "", ""); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(stage, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if chain == ChainBriard && !strings.Contains(string(b), `"min_upgrade_from"`) {
			t.Errorf("the host manifest JSON carries no min_upgrade_from key: %s", b)
		}
		if chain == ChainVM && strings.Contains(string(b), `"min_upgrade_from"`) {
			t.Errorf("the guest manifest JSON carries a min_upgrade_from key: %s", b)
		}
		return m
	}

	if got := read(t, ChainBriard, PlatformLinux, "v3.20260921.bbb2222", "", "").MinUpgradeFrom; got != MinUpgradeFrom {
		t.Errorf("host min_upgrade_from = %q, want the tree's %q", got, MinUpgradeFrom)
	}
	if got := read(t, ChainVM, "", "vm.20260921.bbb2222", "/nix/store/x-nixos-system", "").MinUpgradeFrom; got != "" {
		t.Errorf("guest min_upgrade_from = %q, want empty — the guest chain declares no floor", got)
	}
}

// THE REFUSAL IS A PRODUCT SURFACE, not just an error value ([B.159](e)). A node that cannot be
// upgraded any further has exactly one remedy under the alpha's reinstall-only policy, and the
// refusal is where its owner finds that out -- so the words are asserted, not just the sentinel.
// Both ids appear because "too old" is meaningless without the pair: what is installed, and what
// it would have to be.
func TestUpgradeFloorRefusalNamesTheRemedyAndBothIds(t *testing.T) {
	want := man(ChainBriard, PlatformLinux, "v3.20260921.bbb2222")
	want.MinUpgradeFrom = "v3.20260920.aaa1111"
	have := man(ChainBriard, PlatformLinux, "v3.20260910.ccc3333")
	_, err := Decide(TargetStable, want, &have, nil)
	if !errors.Is(err, ErrTooOldToUpgrade) {
		t.Fatalf("err = %v, want ErrTooOldToUpgrade", err)
	}
	for _, s := range []string{"reinstall", have.Version, want.Version, want.MinUpgradeFrom} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("the refusal does not say %q: %v", s, err)
		}
	}
}

// THE NO-OP LINE IS A PRODUCT SURFACE, AND [B.159](f) MOVED WHICH BRANCH PRINTS IT. Before the
// default became `stable`, a bare `briard update` resolved `latest` and an up-to-date node
// read "already at <id>"; the stable branch had its own wording and nobody met it, because
// nothing reached it by default. The moment the default moved, two host-agent rigs went red on
// the wording alone -- an hour of VM suite to catch a string, which is why it is asserted here.
//
// The distinction is worth keeping, not flattening: EQUAL means nothing is owed and reads as
// "already at X" (what agent/cli/cli.go's help row promises the verb prints); genuinely PAST
// stable is a pin, and saying so is the point.
func TestStableNoOpSaysAlreadyAtWhenTheInstalledReleaseIsTheTarget(t *testing.T) {
	at := man(ChainBriard, PlatformLinux, "v3.20260906.aaaaaaa")
	d, err := Decide(TargetStable, at, &at, nil)
	if err != nil || d.Install {
		t.Fatalf("Decide = %+v, %v; want a no-op", d, err)
	}
	if !strings.Contains(d.Reason, "already at "+at.Version) {
		t.Errorf("an up-to-date node reads %q, want `already at %s`", d.Reason, at.Version)
	}
	// A node PAST stable is a pin, and keeps the line that says so.
	pinned := man(ChainBriard, PlatformLinux, "v3.20260910.ccccccc")
	d, err = Decide(TargetStable, at, &pinned, nil)
	if err != nil || d.Install {
		t.Fatalf("Decide = %+v, %v; want a no-op", d, err)
	}
	if !strings.Contains(d.Reason, "past stable") {
		t.Errorf("a pinned node reads %q, want it to name the pin", d.Reason)
	}
}

// THE FLOOR THIS TREE ACTUALLY DECLARES ([B.159](e), raised 2026-09-20). Pinned as a literal so
// that raising or clearing it is a deliberate edit with a failing test beside it, never a drift:
// the value decides whether every installed node below it is told to reinstall, which is the
// loudest thing this product says to an owner.
//
// WHY THIS VALUE. Gate 3 measured what an upgrade from the pre-[B.160] `stable` does: the guest
// image predates the tool profile, so the pushed agent is refused and the OLD guest agent stays;
// the new host then sends it a node-storage request carrying `metaLV` ([B.145a], after that
// stable), whose decoder refuses the unknown field -- and the host agent crash-loops, 42 restarts
// with the household's apps unreachable. A floor turns that into one refusal that names the
// remedy, with the node still serving its old release. Comparison is on the DATE, so every
// release from 20260920 on can upgrade among themselves; everything older must reinstall.
func TestTheTreeDeclaresTheFloorItMeansTo(t *testing.T) {
	const want = "v3.20260920.ec4d22a"
	if MinUpgradeFrom != want {
		t.Fatalf("MinUpgradeFrom = %q, want %q -- if this was deliberate, change the test and say why in the commit", MinUpgradeFrom, want)
	}
	// And it does what it says: a node on the pre-B.160 stable is refused, one from that day on is not.
	me := man(ChainBriard, PlatformLinux, "v3.20260921.aaaaaaa")
	me.MinUpgradeFrom = MinUpgradeFrom
	old := man(ChainBriard, PlatformLinux, "v3.20260910.64a7834")
	if _, err := Decide(TargetStable, me, &old, nil); !errors.Is(err, ErrTooOldToUpgrade) {
		t.Errorf("the pre-B.160 stable is not refused: %v", err)
	}
	ok := man(ChainBriard, PlatformLinux, "v3.20260920.ec4d22a")
	if _, err := Decide(TargetStable, me, &ok, nil); err != nil {
		t.Errorf("a release at the floor's own date was refused: %v", err)
	}
}
