package guestagent

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"briard.io/agent/quadlet"
)

// THE BACKUP RUNS IN THE BACKGROUND, the recorder check's shape (dbcheck.go): data.backup starts
// it and answers at once, data.backup.result hands the report over once. A first run copies the
// whole volume and a later one still reads every changed file; either would stall every status
// read the host makes behind it on this one-verb-at-a-time channel.
//
// WHAT IS BACKED UP is everything a fresh install needs to come back as this household, laid out
// under one root (backupRoot):
//
//	services/<service>/          the newest member of the service's ring, bind-mounted read-only
//	briard/members/<service>.json  that member's sidecar: the manifest its data was written under
//	briard/volume/               the volume's flock-scoped facts -- .vip-address, .services/, tls/
//	briard/host/<name>           the host's own facts, handed over in the request
//
// The data is the History sample, whatever trigger took it, so consistency comes with it and
// there is no second snapshot schedule; crash-consistent is what every nightly tool gives, and
// waiting for a quiesced member would skip a service that never has one. Its sidecar travels with
// it because a service's data is only usable beside the code that wrote it. The volume's facts
// and the host's are read at run time, not at the member's: they are what the household has now.
// Members are READ FROM THE RING, never from the request -- no path comes off the wire; the host's
// facts arrive as content under names the guest checks.
//
// THE REPOSITORY IS THE HOST'S: restic's REST backend, served by the host agent on the private
// link and written into the household's folder. The request carries its URL; the guest keeps
// neither it nor the host's facts past the run. THE KEY IS THE VOLUME'S (backupKey below).
//
// THE STATE IS THIS PROCESS'S, as the check's is. A restart loses a running backup and its
// report; the lock it leaves is cleared by the next run's unlock.
var backups struct {
	sync.Mutex
	busy   bool          // a backup is running
	report *BackupReport // the finished run's report, until the host collects it
}

// THE REPOSITORY KEY LIVES ON THE REPLICATED VOLUME, beside the certificate's private key: there
// is one repository per household and one key opens it, so the key is the flock's, shared by
// every node and read by whichever is Primary tonight. In a node's own state directory, each node
// of a pair would mint a key of its own and the one that promoted next could not open what the
// other wrote. Nothing needs the key without the volume -- the run, a restore and the page all
// stand on it mounted -- and a household whose volume is gone has only the copy it saved.
//
// MINTED ONLY WHERE THE VOLUME IS MOUNTED (anywhere else the path is the empty mountpoint) AND
// ONLY WHEN THE FILE IS ABSENT, never when it cannot be read: a new key over an unreadable old
// one would orphan every snapshot the old one opens. Synced like .vip-address, for the same
// reason: the next promotion may follow a power cut. Under one mutex, because the nightly run
// and the page's read may both find no key at once, and two minted keys would leave the
// repository opened under one the volume does not hold.
//
// Base32 in groups of four: a person copies it by hand from the dashboard, and 160 bits read
// aloud is eight short groups.
const (
	backupKeyFile = dataMountRoot + "/.backup-key"
	// backupKeySavedFile marks that the household said it has a copy of the key elsewhere -- the
	// household's word, so the flock's, like the key it is about.
	backupKeySavedFile = dataMountRoot + "/.backup-key-saved"
)

var backupKeyMu sync.Mutex

