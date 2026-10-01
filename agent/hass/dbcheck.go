package hass

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path"
	"sort"
	"strings"
	"time"

	"briard.io/shared/manifest"
)

// THE RECORDER CHECK: finding damage in Home Assistant's history database before Home Assistant
// does, and putting back a copy that passed the same check.
//
// WHY BRIARD HAS TO LOOK. A query that hits a bad page makes the recorder rename the database,
// its -wal and its -shm to `*.corrupt.<isotime>` and start empty, with no notification and the app
// still healthy. Its startup check reads one row per table, and the only routine full read is the
// monthly repack (a plain VACUUM in the second Sunday's nightly purge), so damage can sit for a
// month: past every ring member, which is when undo stops being a remedy. Restoring an UNCHECKED
// copy only resets again at the next full read.
//
// SO EACH NIGHT ONE QUIESCED MEMBER IS CHECKED (`PRAGMA quick_check`, read-only, in a throwaway
// container of Home Assistant's own image: its sqlite is the right version by construction and
// the guest needs no tool for it), and the ring holds a member every RetainEvery through the app
// sidecar, so a corrupt finding has a recent copy to restore from. A clean finding releases every
// older hold.
//
// THE RESTORE IS GATED ON TWO POSITIVE READS (a destructive act needs a positive confirmation):
// the copy checked clean, and its schema version is not newer than the live database's. The
// recorder migrates any older schema forward on start, one version at a time, and refuses only a
// newer one. Either read failing means no restore.

// The app sidecar's key for this state, and the states it holds.
const (
	DBKey = "hass-db"

	// DBRetained: a quiesced sample kept as a restore candidate.
	DBRetained = "retained"
	// DBChecking: the check is reading this member. A crash leaves it, and the next night clears it.
	DBChecking = "checking"
	// DBClean: this member passed the check; every older hold is released.
	DBClean = "clean"
)

// RetainEvery is how far apart the retained samples are, and so the most history a restore loses.
const RetainEvery = 8 * time.Hour

// The check's two verdicts. No verdict is the empty string.
const (
	VerdictClean   = "clean"
	VerdictCorrupt = "corrupt"
)

// dbName is the recorder database, in the config directory's root.
const dbName = "home-assistant_v2.db"

// Member is one ring member as the recorder check sees it.
type Member struct {
	Dir      string    // the config directory inside the member
	App      string    // the member's app sidecar, which may not exist
	At       time.Time // when it was taken
	Quiesced bool      // the bytes were flushed or held still
}

// DBRecord is this check's state in an app sidecar.
type DBRecord struct {
	State string `json:"state"`
}

// Record reads a member's hass-db record, or reports there is none.
func Record(x Executor, app string) (DBRecord, bool) {
	all, err := readApp(x, app)
	if err != nil {
		return DBRecord{}, false
	}
	raw, ok := all[DBKey]
	if !ok {
		return DBRecord{}, false
	}
	var r DBRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return DBRecord{}, false
	}
	return r, true
}

// readApp reads an app sidecar's keys. An absent file is an empty map and an error.
func readApp(x Executor, app string) (map[string]json.RawMessage, error) {
	b, err := x.ReadFile(app)
	if err != nil {
		return map[string]json.RawMessage{}, err
	}
	all := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &all); err != nil {
		return map[string]json.RawMessage{}, err
	}
	return all, nil
}

