package guestagent

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

const dummyManifestStaged = `{"name":"dummy","version":"2","network":"host","containers":[{"name":"app",` +
	`"image":"localhost/briard-dummy@sha256:9999999999999999999999999999999999999999999999999999999999999999",` +
	`"mount":"/data","primary":true,"port":8080,"healthPath":"/healthz"}]}`

// index is where argv first appears in what the fake ran, -1 if never.
func (c *convergeExec) index(argv ...string) int {
	for i, r := range c.fakeExec.runs {
		if reflect.DeepEqual(r, argv) {
			return i
		}
	}
	return -1
}

// STAGING LEAVES THE ACCEPTED MANIFEST ALONE, and writes the rollback point durably BEFORE the
// staged manifest: a `.json.next` whose rollback point is not yet on disk is an install a crash
// would leave with nothing to undo it by.
func TestStageWritesTheRollbackPointBeforeTheStagedManifest(t *testing.T) {
	x := dummyNode(t)
	g := dial(t, x)
	if err := g.ServiceStage(context.Background(), "dummy", "/var/lib/briard/dummy", nil, dummyManifestStaged, "/snap/m1"); err != nil {
		t.Fatal(err)
	}
	f := x.fakeExec.files
	if f[manifestPath("dummy")] != dummyManifest {
		t.Fatal("staging overwrote the accepted manifest")
	}
	if f[stagedPath("dummy")] != dummyManifestStaged {
		t.Fatalf("staged manifest = %q", f[stagedPath("dummy")])
	}
	if strings.TrimSpace(f[rollbackPath("dummy")]) != "/snap/m1" {
		t.Fatalf("rollback point = %q", f[rollbackPath("dummy")])
	}
	rb := x.index("mv", "-T", rollbackPath("dummy")+".tmp", rollbackPath("dummy"))
	st := x.index("mv", "-T", stagedPath("dummy")+".tmp", stagedPath("dummy"))
	if rb < 0 || st < 0 || rb > st {
		t.Fatalf("rollback point placed at %d, staged manifest at %d: the rollback point must land first", rb, st)
	}
	if x.index("sync", "-f", manifestDir) < 0 || x.index("sync", "-f", manifestDir) > st {
		t.Fatalf("the rollback point was not flushed before the staged manifest landed; ran %v", x.fakeExec.runs)
	}
}

// A STALE ROLLBACK POINT IS NOT INHERITED: an install with none (a fresh one) removes whatever an
// earlier install left, or undoing it would put back some other day's data.
func TestStageWithoutARollbackPointRemovesAStaleOne(t *testing.T) {
	x := dummyNode(t)
	x.fakeExec.files[rollbackPath("dummy")] = "/snap/old\n"
	g := dial(t, x)
	if err := g.ServiceStage(context.Background(), "dummy", "/var/lib/briard/dummy", nil, dummyManifestStaged, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := x.fakeExec.files[rollbackPath("dummy")]; ok {
		t.Fatal("a stale rollback point survived a stage that has none")
	}
}

// THE COMMIT IS ONE RENAME, flushed before the rollback point goes.
func TestCommitRenamesTheStagedManifestOverTheAccepted(t *testing.T) {
	x := dummyNode(t)
	x.fakeExec.files[stagedPath("dummy")] = dummyManifestStaged
	x.fakeExec.files[rollbackPath("dummy")] = "/snap/m1\n"
	g := dial(t, x)
	if err := g.ServiceCommit(context.Background(), "dummy"); err != nil {
		t.Fatal(err)
	}
	f := x.fakeExec.files
	if f[manifestPath("dummy")] != dummyManifestStaged {
		t.Fatal("the staged manifest is not the accepted one after a commit")
	}
	for _, p := range []string{stagedPath("dummy"), rollbackPath("dummy")} {
		if _, ok := f[p]; ok {
			t.Fatalf("%s survived the commit", p)
		}
	}
	mv := x.index("mv", "-T", stagedPath("dummy"), manifestPath("dummy"))
	rm := x.index("rm", "-f", rollbackPath("dummy"))
	sync := x.index("sync", "-f", manifestDir)
	if !(mv < sync && sync < rm) {
		t.Fatalf("commit order mv=%d sync=%d rm=%d: the rename must be durable before the rollback point goes", mv, sync, rm)
	}
}

// DISCARD drops the staged manifest -- what "pending" means -- before the rollback point, and
// leaves the accepted manifest standing.
func TestDiscardDropsTheStageAndKeepsTheAccepted(t *testing.T) {
	x := dummyNode(t)
	x.fakeExec.files[stagedPath("dummy")] = dummyManifestStaged
	x.fakeExec.files[rollbackPath("dummy")] = "/snap/m1\n"
	g := dial(t, x)
	if err := g.ServiceDiscard(context.Background(), "dummy"); err != nil {
		t.Fatal(err)
	}
	f := x.fakeExec.files
	if f[manifestPath("dummy")] != dummyManifest {
		t.Fatal("discard touched the accepted manifest")
	}
	if _, ok := f[stagedPath("dummy")]; ok {
		t.Fatal("the staged manifest survived a discard")
	}
	if x.index("rm", "-f", stagedPath("dummy")) > x.index("rm", "-f", rollbackPath("dummy")) {
		t.Fatal("the rollback point went before the staged manifest")
	}
}

// PENDING READS EVERYTHING THE UNDO NEEDS FROM DISK -- an upgrade with its rollback point, and a
// fresh install with no accepted manifest and none.
func TestPendingReportsEachStagedInstall(t *testing.T) {
	x := dummyNode(t)
	x.fakeExec.files[stagedPath("dummy")] = dummyManifestStaged
	x.fakeExec.files[rollbackPath("dummy")] = "/snap/m1\n"
	x.fakeExec.files[stagedPath("other")] = otherManifest
	g := dial(t, x)
	got, err := g.ServicePending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]PendingInstall{}
	for _, p := range got {
		byName[p.Name] = p
	}
	want := map[string]PendingInstall{
		"dummy": {Name: "dummy", Staged: dummyManifestStaged, Accepted: dummyManifest, Rollback: "/snap/m1"},
		"other": {Name: "other", Staged: otherManifest},
	}
	if !reflect.DeepEqual(byName, want) {
		t.Fatalf("pending = %+v\nwant %+v", byName, want)
	}
}

