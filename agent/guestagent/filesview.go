package guestagent

// FILES' VIEW: one folder per installed app under files.ViewDir, holding the directories that
// app's manifest mounts, each a bind of the real one, plus that app's Versions under names a
// person reads. Files (agent/files) mounts the root once, with slave propagation, so the binds
// made here reach the running container and it never restarts for another app's sake.
//
// A BIND IS KEPT TRUE BY ASKING, NOT BY HOOKING EVERY ACT. A bind holds the directory it was
// made from, so a restore that swaps an app's area leaves it showing the old, deleted copy, and
// an edit made through it vanishes. Rather than teach every act that replaces an area to rebind,
// the view is re-derived whenever it is refreshed and a bind is replaced exactly when its folder
// no longer resolves to the same directory as its source (device and inode, which btrfs gives
// every subvolume its own of). Converge refreshes it, so an install, an undo, a rollback and a
// promotion are seen at once; the files tick refreshes it, so a new Version, a renamed title or
// anything missed is seen within the tick.
//
// The data and unversioned tiers are bound read-write, the household's to edit; files and its
// Versions read-only; the cache not at all, being the app's to throw away.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"briard.io/agent/files"
	"briard.io/agent/quadlet"
	"briard.io/shared/dashboard"
	"briard.io/shared/manifest"
)

// filesViewLock serialises refreshes across processes: the promotion's converge runs as its own
// process while the agent's tick may be refreshing too, and two refreshes binding one folder
// would stack mounts. A variable so a test can move it.
var filesViewLock = "/run/briard/files-view.lock"

// versionsFolder holds an app's Versions inside its folder. Capitalised, so no manifest directory
// (a lowercase slug) can be named it.
const versionsFolder = "Versions"

// versionName is how a Version is named in the view: absolute, so a name never changes under an
// open folder or a download, and without a colon, which Windows refuses in a file name.
const versionName = "2006-01-02 Mon 15.04"

// viewBind is one folder of the view: what it binds and whether read-only.
type viewBind struct {
	src string
	ro  bool
}

// refreshFilesView brings the view to what the volume says now. A failure names what failed and
// leaves the rest of the view as good as it got; nothing here may fail what called it.
func refreshFilesView(ctx context.Context, x Executor) error {
	unlock, err := lockFilesView()
	if err != nil {
		return err
	}
	defer unlock()
	want, err := filesViewWanted(ctx, x)
	if err != nil {
		return err
	}
	return reconcileFilesView(ctx, x, want)
}

// tearDownFilesView unbinds the whole view: the volume is about to be unmounted, and a bind left
// behind would hold it.
func tearDownFilesView(ctx context.Context, x Executor) error {
	unlock, err := lockFilesView()
	if err != nil {
		return err
	}
	defer unlock()
	return reconcileFilesView(ctx, x, nil)
}

