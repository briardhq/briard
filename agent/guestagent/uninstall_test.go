package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/manifest"
)

// A manifest with an area in every tier but files, and one with files.
const uninstallManifest = `{"name":"dummy","version":"1","network":"host","containers":[{"name":"app",` +
	`"image":"localhost/briard-dummy@sha256:1111111111111111111111111111111111111111111111111111111111111111",` +
	`"mounts":[{"tier":"data","dir":"app","path":"/data"},{"tier":"unversioned","dir":"bk","path":"/bk"},` +
	`{"tier":"cache","dir":"c","path":"/cache"}],"primary":true,"port":8080,"healthPath":"/healthz"}]}`

const uninstallFilesManifest = `{"name":"dummy","version":"1","network":"host","containers":[{"name":"app",` +
	`"image":"localhost/briard-dummy@sha256:1111111111111111111111111111111111111111111111111111111111111111",` +
	`"mounts":[{"tier":"data","dir":"app","path":"/data"},{"tier":"files","dir":"photos","path":"/photos"}],` +
	`"primary":true,"port":8080,"healthPath":"/healthz"}]}`

var uninstallAt = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// uninstallRig is a volume whose subvolumes btrfs answers for, and whose directories list them.
type uninstallRig struct {
	*fakeExec
	subvols map[string]bool
}

func newUninstallRig(raw string, subvols ...string) *uninstallRig {
	r := &uninstallRig{fakeExec: &fakeExec{files: map[string]string{}}, subvols: map[string]bool{}}
	if raw != "" {
		r.files[manifestPath("dummy")] = raw
	}
	for _, s := range subvols {
		r.subvols[s] = true
		if _, tr, at, ok := quadlet.ParseSnapshotMember(filepath.Base(s)); ok {
			b, _ := json.Marshal(quadlet.SnapshotMeta{App: "dummy", Trigger: tr, TakenAt: at})
			r.files[quadlet.SnapshotSidecar(s)] = string(b)
		}
	}
	r.runFn = func(name string, args []string) ([]byte, error) {
		switch {
		case name == "ls" && args[1] == manifestDir:
			var ls []string
			for p := range r.files {
				if filepath.Dir(p) == manifestDir {
					ls = append(ls, filepath.Base(p))
				}
			}
			return []byte(strings.Join(ls, "\n")), nil
		case name == "ls":
			var ls []string
			for s := range r.subvols {
				if filepath.Dir(s) == strings.TrimSuffix(args[1], "/") {
					ls = append(ls, filepath.Base(s), filepath.Base(s)+".json")
				}
			}
			return []byte(strings.Join(ls, "\n")), nil
		case name == "btrfs" && args[0] == "subvolume" && args[1] == "show":
			if r.subvols[args[2]] {
				return []byte(args[2]), nil
			}
			return nil, errors.New("ERROR: not a subvolume")
		case name == "btrfs" && args[0] == "subvolume" && args[1] == "snapshot":
			r.subvols[args[len(args)-1]] = true
		case name == "btrfs" && args[0] == "subvolume" && args[1] == "delete":
			delete(r.subvols, args[2])
		case name == "rm":
			for _, p := range args[1:] {
				delete(r.files, p)
			}
		case name == "mv":
			src, dst := args[len(args)-2], args[len(args)-1]
			if r.subvols[src] {
				delete(r.subvols, src)
				r.subvols[dst] = true
			} else {
				r.files[dst] = r.files[src]
				delete(r.files, src)
			}
		}
		return nil, nil
	}
	return r
}

func (r *uninstallRig) run(name string, args ...string) error {
	out, err := r.Run(context.Background(), name, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}
	return nil
}

func (r *uninstallRig) first(match func([]string) bool) int {
	return slices.IndexFunc(r.runs, match)
}

func memberAt(app string, tr quadlet.Trigger, at time.Time) string {
	return quadlet.SnapshotMember(app, tr, at)
}