// CONVERGE HOLDS A PENDING INSTALL BACK unless it is the live one. A node promoting onto a staged
// upgrade starts NEITHER version: the staged one was never accepted, and the accepted one may be
// about to run on data the staged one already migrated.
func TestConvergeHoldsBackAStagedServiceThatIsNotLive(t *testing.T) {
	x := dummyNode(t)
	x.fakeExec.files[stagedPath("dummy")] = dummyManifestStaged
	if _, err := Converge(context.Background(), x, ""); err != nil {
		t.Fatal(err)
	}
	if x.ran("systemctl", "start", "briard-dummy-app.service") {
		t.Fatal("converge started a service whose install was never accepted")
	}
	if _, ok := x.fakeExec.files[quadletDir+"/briard-dummy-app.container"]; ok {
		t.Fatal("converge rendered a held-back service")
	}
}

// THE LIVE INSTALL RENDERS FROM ITS STAGED MANIFEST -- the version under the health gate.
func TestConvergeRendersTheLiveInstallFromItsStagedManifest(t *testing.T) {
	x := dummyNode(t)
	x.fakeExec.files[stagedPath("dummy")] = dummyManifestStaged
	if _, err := Converge(context.Background(), x, "dummy"); err != nil {
		t.Fatal(err)
	}
	if !x.ran("systemctl", "start", "briard-dummy-app.service") {
		t.Fatal("the live install was not started")
	}
	if c := x.fakeExec.files[quadletDir+"/briard-dummy-app.container"]; !strings.Contains(c, "sha256:9999") {
		t.Fatalf("the live install rendered from the accepted manifest, not the staged one:\n%s", c)
	}
}

// A FRESH INSTALL has no accepted manifest: live, it is rendered from the stage alone; not live,
// it does not exist.
func TestConvergeFreshStagedInstall(t *testing.T) {
	for _, live := range []string{"other", ""} {
		x := dummyNode(t)
		x.fakeExec.files[stagedPath("other")] = otherManifest
		if _, err := Converge(context.Background(), x, live); err != nil {
			t.Fatal(err)
		}
		started := x.ran("systemctl", "start", "briard-other-app.service")
		if started != (live == "other") {
			t.Fatalf("live=%q: fresh staged install started = %v", live, started)
		}
		if !x.ran("systemctl", "start", "briard-dummy-app.service") {
			t.Fatalf("live=%q: the accepted service beside it was not started", live)
		}
	}
}