func lockFilesView() (func(), error) {
	f, err := os.OpenFile(filesViewLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("files view: lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("files view: lock: %w", err)
	}
	return func() { f.Close() }, nil
}

// filesViewWanted is the view the volume describes: nothing while Files is not installed.
func filesViewWanted(ctx context.Context, x Executor) (map[string]viewBind, error) {
	ms, err := viewManifests(ctx, x)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(ms, func(m manifest.Manifest) bool { return m.Name == files.Name }) {
		return nil, nil
	}
	var cat *dashboard.Catalog
	if raw, err := x.ReadFile(dashboard.CatalogPath); err == nil {
		var c dashboard.Catalog
		if json.Unmarshal(raw, &c) == nil {
			cat = &c
		}
	}
	zone := time.UTC
	if raw, err := x.ReadFile(localtimePath); err == nil {
		if z, err := time.LoadLocationFromTZData("Local", raw); err == nil {
			zone = z
		}
	}
	versions := map[string][]fileVersion{}
	for _, v := range fileVersions(ctx, x) {
		versions[v.app] = append(versions[v.app], v)
	}

	want := map[string]viewBind{}
	used := map[string]bool{}
	for _, m := range ms {
		if m.Name == files.Name {
			continue
		}
		folder := folderName(cat.Title(m.Name))
		if used[folder] {
			folder += " (" + m.Name + ")" // two titles alike: the slug tells them apart
		}
		used[folder] = true
		var filesDirs []string
		for _, tier := range []string{manifest.TierData, manifest.TierUnversioned, manifest.TierFiles} {
			for _, d := range tierDirs(m, tier) {
				p := path.Join(files.ViewDir, folder, d)
				if _, taken := want[p]; taken {
					log.Printf("files view: %s mounts %q in two tiers; showing the first", m.Name, d)
					continue
				}
				want[p] = viewBind{src: quadlet.Area(tier, m.Name) + "/" + d, ro: tier == manifest.TierFiles}
				if tier == manifest.TierFiles {
					filesDirs = append(filesDirs, d)
				}
			}
		}
		if len(filesDirs) == 0 {
			continue
		}
		for _, v := range versions[m.Name] {
			name := v.at.In(zone).Format(versionName)
			for _, d := range filesDirs {
				p := path.Join(files.ViewDir, folder, versionsFolder, name, d)
				if _, taken := want[p]; !taken {
					want[p] = viewBind{src: v.path + "/" + d, ro: true}
				}
			}
		}
	}
	return want, nil
}

// viewManifests is every app on the volume, an install still being staged included: its areas
// exist from the moment it is staged, and the household may as well see them.
func viewManifests(ctx context.Context, x Executor) ([]manifest.Manifest, error) {
	out, err := x.Run(ctx, "ls", "-1", manifestDir)
	if err != nil {
		return nil, nil // no manifest directory: nothing installed
	}
	byName := map[string]manifest.Manifest{}
	names := nonEmptyLines(out)
	slices.Sort(names) // `<app>.json` before `<app>.json.next`: the staged one wins
	for _, n := range names {
		if !strings.HasSuffix(n, ".json") && !strings.HasSuffix(n, ".json.next") {
			continue
		}
		raw, err := x.ReadFile(manifestDir + "/" + n)
		if err != nil {
			return nil, fmt.Errorf("files view: read %s: %w", n, err)
		}
		m, _, err := manifest.Parse(raw)
		if err != nil {
			log.Printf("files view: %s does not parse (%v); not shown", n, err)
			continue
		}
		byName[m.Name] = m
	}
	var ms []manifest.Manifest
	for _, n := range slices.Sorted(maps.Keys(byName)) {
		ms = append(ms, byName[n])
	}
	return ms, nil
}

// tierDirs is the directories an app mounts from one tier, once each, in its manifest's order.
func tierDirs(m manifest.Manifest, tier string) []string {
	var out []string
	for _, c := range m.Containers {
		for _, mt := range c.Mounts {
			if mt.Tier == tier && !slices.Contains(out, mt.Dir) {
				out = append(out, mt.Dir)
			}
		}
	}
	return out
}

// folderName is a title made safe as one path element: no separator, and not hidden.
func folderName(title string) string {
	return strings.TrimLeft(strings.ReplaceAll(title, "/", "-"), ".")
}

// reconcileFilesView makes the binds under the view exactly want: a bind no longer wanted is
// unbound, one that went stale is made again, a missing one is made.
func reconcileFilesView(ctx context.Context, x Executor, want map[string]viewBind) error {
	have, err := viewMounts(x)
	if err != nil {
		return err
	}
	var errs []error
	// Deepest first, so a folder is emptied before its parent is pruned.
	slices.SortFunc(have, func(a, b string) int { return strings.Count(b, "/") - strings.Count(a, "/") })
	mounted := map[string]bool{}
	for _, p := range have {
		if _, ok := want[p]; ok && !mounted[p] {
			mounted[p] = true
			continue
		}
		// Not wanted, or a second mount stacked on one folder: away with it.
		if out, err := x.Run(ctx, "umount", "-l", p); err != nil {
			errs = append(errs, fmt.Errorf("unbind %s: %w: %s", p, err, strings.TrimSpace(string(out))))
			continue
		}
		if _, ok := want[p]; !ok {
			pruneViewDirs(ctx, x, p)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(want)) {
		b := want[p]
		if mounted[p] {
			if same(ctx, x, p, b.src) {
				continue
			}
			// Stale: the source was swapped underneath (a restore). Bind it again.
			if out, err := x.Run(ctx, "umount", "-l", p); err != nil {
				errs = append(errs, fmt.Errorf("unbind stale %s: %w: %s", p, err, strings.TrimSpace(string(out))))
				continue
			}
		}
		if _, err := x.Run(ctx, "test", "-d", b.src); err != nil {
			continue // an area not made yet (an install part-way): the next refresh binds it
		}
		if out, err := x.Run(ctx, "mkdir", "-p", p); err != nil {
			errs = append(errs, fmt.Errorf("make %s: %w: %s", p, err, strings.TrimSpace(string(out))))
			continue
		}
		opts := "bind"
		if b.ro {
			opts = "bind,ro"
		}
		if out, err := x.Run(ctx, "mount", "-o", opts, b.src, p); err != nil {
			errs = append(errs, fmt.Errorf("bind %s: %w: %s", p, err, strings.TrimSpace(string(out))))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("files view: %w", err)
	}
	return nil
}

// same reports whether a folder of the view still shows its source: one directory, one device and
// inode. Anything unreadable is answered "no", which only costs a rebind.
func same(ctx context.Context, x Executor, p, src string) bool {
	out, err := x.Run(ctx, "stat", "-L", "-c", "%d:%i", p, src)
	if err != nil {
		return false
	}
	ids := nonEmptyLines(out)
	return len(ids) == 2 && ids[0] == ids[1]
}

// pruneViewDirs removes the folders an unbound path leaves empty, up to the view's root and never
// the root itself: Files holds the root, and a root made again would be one it cannot see.
func pruneViewDirs(ctx context.Context, x Executor, p string) {
	for d := p; d != files.ViewDir && strings.HasPrefix(d, files.ViewDir+"/"); d = path.Dir(d) {
		if _, err := x.Run(ctx, "rmdir", d); err != nil {
			return // not empty: something else still lives there
		}
	}
}

// viewMounts is every mount point under the view, read off /proc/self/mountinfo.
func viewMounts(x Executor) ([]string, error) {
	raw, err := x.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("files view: read the mount table: %w", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		if p := unescapeMountinfo(f[4]); strings.HasPrefix(p, files.ViewDir+"/") {
			out = append(out, p)
		}
	}
	return out, nil
}

// unescapeMountinfo undoes the kernel's octal escapes in a mountinfo path: a space is \040, which
// every app title of two words has.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