// SetRecord writes a member's hass-db record, or removes it when r is nil. Other keys are kept,
// and a sidecar left with no keys is removed, since its existence is what holds the member.
//
// tmp + rename + `sync -f`: the next node to read this may be the one that promotes after this
// one dies.
func SetRecord(ctx context.Context, x Executor, app string, r *DBRecord) error {
	all, _ := readApp(x, app)
	if r == nil {
		if _, ok := all[DBKey]; !ok {
			return nil
		}
		delete(all, DBKey)
	} else {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		all[DBKey] = b
	}
	if len(all) == 0 {
		_, err := x.Run(ctx, "rm", "-f", app)
		return err
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	if err := x.WriteFile(app+".tmp", b); err != nil {
		return err
	}
	if out, err := x.Run(ctx, "mv", "-f", app+".tmp", app); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	_, err = x.Run(ctx, "sync", "-f", app)
	return err
}

// Sampled is the take path's half: a QUIESCED sample taken at least RetainEvery after the newest
// member holding a record is retained. ring is the service's members, the new one among them.
//
// It only ever creates a sidecar, on a member that has just been taken; only the check rewrites
// one.
func Sampled(ctx context.Context, x Executor, ring []Member, m Member) error {
	if !m.Quiesced {
		return nil
	}
	for _, o := range ring {
		if o.App == m.App || o.At.After(m.At) {
			continue
		}
		if _, ok := Record(x, o.App); ok && m.At.Sub(o.At) < RetainEvery {
			return nil
		}
	}
	return SetRecord(ctx, x, m.App, &DBRecord{State: DBRetained})
}

// DBReport is what one night's check found and did. The host reads it to tell the household.
type DBReport struct {
	Checked string `json:"checked,omitempty"` // the member checked
	Verdict string `json:"verdict,omitempty"` // VerdictClean, VerdictCorrupt, or "" when the check could not run
	// RestoredFrom is when the member the database came back from was taken; zero when nothing was
	// restored.
	RestoredFrom time.Time `json:"restored_from,omitzero"`
	Why          string    `json:"why,omitempty"` // why there is no verdict, or no restore
}

// Nightly is one night's check over ring (oldest first), with live the config directory Home
// Assistant runs on. On a corrupt finding it searches for a copy to put back and hands it to
// restore, which owns stopping the app, the before-sample and starting it again.
func Nightly(ctx context.Context, x Executor, image, live string, ring []Member, restore func(Member) error) DBReport {
	sort.Slice(ring, func(i, j int) bool { return ring[i].At.Before(ring[j].At) })
	// A `checking` left by a crash holds nothing any more.
	for _, m := range ring {
		if r, ok := Record(x, m.App); ok && r.State == DBChecking {
			release(ctx, x, m)
		}
	}
	newest := -1
	for i, m := range ring {
		if m.Quiesced {
			newest = i
		}
	}
	if newest < 0 {
		return DBReport{Why: "no quiesced member to check"}
	}
	m := ring[newest]
	rep := DBReport{Checked: m.Dir}
	// HELD THROUGH THE CHECK, so a take landing meanwhile cannot prune it from under the read. A
	// member already held keeps its record.
	_, held := Record(x, m.App)
	if !held {
		if err := SetRecord(ctx, x, m.App, &DBRecord{State: DBChecking}); err != nil {
			rep.Why = "could not hold the member for the check: " + err.Error()
			return rep
		}
	}
	verdict, _, err := Check(ctx, x, image, m.Dir, true)
	if err != nil {
		if !held {
			release(ctx, x, m)
		}
		rep.Why = "the check could not run: " + err.Error()
		return rep
	}
	rep.Verdict = verdict
	if verdict == VerdictClean {
		markClean(ctx, x, ring, newest)
		return rep
	}
	release(ctx, x, m) // a damaged copy is no restore candidate
	from, why := search(ctx, x, image, live, ring[:newest])
	if why != "" {
		rep.Why = why
		return rep
	}
	markClean(ctx, x, ring, from)
	if err := restore(ring[from]); err != nil {
		rep.Why = "the restore failed: " + err.Error()
		return rep
	}
	rep.RestoredFrom = ring[from].At
	return rep
}

// search is the newest held member in ring that checks clean and whose schema is not newer than
// the live database's, as its index, or the reason there is none.
//
// ANY READ THAT FAILS ENDS THE SEARCH: an older member that would check clean is still a guess
// about one that could not be read, and a restore on a guess loses history the household had.
func search(ctx context.Context, x Executor, image, live string, ring []Member) (int, string) {
	_, liveSchema, err := Check(ctx, x, image, live, false)
	if err == nil && liveSchema < 0 {
		err = fmt.Errorf("no schema version")
	}
	if err != nil {
		return -1, "the live database's schema could not be read: " + err.Error()
	}
	for i := len(ring) - 1; i >= 0; i-- {
		m := ring[i]
		r, ok := Record(x, m.App)
		if !ok {
			continue
		}
		verdict, schema, err := Check(ctx, x, image, m.Dir, r.State != DBClean)
		if err != nil {
			return -1, fmt.Sprintf("%s could not be checked: %v", path.Dir(m.Dir), err)
		}
		if verdict == VerdictCorrupt {
			release(ctx, x, m)
			continue
		}
		if schema < 0 {
			return -1, fmt.Sprintf("%s checked clean but its schema version could not be read", path.Dir(m.Dir))
		}
		if schema > liveSchema {
			log.Printf("hass-db: %s has schema %d, newer than the live %d; not restoring from it", path.Dir(m.Dir), schema, liveSchema)
			continue
		}
		return i, ""
	}
	return -1, "no held copy checked clean"
}

// markClean records ring[i] clean, THEN releases every older hold. A crash between the two only
// leaves extra holds, which the next clean night releases.
func markClean(ctx context.Context, x Executor, ring []Member, i int) {
	if err := SetRecord(ctx, x, ring[i].App, &DBRecord{State: DBClean}); err != nil {
		log.Printf("hass-db: could not record %s clean: %v", path.Dir(ring[i].Dir), err)
		return
	}
	for _, m := range ring[:i] {
		release(ctx, x, m)
	}
}

func release(ctx context.Context, x Executor, m Member) {
	if err := SetRecord(ctx, x, m.App, nil); err != nil {
		log.Printf("hass-db: could not release %s: %v", path.Dir(m.Dir), err)
	}
}

// checkScript reads the database in /db without writing anything: immutable=1 takes no lock and
// opens no -wal, so it works on a read-only member, and on the live database it reads the main
// file alone. A non-empty -wal in a member is never read: its frames are checked the next night,
// once they are in the main file. "full" adds quick_check to the schema read.
//
// THE ERROR CLASSES ARE THE VERDICT. OperationalError (cannot open, I/O) is "could not run";
// any other DatabaseError ("malformed", "not a database") is damage, the same messages the
// recorder resets on.
const checkScript = `
import json, sqlite3, sys
out = {}
try:
    c = sqlite3.connect("file:/db/` + dbName + `?mode=ro&immutable=1", uri=True)
    if sys.argv[1] == "full":
        try:
            rows = [r[0] for r in c.execute("PRAGMA quick_check(5)")]
            out["check"] = "ok" if rows == ["ok"] else "corrupt"
            out["detail"] = "; ".join(rows)[:300]
        except sqlite3.OperationalError:
            raise
        except sqlite3.DatabaseError as e:
            out["check"], out["detail"] = "corrupt", str(e)
    if out.get("check") != "corrupt":
        try:
            out["schema"] = c.execute("SELECT MAX(schema_version) FROM schema_changes").fetchone()[0]
        except sqlite3.Error as e:
            out["schema_error"] = str(e)
except sqlite3.OperationalError as e:
    out["error"] = str(e)
print(json.dumps(out))
`

// Check reads the recorder database in dir: whether it is damaged (full only; otherwise the
// verdict is clean) and its schema version, or -1 when that could not be read. An error means no
// answer at all, never damage.
//
// -1, NEVER ZERO, for an unread schema: zero compares as older than anything, which is the answer
// that would let a restore through.
func Check(ctx context.Context, x Executor, image, dir string, full bool) (verdict string, schema int, err error) {
	mode := "schema"
	if full {
		mode = "full"
	}
	out, err := x.Run(ctx, "podman", "run", "--rm", "--pull=never", "--network=none",
		"--entrypoint", "python3", "-v", dir+":/db:ro", image, "-c", checkScript, mode)
	if err != nil {
		return "", -1, fmt.Errorf("%w: %s", err, lastLine(out))
	}
	var r struct {
		Check       string `json:"check"`
		Detail      string `json:"detail"`
		Schema      *int   `json:"schema"`
		SchemaError string `json:"schema_error"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal([]byte(lastLine(out)), &r); err != nil {
		return "", -1, fmt.Errorf("the check's answer does not parse: %w", err)
	}
	switch {
	case r.Error != "":
		return "", -1, fmt.Errorf("%s", r.Error)
	case r.Check == "corrupt":
		log.Printf("hass-db: %s is damaged: %s", dir, r.Detail)
		return VerdictCorrupt, -1, nil
	case full && r.Check != "ok":
		return "", -1, fmt.Errorf("the check gave no verdict")
	case r.Schema == nil:
		log.Printf("hass-db: %s has no readable schema version: %s", dir, r.SchemaError)
		return VerdictClean, -1, nil
	}
	return VerdictClean, *r.Schema, nil
}

// lastLine is the last non-empty line of a command's output: podman may say things first.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// RestoreDB puts the recorder database from one config directory into the live one. The caller
// has stopped Home Assistant. It reports whether the live directory was touched: a failure before
// that leaves everything as it was, so the app may be started again.
//
// COPIES FIRST, then the destructive steps. The live -wal goes BEFORE the database is replaced:
// a stale -wal beside a different database is replayed into it. The `*.corrupt.*` renames the
// recorder made of the damaged database go too; the before-sample holds them.
func RestoreDB(ctx context.Context, x Executor, from, live string) (touched bool, err error) {
	run := func(name string, args ...string) error {
		if out, err := x.Run(ctx, name, args...); err != nil {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	db, staged := live+"/"+dbName, live+"/"+dbName+".restoring"
	wal, stagedWal := db+"-wal", staged+"-wal"
	_ = run("rm", "-f", staged, stagedWal)
	if err := run("cp", "-a", "--reflink=auto", from+"/"+dbName, staged); err != nil {
		return false, err
	}
	// Only a -wal with frames in it: an empty one is what Home Assistant creates anyway.
	withWal := run("test", "-s", from+"/"+dbName+"-wal") == nil
	if withWal {
		if err := run("cp", "-a", "--reflink=auto", from+"/"+dbName+"-wal", stagedWal); err != nil {
			_ = run("rm", "-f", staged, stagedWal)
			return false, err
		}
	}
	if err := run("rm", "-f", wal, db+"-shm"); err != nil {
		return true, err
	}
	if err := run("mv", "-f", staged, db); err != nil {
		return true, err
	}
	if withWal {
		if err := run("mv", "-f", stagedWal, wal); err != nil {
			return true, err
		}
	}
	for _, rel := range Corrupt(ctx, x, live) {
		if strings.HasPrefix(rel, dbName) {
			if err := run("rm", "-f", live+"/"+rel); err != nil {
				return true, err
			}
		}
	}
	return true, run("sync", "-f", db)
}

// ConfigContainer is the container whose data directory is Home Assistant's config directory:
// the one that keeps state. The others share the subvolume and write nothing of their own.
func ConfigContainer(m manifest.Manifest) (manifest.Container, bool) {
	for _, c := range m.Containers {
		if c.Mount != "" {
			return c, true
		}
	}
	return manifest.Container{}, false
}
