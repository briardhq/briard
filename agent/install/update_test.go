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

// min_host ([B.86d]): the guest chain's one-directional compatibility promise, ordered on the
// date like everything else; a host id with no date cannot satisfy any requirement.
func TestHostSatisfies(t *testing.T) {
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
		err := HostSatisfies(tc.minHost, tc.host)
		if (err == nil) != tc.ok {
			t.Errorf("HostSatisfies(%q, %q) = %v, want ok=%v", tc.minHost, tc.host, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrHostTooOld) {
			t.Errorf("HostSatisfies(%q, %q) = %v, want ErrHostTooOld", tc.minHost, tc.host, err)
		}
	}
}

// The guest manifest names its closure and min_host, round-tripped through the one writer and
// reader; the host chain refuses them.
func TestWriteManifestCarriesTheGuestFacts(t *testing.T) {
	stage := t.TempDir()
	os.WriteFile(filepath.Join(stage, "nixos.qcow2.zst"), []byte("img"), 0o644)
	if err := WriteManifest(stage, ChainGuest, "", "guest.20260906.abc1234", "/nix/store/abc-nixos-system", "v3.20260906.abc1234", "", ""); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	b, _ := os.ReadFile(filepath.Join(stage, ManifestName))
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.System != "/nix/store/abc-nixos-system" || m.MinHost != "v3.20260906.abc1234" {
		t.Errorf("system/min_host = %q/%q", m.System, m.MinHost)
	}
	for _, bad := range [][]string{
		{ChainHost, "/nix/store/x", ""},    // a host manifest naming a closure
		{ChainGuest, "/tmp/not-store", ""}, // not a store path
		{ChainGuest, "", "not a segment/"}, // an unusable min_host
	} {
		if err := WriteManifest(stage, bad[0], "", "guest.20260906.abc1234", bad[1], bad[2], "", ""); err == nil {
			t.Errorf("WriteManifest(%v) accepted", bad)
		}
	}
	// Omitted when empty, so a host manifest's bytes are unchanged by the fields' existence.
	if err := WriteManifest(stage, ChainGuest, "", "guest.20260906.abc1234", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(stage, ManifestName))
	if bytes.Contains(b, []byte("system")) || bytes.Contains(b, []byte("min_host")) {
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
	if err := WriteManifest(stage, ChainHost, PlatformLinux, "v3.20260907.abc1234", "", "", "guest.20260901.def5678", ""); err != nil {
		t.Fatalf("host manifest naming its guest refused: %v", err)
	}
	m, err := ReadManifest(filepath.Join(stage, ManifestName))
	if err != nil || m.Guest != "guest.20260901.def5678" || m.Inputs != "" {
		t.Fatalf("host manifest read back as %+v (%v)", m, err)
	}
	if err := WriteManifest(stage, ChainGuest, "", "guest.20260901.def5678", "/nix/store/abc-sys", "v3.20260907.abc1234", "", inputs); err != nil {
		t.Fatalf("guest manifest carrying its inputs refused: %v", err)
	}
	if m, err = ReadManifest(filepath.Join(stage, ManifestName)); err != nil || m.Inputs != inputs || m.Guest != "" {
		t.Fatalf("guest manifest read back as %+v (%v)", m, err)
	}
	for _, bad := range [][]string{
		{ChainGuest, "guest.20260901.def5678", ""}, // a guest naming a guest
		{ChainHost, "", inputs},                    // a host carrying inputs
		{ChainHost, "stable", ""},                  // a pointer word as the pair
		{ChainGuest, "", "not-a-hash"},             // inputs that are not a sha256
	} {
		if err := WriteManifest(stage, bad[0], "", "v3.20260907.abc1234", "", "", bad[1], bad[2]); err == nil {
			t.Errorf("WriteManifest(%v) accepted", bad)
		}
	}
}