func backupKey(ctx context.Context, x Executor) (string, error) {
	backupKeyMu.Lock()
	defer backupKeyMu.Unlock()
	out, err := x.Run(ctx, "stat", "-c", "%m", dataMountRoot)
	if err != nil || strings.TrimSpace(string(out)) != dataMountRoot {
		return "", errors.New("the data volume is not mounted here")
	}
	if _, err := x.Run(ctx, "test", "-e", backupKeyFile); err == nil {
		b, err := x.ReadFile(backupKeyFile)
		if err != nil {
			return "", err
		}
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
		return "", errors.New(backupKeyFile + " is empty")
	}
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	enc := strings.ToLower(base32.StdEncoding.EncodeToString(raw))
	var groups []string
	for i := 0; i < len(enc); i += 4 {
		groups = append(groups, enc[i:i+4])
	}
	k := strings.Join(groups, "-")
	if out, err := x.Run(ctx, "install", "-m", "0600", "/dev/null", backupKeyFile); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := x.WriteFile(backupKeyFile, []byte(k+"\n")); err != nil {
		return "", err
	}
	if out, err := x.Run(ctx, "sync", "-f", dataMountRoot); err != nil {
		return "", fmt.Errorf("sync: %w: %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("backup: minted the household's repository key")
	return k, nil
}

// BackupKey is what the page shows of the key: the key itself, and whether the household said it
// has a copy somewhere else.
type BackupKey struct {
	Key   string `json:"key"`
	Saved bool   `json:"saved"`
}

// backupKeyRequest asks for the key, minting it if there is none; Saved true records the
// household's word that it is saved.
type backupKeyRequest struct {
	Saved bool `json:"saved,omitempty"`
}

func backupKeyState(ctx context.Context, x Executor, req backupKeyRequest) (BackupKey, error) {
	k, err := backupKey(ctx, x)
	if err != nil {
		return BackupKey{}, err
	}
	if req.Saved {
		if out, err := x.Run(ctx, "touch", backupKeySavedFile); err != nil {
			return BackupKey{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		if out, err := x.Run(ctx, "sync", "-f", dataMountRoot); err != nil {
			return BackupKey{}, fmt.Errorf("sync: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	_, err = x.Run(ctx, "test", "-e", backupKeySavedFile)
	return BackupKey{Key: k, Saved: err == nil}, nil
}

const (
	// backupRoot is the one path restic backs up, so its paths never change: restic finds a run's
	// parent snapshot by them, and member names carry their stamp -- backing up `.snapshots/...`
	// directly, or one path per service, would rescan every file whenever either changed.
	backupRoot = "/run/briard/backup"
	// backupServices holds the mount points; backupFacts the copies, which are ours alone and
	// never a mount, so clearing them cannot reach a member.
	backupServices = backupRoot + "/services"
	backupFacts    = backupRoot + "/briard"
	// backupPasswordFile holds the repository password for the run only, 0600 on tmpfs and outside
	// backupRoot: an argument or an environment variable would put it in /proc for the run's whole
	// length, and inside the root it would be backed up beside what it opens.
	backupPasswordFile = "/run/briard/backup.password"
	// backupHost is the name a snapshot is taken under when the request names no group: a guest
	// with no host (Backup below), or a node from before it had a flock id.
	backupHost = "briard"
)

// volumeFacts are the replicated volume's flock-scoped facts -- the ones every node shares and
// whichever promotes next reads -- copied whole when present. tls/ carries the private key; the repository is encrypted under the household's key.
var volumeFacts = []string{dataMountRoot + "/.vip-address", manifestDir, tlsDir}

// backupRequest is what the host hands a run: where the repository is, whose snapshots these are,
// and the facts only the host holds.
type backupRequest struct {
	// Repository is the REST endpoint's URL (http://...). The guest prefixes the backend itself,
	// so the host can only ever point a run at a REST server.
	Repository string `json:"repository"`
	// Group is the name every snapshot is taken under and the only group the retention touches:
	// restic groups snapshots by host for parent detection and for forget. THE HOUSEHOLD'S, not
	// the node's -- the flock id -- so a failover continues one ladder rather than starting a
	// second, and so a repository an earlier household wrote is never pruned by this one: a
	// fresh install writing into it keeps its own ladder, and the earlier nights are frozen, there
	// to restore from until somebody deletes them on purpose, rather than pushed out within two
	// nights by a new install's near-empty data.
	Group string `json:"group,omitempty"`
	// Host is the host's node-held facts a new install could not re-derive, by file name.
	Host map[string][]byte `json:"host,omitempty"`
}

// BackupReport is one finished run. Error set with a Snapshot is a snapshot restic took but
// could not complete cleanly (a file it could not read); Error alone is a run that saved nothing.
// Neither set is a node with nothing to back up.
type BackupReport struct {
	Snapshot   string   `json:"snapshot,omitempty"`
	Services   []string `json:"services,omitempty"`
	Files      int      `json:"files"`
	BytesAdded int64    `json:"bytesAdded"`
	Error      string   `json:"error,omitempty"`
}

// BackupState is what the host reads back of a run in the background: whether it is still
// running, and its report once it has finished, handed over once.
type BackupState struct {
	Running bool          `json:"running"`
	Report  *BackupReport `json:"report,omitempty"`
}

// startBackup starts a run in the background and reports whether it did. It does not when one is
// running, or when a finished run's report has not been collected.
func startBackup(x Executor, req backupRequest) (bool, error) {
	if !strings.HasPrefix(req.Repository, "http://") && !strings.HasPrefix(req.Repository, "https://") {
		return false, fmt.Errorf("%s: the repository is not a REST URL: %q", verbDataBackup, req.Repository)
	}
	for name := range req.Host {
		if name == "" || name == "." || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			return false, fmt.Errorf("%s: unsafe host fact name %q", verbDataBackup, name)
		}
	}
	backups.Lock()
	defer backups.Unlock()
	if backups.busy || backups.report != nil {
		return false, nil
	}
	backups.busy = true
	go func() {
		// NOT THE REQUEST'S CONTEXT: that ends with the reply, and this outlives it.
		rep := runBackup(context.Background(), x, req)
		backups.Lock()
		backups.busy, backups.report = false, &rep
		backups.Unlock()
	}()
	return true, nil
}

// backupResult answers whether a run is going, and hands over a finished one's report once.
func backupResult() BackupState {
	backups.Lock()
	defer backups.Unlock()
	s := BackupState{Running: backups.busy, Report: backups.report}
	backups.report = nil
	return s
}

// newestMembers is the member each installed service's backup reads: the newest of its ring.
// A service with no member yet (installed, never started) is left out.
func newestMembers(ctx context.Context, x Executor) (map[string]string, error) {
	names, err := manifestNames(ctx, x)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, n := range names {
		svc := strings.TrimSuffix(n, ".json")
		ring, err := listMembers(ctx, x, svc)
		if err != nil {
			return nil, err
		}
		if len(ring) > 0 {
			out[svc] = ring[len(ring)-1].Member // oldest first
		}
	}
	return out, nil
}

// runBackup is one run: lay out the tree, open (or create) the repository, clear a stale lock,
// back up, apply the retention, take the tree down. Everything that stops it becomes the
// report's Error.
func runBackup(ctx context.Context, x Executor, req backupRequest) BackupReport {
	run := func(name string, args ...string) ([]byte, error) {
		out, err := x.Run(ctx, name, args...)
		if err != nil {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}
	fail := func(err error) BackupReport {
		log.Printf("backup: %v", err)
		return BackupReport{Error: err.Error()}
	}
	members, err := newestMembers(ctx, x)
	if err != nil {
		return fail(fmt.Errorf("the ring could not be read: %w", err))
	}
	if len(members) == 0 {
		return BackupReport{}
	}

	secret := func(path string, data []byte) error {
		if _, err := run("install", "-D", "-m", "0600", "/dev/null", path); err != nil {
			return err
		}
		if err := x.WriteFile(path, data); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		return nil
	}
	key, err := backupKey(ctx, x)
	if err != nil {
		return fail(fmt.Errorf("no repository key: %w", err))
	}
	defer x.Run(ctx, "rm", "-f", backupPasswordFile)
	if err := secret(backupPasswordFile, []byte(key)); err != nil {
		return fail(err)
	}

	// THE TREE. The copies first: a run that died left them (and they may hold the host's
	// secrets), and they are never a mount. Then a mount a dead run left is in the way of this
	// one; not being mounted is the ordinary answer.
	var mounted, services []string
	defer func() {
		for _, p := range mounted {
			if _, err := run("umount", p); err != nil {
				log.Printf("backup: %v", err)
			}
		}
		if _, err := run("rm", "-rf", backupFacts); err != nil {
			log.Printf("backup: %v", err)
		}
	}()
	if _, err := run("rm", "-rf", backupFacts); err != nil {
		return fail(err)
	}
	for _, d := range []string{backupFacts + "/members", backupFacts + "/volume"} {
		if _, err := run("mkdir", "-p", d); err != nil {
			return fail(err)
		}
	}
	for _, svc := range slices.Sorted(maps.Keys(members)) {
		member, p := members[svc], backupServices+"/"+svc
		_, _ = x.Run(ctx, "umount", p)
		if _, err := run("mkdir", "-p", p); err != nil {
			return fail(err)
		}
		if _, err := run("mount", "-o", "bind,ro", member, p); err != nil {
			return fail(err)
		}
		mounted = append(mounted, p)
		services = append(services, svc)
		if _, err := run("cp", "-a", quadlet.SnapshotSidecar(member), backupFacts+"/members/"+svc+".json"); err != nil {
			return fail(err)
		}
	}
	for _, f := range volumeFacts {
		if _, err := x.Run(ctx, "test", "-e", f); err != nil {
			continue // not every volume has every fact: no VIP set, no cert yet
		}
		if _, err := run("cp", "-a", f, backupFacts+"/volume/"); err != nil {
			return fail(err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(req.Host)) {
		if err := secret(backupFacts+"/host/"+name, req.Host[name]); err != nil {
			return fail(err)
		}
	}

	repo := []string{"--repo", "rest:" + req.Repository, "--password-file", backupPasswordFile,
		// No local cache: the guest's root is tmpfs, so a cache is memory, and the repository is
		// one hop away on the private link.
		"--no-cache"}
	restic := func(args ...string) ([]byte, error) { return run("restic", append(slices.Clone(repo), args...)...) }

	// Opened, or else created. init refuses a repository that exists, so a config that could
	// not be read for any other reason (a wrong password, the store down) fails here too rather
	// than being replaced.
	if _, err := restic("cat", "config"); err != nil {
		if _, ierr := restic("init"); ierr != nil {
			return fail(errors.Join(err, ierr))
		}
	}
	// ONE WRITER BY CONSTRUCTION -- only the Primary runs this, one run at a time -- so a lock
	// still in the repository is always a run that died (a guest reboot mid-run).
	if _, err := restic("unlock"); err != nil {
		return fail(err)
	}
	group := req.Group
	if group == "" {
		group = backupHost
	}
	// HA's own backup tarballs are a second copy of the same data inside the first.
	out, err := restic("backup", "--json", "--quiet", "--host", group,
		"--exclude", "**/app/backups", backupRoot)
	rep := backupSummary(out)
	rep.Services = services
	if err != nil {
		rep.Error = err.Error()
		log.Printf("backup: %v", err)
		if rep.Snapshot == "" {
			return rep
		}
	}
	// OUR GROUP ONLY (backupRequest.Group): restic applies the policy per group and keep-* counts
	// snapshots, not days, so another household's nights in this repository stay exactly as they
	// are -- but the flag says so, rather than the policy's arithmetic.
	if _, err := restic("forget", "--host", group, "--keep-daily", "2", "--keep-weekly", "1", "--prune"); err != nil {
		rep.Error = strings.TrimSpace(rep.Error + "\n" + err.Error())
		log.Printf("backup: %v", err)
	}
	log.Printf("backup: snapshot %s of %s: %d files, %d bytes added", rep.Snapshot, strings.Join(services, ", "), rep.Files, rep.BytesAdded)
	return rep
}

// backupSummary reads `restic backup --json`'s summary line out of its output.
func backupSummary(out []byte) BackupReport {
	var rep BackupReport
	for _, line := range strings.Split(string(out), "\n") {
		var s struct {
			Type      string `json:"message_type"`
			Snapshot  string `json:"snapshot_id"`
			Files     int    `json:"total_files_processed"`
			DataAdded int64  `json:"data_added"`
		}
		if json.Unmarshal([]byte(line), &s) == nil && s.Type == "summary" {
			rep = BackupReport{Snapshot: s.Snapshot, Files: s.Files, BytesAdded: s.DataAdded}
		}
	}
	return rep
}

// Backup is one run in line FOR A GUEST WITH NO HOST, the accommodation CheckRecorder makes: the
// agent-less rigs drive the product's own backup through it against a real store. Nothing in the
// product invokes it; the host's facts and the group are the host's, and a rig has neither.
func Backup(ctx context.Context, x Executor, url string) BackupReport {
	return runBackup(ctx, x, backupRequest{Repository: url})
}

// THE RESTORE'S GUEST HALF: one service's copy from one night of the backup, MATERIALISED AS A
// RING MEMBER -- a read-only subvolume with a sidecar, beside the members the ring took itself.
// Putting it back is then the ordinary restore of a member (the host's service-restore: the
// images first, the undo point, stop, swap, the member's manifest, converge), so there is one
// restore path and the backup joins it rather than adding a second.
//
// WHAT IT READS is what the backup wrote (runBackup): the service's data from
// services/<service>/ and the member's own sidecar from briard/members/<service>.json, which
// names the manifest the data was written under -- the sidecar is the member's, verbatim, except
// for its trigger (TriggerFromBackup says where these bytes come from) and Pending (nothing is
// waiting to evaluate it). The snapshot may be any night's, of any group in the repository
// ("latest" for the newest), so an earlier install's nights restore the same way as this one's.
//
// AN ACT, in line: it writes the ring, and the channel serves acts one at a time beside its
// reads, so a restore that takes minutes stalls nothing the host asks meanwhile.
//
// SIDECAR BEFORE DATA, and the member goes if either cannot be finished: a half-restored member
// would be offered as a restore point, which is worse than none. The subvolume is created empty,
// restic restores into it, and only then is it made read-only and labelled.
type backupRestoreRequest struct {
	Repository string `json:"repository"`
	Snapshot   string `json:"snapshot"`
	Service    string `json:"service"`
}

// backupSnapshotID is what a restic snapshot may be named by: a (prefix of a) hex id, or latest.
var backupSnapshotID = regexp.MustCompile(`^([0-9a-f]{8,64}|latest)$`)

func restoreBackupMember(ctx context.Context, x Executor, req backupRestoreRequest, now time.Time) (string, error) {
	if !strings.HasPrefix(req.Repository, "http://") && !strings.HasPrefix(req.Repository, "https://") {
		return "", fmt.Errorf("the repository is not a REST URL: %q", req.Repository)
	}
	if !backupSnapshotID.MatchString(req.Snapshot) {
		return "", fmt.Errorf("%q is not a snapshot id", req.Snapshot)
	}
	if err := safeUnitName(req.Service); err != nil {
		return "", err
	}
	run := func(name string, args ...string) ([]byte, error) {
		out, err := x.Run(ctx, name, args...)
		if err != nil {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}
	key, err := backupKey(ctx, x)
	if err != nil {
		return "", fmt.Errorf("no repository key: %w", err)
	}
	defer x.Run(ctx, "rm", "-f", backupPasswordFile)
	if _, err := run("install", "-D", "-m", "0600", "/dev/null", backupPasswordFile); err != nil {
		return "", err
	}
	if err := x.WriteFile(backupPasswordFile, []byte(key)); err != nil {
		return "", fmt.Errorf("write %s: %w", backupPasswordFile, err)
	}
	restic := func(args ...string) ([]byte, error) {
		return run("restic", append([]string{"--repo", "rest:" + req.Repository, "--password-file", backupPasswordFile, "--no-cache"}, args...)...)
	}

	raw, err := restic("dump", req.Snapshot, backupFacts+"/members/"+req.Service+".json")
	if err != nil {
		return "", fmt.Errorf("that night has no %s: %w", req.Service, err)
	}
	var meta quadlet.SnapshotMeta
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Service != req.Service || meta.Manifest == "" {
		return "", fmt.Errorf("the backup's sidecar for %s does not describe it (%v)", req.Service, err)
	}
	meta.Trigger, meta.Pending = quadlet.TriggerFromBackup, false
	sidecar, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	member := quadlet.SnapshotMember(req.Service, quadlet.TriggerFromBackup, now)
	if _, err := x.Run(ctx, "btrfs", "subvolume", "show", member); err == nil {
		return "", fmt.Errorf("%s already exists -- refusing to replace a ring member", member)
	}
	if _, err := run("btrfs", "subvolume", "create", member); err != nil {
		return "", err
	}
	undo := func(err error) (string, error) {
		if _, derr := run("btrfs", "subvolume", "delete", member); derr != nil {
			return "", fmt.Errorf("%w; AND the half-made member could not be removed: %v", err, derr)
		}
		return "", fmt.Errorf("%w (nothing was left in the ring)", err)
	}
	if _, err := restic("restore", req.Snapshot+":"+backupServices+"/"+req.Service, "--target", member); err != nil {
		return undo(err)
	}
	if _, err := run("btrfs", "property", "set", "-ts", member, "ro", "true"); err != nil {
		return undo(err)
	}
	if err := x.WriteFile(quadlet.SnapshotSidecar(member), sidecar); err != nil {
		return undo(fmt.Errorf("write the sidecar: %w", err))
	}
	if _, err := run("sync", "-f", dataMountRoot); err != nil {
		return undo(err)
	}
	log.Printf("backup: %s of %s from snapshot %s is in the ring as %s", meta.TakenAt.Format(time.RFC3339), req.Service, req.Snapshot, member)
	return member, nil
}

// RestoreBackupMember is one materialisation in line FOR A GUEST WITH NO HOST, as Backup is:
// the agent-less rigs prove it against a real ring and a real store.
func RestoreBackupMember(ctx context.Context, x Executor, url, snapshot, service string) (string, error) {
	return restoreBackupMember(ctx, x, backupRestoreRequest{Repository: url, Snapshot: snapshot, Service: service}, time.Now())
}