// AN UNINSTALL KEEPS THE DATA AND DELETES THE REST: the data area as a member named for the
// uninstall, pinned to the manifest and carrying its event; every area, every other member of the
// app gone, another app's untouched. The manifest goes, flushed, before any area is deleted.
func TestAnUninstallKeepsTheDataAndDeletesTheRest(t *testing.T) {
	start, clock := memberAt("dummy", quadlet.TriggerStart, uninstallAt.Add(-time.Hour)), memberAt("dummy", quadlet.TriggerClock, uninstallAt.Add(-2*time.Hour))
	other := memberAt("other", quadlet.TriggerStart, uninstallAt.Add(-time.Hour))
	areas := []string{quadlet.DataRoot("dummy"), quadlet.Area(manifest.TierUnversioned, "dummy"), quadlet.Area(manifest.TierCache, "dummy")}
	r := newUninstallRig(uninstallManifest, append(areas, start, clock, other)...)
	if err := uninstallApp(context.Background(), r, r.run, appUninstallRequest{App: "dummy", Keep: true, At: uninstallAt}); err != nil {
		t.Fatal(err)
	}
	kept := memberAt("dummy", quadlet.TriggerUninstall, uninstallAt)
	if !r.subvols[kept] || !r.subvols[other] {
		t.Fatalf("kept %v, the other app's %v: %v", r.subvols[kept], r.subvols[other], r.subvols)
	}
	for _, gone := range append(areas, start, clock) {
		if r.subvols[gone] {
			t.Errorf("%s is still there", gone)
		}
	}
	if len(r.subvols) != 2 {
		t.Errorf("subvolumes left: %v", r.subvols)
	}
	var meta quadlet.SnapshotMeta
	if err := json.Unmarshal([]byte(r.files[quadlet.SnapshotSidecar(kept)]), &meta); err != nil {
		t.Fatalf("the kept member's sidecar: %v", err)
	}
	if meta.Trigger != quadlet.TriggerUninstall || meta.Manifest != uninstallManifest || meta.Consistency != quadlet.Quiesced ||
		!meta.Event.Has(quadlet.ReasonAppUninstall) || !meta.TakenAt.Equal(uninstallAt) {
		t.Errorf("sidecar %+v", meta)
	}
	if _, ok := r.files[manifestPath("dummy")]; ok {
		t.Error("the manifest is still on the volume")
	}
	take := r.first(func(c []string) bool { return len(c) > 2 && c[1] == "subvolume" && c[2] == "snapshot" })
	rm := r.first(func(c []string) bool { return c[0] == "rm" && slices.Contains(c, manifestPath("dummy")) })
	sync := r.first(func(c []string) bool { return slices.Equal(c, []string{"sync", "-f", manifestDir}) })
	del := r.first(func(c []string) bool { return len(c) > 3 && c[2] == "delete" })
	if take < 0 || rm < take || sync < rm || del < sync {
		t.Errorf("take %d, manifest removed %d, flushed %d, first delete %d: want them in that order", take, rm, sync, del)
	}
}

// DELETE NOW, after the uninstall: the kept copy and its sidecar go, and nothing converges -- the
// app is not on the volume to stop.
func TestDeleteNowTakesTheKeptCopy(t *testing.T) {
	kept := memberAt("dummy", quadlet.TriggerUninstall, uninstallAt)
	r := newUninstallRig("", kept)
	if err := uninstallApp(context.Background(), r, r.run, appUninstallRequest{App: "dummy"}); err != nil {
		t.Fatal(err)
	}
	if r.subvols[kept] {
		t.Error("the kept copy is still there")
	}
	if _, ok := r.files[quadlet.SnapshotSidecar(kept)]; ok {
		t.Error("the kept copy's sidecar outlived it")
	}
	if r.first(func(c []string) bool { return c[0] == "systemctl" }) >= 0 {
		t.Error("a Delete now touched a unit")
	}
	if err := uninstallApp(context.Background(), r, r.run, appUninstallRequest{App: "dummy", Keep: true, At: uninstallAt}); err == nil {
		t.Error("an app that is not installed was uninstalled again, keeping")
	}
}

// AN INSTALL STILL WAITING FOR ITS GATE refuses the uninstall, before anything is touched.
func TestAnUnfinishedInstallRefusesTheUninstall(t *testing.T) {
	r := newUninstallRig(uninstallManifest, quadlet.DataRoot("dummy"))
	r.files[stagedPath("dummy")] = uninstallManifest
	if err := uninstallApp(context.Background(), r, r.run, appUninstallRequest{App: "dummy", Keep: true, At: uninstallAt}); err == nil {
		t.Fatal("uninstalled over a pending install")
	}
	if len(r.runs) != 0 || !r.subvols[quadlet.DataRoot("dummy")] {
		t.Errorf("the refusal did %v", r.runs)
	}
}

