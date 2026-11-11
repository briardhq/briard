package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"briard.io/agent/quadlet"
	"briard.io/shared/manifest"
)

// UNINSTALLING ONE APP: everything of it on the volume goes, except the copy it keeps.
//
// The data and files areas are kept, as they stand with the app stopped, as members named for the
// uninstall (quadlet.TriggerUninstall): the data area's in the ring, where installing the app again
// finds it and restores it like any other point, and the files area's beside the Versions. Their
// sidecar carries the manifest the app ran and the "Uninstalled" event, so the members are the
// app's whole presence on the volume until they go: at keptFor, at the household's Delete now, or
// to the files tier's valve once a backup holds them (filestier.go). Everything else goes at once --
// the unversioned and cache areas, the History and the Versions -- because a reinstall restores the
// state at the uninstall, which no older copy adds to.
//
// THE ORDER IS THE SAFETY: the copies are taken and flushed first, then the manifest goes and the
// node converges without the app -- its pod stopped, its units and its Files view binds gone --
// and only then are the areas deleted, so nothing holds them and nothing needs them. A crash
// part-way leaves either the app installed with an extra member, or the app gone with areas still
// there; running the uninstall again finishes either.

const (
	// app.uninstall serves an uninstall and Delete now: the host has stopped the app's containers.
	verbAppUninstall = "app.uninstall"
	// app.reinstate puts a kept files area back, the last step of installing an app again.
	verbAppReinstate = "app.reinstate"
)

// keptFor is how long an uninstall's copy stays.
const keptFor = quadlet.RetainUninstalled

// appUninstallRequest is app.uninstall's: Keep takes the copies, stamped At; without it every copy
// of the app goes, a kept one too (Delete now).
type appUninstallRequest struct {
	App  string    `json:"app"`
	Keep bool      `json:"keep,omitempty"`
	At   time.Time `json:"at"`
}

// appReinstateRequest names the data area's kept member that was just restored; its files twin,
// of the same name, is the one put back.
type appReinstateRequest struct {
	App    string `json:"app"`
	Member string `json:"member"`
}

// uninstallApp serves app.uninstall.
func uninstallApp(ctx context.Context, x Executor, run func(string, ...string) error, req appUninstallRequest) error {
	app := req.App
	if err := safeUnitName(app); err != nil {
		return err
	}
	if _, err := x.ReadFile(stagedPath(app)); err == nil {
		return fmt.Errorf("an install of %s has not finished; try again once it has", app)
	}
	raw, err := x.ReadFile(manifestPath(app))
	installed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s's manifest: %w", app, err)
	}
	if req.Keep && !installed {
		return fmt.Errorf("%s is not installed", app)
	}
	var kept []string
	if req.Keep {
		m, _, err := manifest.Parse(raw)
		if err != nil {
			return fmt.Errorf("%s's manifest does not parse: %w", app, err)
		}
		sidecar, err := json.Marshal(quadlet.SnapshotMeta{
			App: app, Trigger: quadlet.TriggerUninstall, TakenAt: req.At,
			// The host stopped the containers before asking.
			Consistency: quadlet.Quiesced, Manifest: string(raw),
			Event: &quadlet.Event{At: req.At, Reasons: []quadlet.Reason{{Kind: quadlet.ReasonAppUninstall, What: "Uninstalled"}}},
		})
		if err != nil {
			return err
		}
		// The data member whenever anything is kept, files alone too: it is in the ring, where
		// installing the app again finds it, and its files twin is found by its name.
		if m.UsesTier(manifest.TierData) || m.UsesTier(manifest.TierFiles) {
			member := quadlet.SnapshotMember(app, quadlet.TriggerUninstall, req.At)
			if err := takeSnapshot(ctx, x, run, quadlet.DataRoot(app), member, string(sidecar)); err != nil {
				return fmt.Errorf("keep %s's data: %w", app, err)
			}
			kept = append(kept, member)
		}
		if m.UsesTier(manifest.TierFiles) {
			member := filesKept(quadlet.SnapshotMember(app, quadlet.TriggerUninstall, req.At))
			if err := run("mkdir", "-p", filesVersionsDir()); err != nil {
				return err
			}
			if err := takeSnapshot(ctx, x, run, filesArea(app), member, string(sidecar)); err != nil {
				return fmt.Errorf("keep %s's files: %w", app, err)
			}
			kept = append(kept, member)
		}
	}
	if installed {
		// One flush for the copies' sidecars and the manifest's removal: the same filesystem.
		if err := run("rm", "-f", manifestPath(app), rollbackPath(app)); err != nil {
			return err
		}
		if err := run("sync", "-f", manifestDir); err != nil {
			return err
		}
		if _, err := Converge(ctx, x, ""); err != nil {
			return fmt.Errorf("stop %s: %w", app, err)
		}
	}
	// Files' view too when Files itself went, which converge no longer refreshes.
	if err := refreshFilesView(ctx, x); err != nil {
		log.Printf("app.uninstall %s: %v; Files shows what it can", app, err)
	}
	// Each area deleted only when btrfs says positively it is a subvolume.
	for _, tier := range manifest.Tiers {
		area := quadlet.Area(tier, app)
		if out, err := x.Run(ctx, "btrfs", "subvolume", "show", area); err != nil || len(out) == 0 {
			continue
		}
		if err := run("btrfs", "subvolume", "delete", area); err != nil {
			return err
		}
	}
	for _, c := range appCopies(ctx, x, app) {
		if slices.Contains(kept, c) {
			continue
		}
		if err := deleteCopy(run, c); err != nil {
			return err
		}
	}
	return nil
}

