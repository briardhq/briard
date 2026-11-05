package guestagent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"briard.io/agent/files"
	"briard.io/shared/dashboard"
	"briard.io/shared/manifest"
)

// viewWorld is the guest as the view sees it: which directories exist and what each one is
// (btrfs gives every subvolume its own device, so a restore's new area is a new id at the old
// path), which are bound where, and the mount table the kernel would print for that.
type viewWorld struct {
	f       *fakeExec
	id      map[string]string // a source directory -> its device:inode
	mounts  map[string]string // a mount point -> the id it showed when bound
	ro      map[string]bool
	members []string // the files tier's Versions, by name
	next    int
}

func newViewWorld(t *testing.T) *viewWorld {
	t.Helper()
	filesViewLock = filepath.Join(t.TempDir(), "lock")
	w := &viewWorld{id: map[string]string{}, mounts: map[string]string{}, ro: map[string]bool{}}
	w.f = &fakeExec{files: map[string]string{}}
	w.f.runFn = w.run
	return w
}

func (w *viewWorld) dir(p string) {
	w.next++
	w.id[p] = fmt.Sprintf("%d:256", w.next)
}

func (w *viewWorld) install(t *testing.T, name string, mounts ...manifest.Mount) {
	t.Helper()
	m := manifest.Manifest{Name: name, Version: "1", Containers: []manifest.Container{{
		Name: "app", Image: "example.com/" + name + "@sha256:" + strings.Repeat("1", 64),
		Mounts: mounts, Primary: true, Port: 80, HealthPath: "/health",
	}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manifest.Parse(raw); err != nil {
		t.Fatalf("the test's own manifest is invalid: %v", err)
	}
	w.f.files[manifestDir+"/"+name+".json"] = string(raw)
	for _, mt := range mounts {
		w.dir("/var/lib/briard/" + mt.Tier + "/" + name + "/" + mt.Dir)
	}
}

func (w *viewWorld) run(name string, args []string) ([]byte, error) {
	switch {
	case name == "ls" && len(args) == 2 && args[1] == manifestDir:
		var out []string
		for p := range w.f.files {
			if strings.HasPrefix(p, manifestDir+"/") {
				out = append(out, strings.TrimPrefix(p, manifestDir+"/"))
			}
		}
		slices.Sort(out)
		return []byte(strings.Join(out, "\n")), nil
	case name == "ls" && len(args) == 2 && args[1] == filesVersionsDir():
		return []byte(strings.Join(w.members, "\n")), nil
	case name == "test" && args[0] == "-d":
		if _, ok := w.id[args[1]]; ok {
			return nil, nil
		}
		return nil, os.ErrNotExist
	case name == "stat":
		p, src := args[len(args)-2], args[len(args)-1]
		return []byte(w.mounts[p] + "\n" + w.id[src] + "\n"), nil
	case name == "mount":
		opts, src, p := args[1], args[2], args[3]
		w.mounts[p], w.ro[p] = w.id[src], strings.Contains(opts, "ro")
	case name == "umount":
		delete(w.mounts, args[len(args)-1])
	}
	return nil, nil
}

// refresh runs one refresh against the mount table as it stands, and returns the commands that
// changed a mount.
func (w *viewWorld) refresh(t *testing.T) []string {
	t.Helper()
	var table []string
	for p := range w.mounts {
		table = append(table, "1 2 0:1 / "+strings.ReplaceAll(p, " ", `\040`)+" rw - btrfs /dev/vda rw")
	}
	w.f.files["/proc/self/mountinfo"] = strings.Join(table, "\n")
	w.f.runs = nil
	if err := refreshFilesView(context.Background(), w.f); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	var acts []string
	for _, r := range w.f.runs {
		if r[0] == "mount" || r[0] == "umount" {
			acts = append(acts, strings.Join(r, " "))
		}
	}
	return acts
}

func (w *viewWorld) view() []string {
	var out []string
	for p := range w.mounts {
		mode := "rw"
		if w.ro[p] {
			mode = "ro"
		}
		out = append(out, strings.TrimPrefix(p, files.ViewDir+"/")+" "+mode)
	}
	slices.Sort(out)
	return out
}

func homeAssistantAndFiles(t *testing.T) *viewWorld {
	w := newViewWorld(t)
	w.install(t, "home-assistant",
		manifest.Mount{Tier: manifest.TierData, Dir: "config", Path: "/config"},
		manifest.Mount{Tier: manifest.TierUnversioned, Dir: "backups", Path: "/config/backups"})
	w.install(t, "mosquitto", manifest.Mount{Tier: manifest.TierData, Dir: "data", Path: "/mosquitto/data"})
	w.install(t, files.Name, manifest.Mount{Tier: manifest.TierCache, Dir: "state", Path: "/home/filebrowser/data"})
	return w
}

// Each app is a folder named as the household reads it, holding exactly the directories its
// manifest mounts: data and unversioned read-write, the cache not at all, Files not itself.
func TestTheViewIsEachAppsMountsUnderItsTitle(t *testing.T) {
	w := homeAssistantAndFiles(t)
	w.refresh(t)
	want := []string{"Home Assistant/backups rw", "Home Assistant/config rw", "Mosquitto/data rw"}
	if got := w.view(); !slices.Equal(got, want) {
		t.Fatalf("the view is %v, want %v", got, want)
	}
	if !w.f.ran("mount", "-o", "bind", "/var/lib/briard/data/home-assistant/config", files.ViewDir+"/Home Assistant/config") {
		t.Errorf("Home Assistant's config was not bound from its data area: %v", w.f.runs)
	}
}

// The index names an app the way its project spells it, and the view follows the index.
func TestTheViewFollowsTheIndexsTitle(t *testing.T) {
	w := homeAssistantAndFiles(t)
	raw, _ := json.Marshal(dashboard.Catalog{Apps: []dashboard.CatalogApp{{App: manifest.App{Name: "mosquitto", Title: "Eclipse Mosquitto"}}}})
	w.f.files[dashboard.CatalogPath] = string(raw)
	w.refresh(t)
	if !slices.Contains(w.view(), "Eclipse Mosquitto/data rw") {
		t.Fatalf("the view does not use the index's title: %v", w.view())
	}
}

// The household's files and their Versions are read-only, each Version under a name that reads
// as a date and never changes.
func TestFilesAndTheirVersionsAreReadOnlyUnderAbsoluteNames(t *testing.T) {
	w := homeAssistantAndFiles(t)
	w.install(t, "photos", manifest.Mount{Tier: manifest.TierFiles, Dir: "library", Path: "/library"})
	member := "photos-clock-20261105T130000Z"
	w.members = []string{member, member + ".json"}
	w.dir(filesVersionsDir() + "/" + member + "/library")
	w.refresh(t)
	for _, want := range []string{"Photos/library ro", "Photos/Versions/2026-11-05 Thu 13.00/library ro"} {
		if !slices.Contains(w.view(), want) {
			t.Errorf("the view lacks %q: %v", want, w.view())
		}
	}
}

// A restore swaps an area underneath its bind, and the next refresh binds the new one -- the
// stale copy is exactly what an edit would otherwise vanish into.
func TestARestoreUnderneathIsBoundAgain(t *testing.T) {
	w := homeAssistantAndFiles(t)
	w.refresh(t)
	cfg, mp := "/var/lib/briard/data/home-assistant/config", files.ViewDir+"/Home Assistant/config"
	w.dir(cfg) // the restore: a new subvolume at the old path
	acts := w.refresh(t)
	if w.mounts[mp] != w.id[cfg] {
		t.Fatalf("the view still shows the area from before the restore (%s, want %s)", w.mounts[mp], w.id[cfg])
	}
	if want := []string{"umount -l " + mp, "mount -o bind " + cfg + " " + mp}; !slices.Equal(acts, want) {
		t.Fatalf("the refresh did %v, want exactly %v", acts, want)
	}
}

// A view that matches the volume is left alone: a refresh every tick must not churn mounts under
// a household browsing them.
func TestAnUnchangedViewIsLeftAlone(t *testing.T) {
	w := homeAssistantAndFiles(t)
	w.refresh(t)
	if acts := w.refresh(t); len(acts) != 0 {
		t.Fatalf("an unchanged view was touched: %v", acts)
	}
}

// Without Files there is no view, and a view left from before is unbound; teardown unbinds it
// all, because each bind holds the volume about to be unmounted.
func TestWithoutFilesThereIsNoView(t *testing.T) {
	w := homeAssistantAndFiles(t)
	w.refresh(t)
	delete(w.f.files, manifestDir+"/"+files.Name+".json")
	w.refresh(t)
	if len(w.mounts) != 0 {
		t.Fatalf("the view outlived Files: %v", w.view())
	}

	w = homeAssistantAndFiles(t)
	w.refresh(t)
	var table []string
	for p := range w.mounts {
		table = append(table, "1 2 0:1 / "+strings.ReplaceAll(p, " ", `\040`)+" rw - btrfs /dev/vda rw")
	}
	w.f.files["/proc/self/mountinfo"] = strings.Join(table, "\n")
	if err := tearDownFilesView(context.Background(), w.f); err != nil {
		t.Fatal(err)
	}
	if len(w.mounts) != 0 {
		t.Fatalf("teardown left binds holding the volume: %v", slices.Sorted(maps.Keys(w.mounts)))
	}
}

// An install still being staged shows what it stages: its areas exist from the stage on.
func TestAStagedInstallShowsItsStagedMounts(t *testing.T) {
	w := homeAssistantAndFiles(t)
	m := w.f.files[manifestDir+"/mosquitto.json"]
	w.f.files[manifestDir+"/mosquitto.json.next"] = strings.Replace(m, `"dir":"data"`, `"dir":"store"`, 1)
	w.dir("/var/lib/briard/data/mosquitto/store")
	w.refresh(t)
	if v := w.view(); !slices.Contains(v, "Mosquitto/store rw") || slices.Contains(v, "Mosquitto/data rw") {
		t.Fatalf("the view is %v, want the staged manifest's mounts", v)
	}
}

func TestMountinfoPathsAreUnescaped(t *testing.T) {
	if got := unescapeMountinfo(`/run/briard/files-view/Home\040Assistant/config`); got != "/run/briard/files-view/Home Assistant/config" {
		t.Fatalf("unescaped %q", got)
	}
}