// THE FILES ARE KEPT BESIDE THE VERSIONS, under the data member's name, and installing the app
// again puts them back as its files area and drops the copy. Its Versions go at the uninstall.
func TestTheFilesAreKeptAndPutBack(t *testing.T) {
	version := filepath.Join(filesVersionsDir(), filepath.Base(memberAt("dummy", quadlet.TriggerClock, uninstallAt.Add(-time.Hour))))
	r := newUninstallRig(uninstallFilesManifest, quadlet.DataRoot("dummy"), filesArea("dummy"), version)
	if err := uninstallApp(context.Background(), r, r.run, appUninstallRequest{App: "dummy", Keep: true, At: uninstallAt}); err != nil {
		t.Fatal(err)
	}
	kept := memberAt("dummy", quadlet.TriggerUninstall, uninstallAt)
	twin := filesKept(kept)
	if !r.subvols[kept] || !r.subvols[twin] || r.subvols[filesArea("dummy")] || r.subvols[version] {
		t.Fatalf("after the uninstall: %v", r.subvols)
	}
	if k := keptCopies(context.Background(), r); len(k) != 2 || k[0].files == k[1].files {
		t.Errorf("kept copies %+v, want the data's and the files'", k)
	}

	// Installed again: the restore made the files area empty, and this puts the copy in its place.
	r.files[manifestPath("dummy")] = uninstallFilesManifest
	r.subvols[filesArea("dummy")] = true
	if err := reinstateApp(context.Background(), r, r.run, appReinstateRequest{App: "dummy", Member: kept}); err != nil {
		t.Fatal(err)
	}
	if r.subvols[twin] || !r.subvols[filesArea("dummy")] {
		t.Errorf("after the reinstall: %v", r.subvols)
	}
	if r.first(func(c []string) bool {
		return len(c) > 3 && c[2] == "snapshot" && c[len(c)-2] == twin && c[len(c)-1] == filesArea("dummy")+".restoring"
	}) < 0 {
		t.Errorf("the files were not staged from the copy: %v", r.runs)
	}
	if k := keptCopies(context.Background(), r); len(k) != 0 {
		t.Errorf("an installed app's member counts as kept: %+v", k)
	}
	if err := reinstateApp(context.Background(), r, r.run, appReinstateRequest{App: "dummy", Member: memberAt("dummy", quadlet.TriggerStart, uninstallAt)}); err == nil {
		t.Error("reinstated from a member that is no uninstall's")
	}
}

// A KEPT COPY ENDS AT THIRTY DAYS, at the files tick, whether or not any app has a files area.
func TestAKeptCopyEndsAtThirtyDays(t *testing.T) {
	v := newUserVolume("immich")
	old := filepath.Join(filesVersionsDir(), filepath.Base(memberAt("gone", quadlet.TriggerUninstall, tickNow.Add(-31*24*time.Hour))))
	young := filepath.Join(filesVersionsDir(), filepath.Base(memberAt("gone", quadlet.TriggerUninstall, tickNow.Add(-29*24*time.Hour))))
	v.members[old], v.members[young] = true, true
	rep := filesTick(context.Background(), v, tickNow)
	if !slices.Equal(rep.Expired, []string{old}) || v.members[old] || !v.members[young] {
		t.Errorf("expired %v; left %v", rep.Expired, v.members)
	}
}

// THE VALVE SHEDS A KEPT COPY BEFORE ANY VERSION, but only once a backup holds it: an unmarked one
// is not a candidate, however old.
func TestTheValveShedsABackedUpKeptCopyFirst(t *testing.T) {
	v := newUserVolume("immich")
	v.memberBytes = 2 * gb
	cap, target := v.allocation()
	v.files = map[string]string{}
	version := v.member("immich", tickNow.Add(-10*24*time.Hour))
	kept := func(app string, age time.Duration, backed bool) string {
		m := filepath.Join(filesVersionsDir(), filepath.Base(memberAt(app, quadlet.TriggerUninstall, tickNow.Add(-age))))
		v.members[m] = true
		b, _ := json.Marshal(quadlet.SnapshotMeta{App: app, Trigger: quadlet.TriggerUninstall, BackedUp: backed})
		v.files[quadlet.SnapshotSidecar(m)] = string(b)
		return m
	}
	unmarked := kept("old", 20*24*time.Hour, false)
	marked := kept("gone", 5*24*time.Hour, true)
	// Room for all but one member's bytes above the target, the hourly one this tick takes counted.
	v.areaBytes = cap - target - 3*v.memberBytes
	rep := filesTick(context.Background(), v, tickNow)
	if len(rep.Errors) > 0 {
		t.Fatalf("errors: %v", rep.Errors)
	}
	if !slices.Equal(rep.Shed, []string{marked}) || !v.members[version] || !v.members[unmarked] || rep.Short {
		t.Errorf("shed %v (short %v); left %v", rep.Shed, rep.Short, v.members)
	}
}

// THE BACKUP CARRIES A KEPT COPY as the app's member, and marks it once the run has finished.
func TestTheBackupCarriesAKeptCopyAndMarksIt(t *testing.T) {
	gone := strings.TrimPrefix(memberAt("mealie", quadlet.TriggerUninstall, time.Now().Add(-time.Hour)), quadlet.SnapshotsDir)
	f, _ := backupRig(map[string]func() ([]byte, error){
		"backup": func() ([]byte, error) { return []byte(resticSummary), nil },
	}, gone)
	rep := runBackup(context.Background(), f, backupRequest{Repository: "http://h/"})
	if rep.Error != "" || !slices.Contains(rep.Apps, "mealie") {
		t.Fatalf("report %+v", rep)
	}
	if runIndex(f, 0, "mount", "-o", "bind,ro", quadlet.SnapshotsDir+gone, backupData+"/mealie") < 0 {
		t.Errorf("the kept copy was not backed up: %v", f.runs)
	}
	if !backedUp(f, quadlet.SnapshotsDir+gone) {
		t.Error("the kept copy is not marked backed up")
	}
}