// reinstateApp serves app.reinstate: the files area's kept copy, of the same name as the data
// member just restored, becomes the files area again -- staged beside it, then swapped in, as a
// restore does -- and goes. An app that kept no files has nothing to put back.
func reinstateApp(ctx context.Context, x Executor, run func(string, ...string) error, req appReinstateRequest) error {
	if err := safeUnitName(req.App); err != nil {
		return err
	}
	app, tr, _, ok := quadlet.ParseSnapshotMember(filepath.Base(req.Member))
	if !ok || app != req.App || tr != quadlet.TriggerUninstall {
		return fmt.Errorf("app.reinstate: %q is not %s's kept copy", req.Member, req.App)
	}
	member := filesKept(req.Member)
	if out, err := x.Run(ctx, "btrfs", "subvolume", "show", member); err != nil || len(out) == 0 {
		return nil
	}
	area := filesArea(app)
	if err := makeArea(ctx, x, run, area); err != nil {
		return err
	}
	group, err := areaGroup(ctx, x, area)
	if err != nil {
		return err
	}
	staged := area + ".restoring"
	if _, err := x.Run(ctx, "btrfs", "subvolume", "show", staged); err == nil {
		if err := run("btrfs", "subvolume", "delete", staged); err != nil {
			return err
		}
	}
	if err := run("btrfs", "subvolume", "snapshot", "-i", group, member, staged); err != nil {
		return err
	}
	if err := run("btrfs", "subvolume", "delete", area); err != nil {
		return err
	}
	if err := run("mv", staged, area); err != nil {
		return err
	}
	return deleteCopy(run, member)
}

// filesKept is the files area's kept copy beside a data member's name.
func filesKept(dataMember string) string {
	return filepath.Join(filesVersionsDir(), filepath.Base(dataMember))
}

// appCopies is every member of one app on the volume: its ring, and its files area's Versions and
// kept copy. Only names we made are read, so nothing else is ever deleted.
func appCopies(ctx context.Context, x Executor, app string) []string {
	var out []string
	for _, n := range ringMembers(ctx, x, app) {
		out = append(out, quadlet.SnapshotsDir+n)
	}
	for _, n := range snapshotNames(ctx, x, filesVersionsDir()) {
		if a, ok := quadlet.SnapshotMemberApp(n); ok && a == app {
			out = append(out, filepath.Join(filesVersionsDir(), n))
		}
	}
	return out
}

// snapshotNames lists a members' directory without the sidecars; an absent one holds none.
func snapshotNames(ctx context.Context, x Executor, dir string) []string {
	out, err := x.Run(ctx, "ls", "-1", dir)
	if err != nil {
		return nil
	}
	return slices.DeleteFunc(strings.Fields(string(out)), func(n string) bool { return strings.HasSuffix(n, ".json") })
}

// deleteCopy removes a member and then its sidecars, so a member never outlives its label.
func deleteCopy(run func(string, ...string) error, member string) error {
	if err := run("btrfs", "subvolume", "delete", member); err != nil {
		return err
	}
	return run("rm", "-f", quadlet.SnapshotSidecar(member), quadlet.AppSidecar(member))
}

// A keptCopy is an uninstall's copy of an app that is not installed: a data area's (in the ring) or
// a files area's (beside the Versions).
type keptCopy struct {
	app, path string
	at        time.Time
	files     bool
}

// keptCopies lists the kept copies of every app the volume has no manifest for. One whose app is
// installed again is that app's History, no longer a kept copy. A manifest directory that cannot
// be listed lists none, so nothing is shed or expired on a read that did not answer.
func keptCopies(ctx context.Context, x Executor) []keptCopy {
	out, err := x.Run(ctx, "ls", "-1", manifestDir)
	if err != nil {
		return nil
	}
	installed := map[string]bool{}
	for _, n := range nonEmptyLines(out) {
		if a, ok := strings.CutSuffix(n, ".json"); ok {
			installed[a] = true
		}
	}
	var kept []keptCopy
	for _, dir := range []string{strings.TrimSuffix(quadlet.SnapshotsDir, "/"), filesVersionsDir()} {
		for _, n := range snapshotNames(ctx, x, dir) {
			app, tr, at, ok := quadlet.ParseSnapshotMember(n)
			if ok && tr == quadlet.TriggerUninstall && !installed[app] {
				kept = append(kept, keptCopy{app: app, path: filepath.Join(dir, n), at: at, files: dir == filesVersionsDir()})
			}
		}
	}
	slices.SortFunc(kept, func(a, b keptCopy) int { return a.at.Compare(b.at) })
	return kept
}

// backedUp reads a kept copy's mark; an unreadable sidecar is not backed up.
func backedUp(x Executor, member string) bool {
	var meta quadlet.SnapshotMeta
	raw, err := x.ReadFile(quadlet.SnapshotSidecar(member))
	return err == nil && json.Unmarshal(raw, &meta) == nil && meta.BackedUp
}

// markBackedUp records on a kept copy's sidecar that a finished backup holds it. Best-effort: a
// mark lost only keeps the copy from the valve until the next night's.
func markBackedUp(x Executor, run func(string, ...string) error, member string) {
	path := quadlet.SnapshotSidecar(member)
	var meta quadlet.SnapshotMeta
	raw, err := x.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &meta)
	}
	if err == nil && meta.BackedUp {
		return
	}
	if err == nil {
		meta.BackedUp = true
		raw, err = json.Marshal(meta)
	}
	if err == nil {
		err = writeDurably(x, run, path, string(raw))
	}
	if err != nil {
		log.Printf("backup: could not mark %s backed up: %v", filepath.Base(member), err)
	}
}

// AppUninstall uninstalls one app, keeping its copies stamped at (app.uninstall); without keep it
// deletes every copy of it, a kept one too.
func (g *Client) AppUninstall(ctx context.Context, app string, keep bool, at time.Time) error {
	return g.c.Call(ctx, verbAppUninstall, appUninstallRequest{App: app, Keep: keep, At: at}, nil)
}

// SupportsAppUninstall reports whether this guest can uninstall one app.
func (g *Client) SupportsAppUninstall() bool { return g.Supports(verbAppUninstall) }

// AppReinstate puts back the files area kept beside a restored kept member (app.reinstate).
func (g *Client) AppReinstate(ctx context.Context, app, member string) error {
	return g.c.Call(ctx, verbAppReinstate, appReinstateRequest{App: app, Member: member}, nil)
}
