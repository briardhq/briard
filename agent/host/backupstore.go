package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"briard.io/agent/platform"
	"briard.io/shared/atomicfile"
)

// THE BACKUP REPOSITORY'S STORE: restic's REST backend protocol, served to the guest over the
// private link and written into the household's backup folder on this machine.
//
// THE ONE LISTENER THAT SERVES THE GUEST. Every other channel is the host driving the guest; this
// one the guest dials, because restic runs where the volume is mounted and the folder is here. A
// file server over HTTP is the same on every host OS, where a shared filesystem into the VM is
// not. It listens only on the host's own private-link address and answers only the guest's node
// IP; what crosses it is restic's ciphertext, which the repository key -- not this door --
// protects.
//
// THE FOLDER IS THE PERSON'S. Every file and directory written is given the folder's own owner, so
// they can copy, move, delete or restore it without us; the folder's owner is the fact, so nothing
// records a uid, and a folder moved or re-owned is followed on the next write. The folder itself is
// never created here: a missing folder may be an unmounted disk, and creating it would write the
// backup onto whatever is underneath.
//
// WRITES ARE DURABLE AND NEVER OVERWRITE. Each file lands by temp + fsync + rename with its
// directory flushed after, so restic is told "saved" only of a file that survives a power cut.
// Every name is a content hash, so a second write of one that exists is refused rather than
// replacing it -- restic's own REST server answers the same.

// backupStorePort is the store's port on the host's private-link address.
const backupStorePort = "7791"

// backupStore serves one repository directory to one client address.
type backupStore struct {
	dir    string                  // the repository: the household's backup folder
	client string                  // the only source address answered: the guest's node IP
	own    func(path string) error // gives path the folder's owner (platform.ChownLike)
}

// backupObjectTypes are the repository's directories, one per kind of object restic stores.
var backupObjectTypes = []string{"data", "keys", "locks", "snapshots", "index"}

// backupObjectName is every object name restic writes: a SHA-256, hex. Anything else is refused
// before it can become a path.
var backupObjectName = regexp.MustCompile(`^[0-9a-f]{64}$`)

const resticV2 = "application/vnd.x.restic.rest.v2"

func (s *backupStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err != nil || host != s.client {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(s.dir); err != nil {
		// THE FOLDER IS GONE: unmounted, deleted, or a home not mounted while its user is logged
		// out. Say so to restic as a server error; the run fails, and the run's report says why.
		http.Error(w, "the backup folder is not there: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	p := strings.Trim(r.URL.Path, "/")
	switch {
	case p == "":
		if r.Method == http.MethodPost && r.URL.Query().Get("create") == "true" {
			s.create(w)
			return
		}
	case p == "config":
		s.object(w, r, filepath.Join(s.dir, "config"))
		return
	default:
		typ, name, _ := strings.Cut(p, "/")
		if !isObjectType(typ) {
			break
		}
		if name == "" {
			if r.Method == http.MethodGet {
				s.list(w, r, typ)
				return
			}
			break
		}
		if !backupObjectName.MatchString(name) {
			http.Error(w, "bad object name", http.StatusBadRequest)
			return
		}
		s.object(w, r, s.path(typ, name))
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func isObjectType(t string) bool {
	for _, o := range backupObjectTypes {
		if t == o {
			return true
		}
	}
	return false
}

// path is where an object lives: data/ is fanned out by the name's first two hex digits, as in
// every restic repository, so a folder opened with plain restic is the same repository.
func (s *backupStore) path(typ, name string) string {
	if typ == "data" {
		return filepath.Join(s.dir, typ, name[:2], name)
	}
	return filepath.Join(s.dir, typ, name)
}

// create lays out an empty repository's directories. Idempotent: restic writes the config after.
func (s *backupStore) create(w http.ResponseWriter) {
	dirs := []string{}
	for _, t := range backupObjectTypes {
		dirs = append(dirs, filepath.Join(s.dir, t))
	}
	for i := 0; i < 256; i++ {
		dirs = append(dirs, filepath.Join(s.dir, "data", fmt.Sprintf("%02x", i)))
	}
	for _, d := range dirs {
		if err := s.mkdir(d); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
}

// mkdir makes one directory inside the folder, the folder's owner's.
func (s *backupStore) mkdir(d string) error {
	if err := os.Mkdir(d, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return s.own(d)
}

// object answers HEAD/GET/POST/DELETE on one file.
func (s *backupStore) object(w http.ResponseWriter, r *http.Request, path string) {
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		f, err := os.Open(path)
		if err != nil {
			storeError(w, err)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			storeError(w, err)
			return
		}
		// Ranges and HEAD's length come with it: restic reads parts of a pack by Range.
		http.ServeContent(w, r, "", fi.ModTime(), f)
	case http.MethodPost:
		if _, err := os.Lstat(path); err == nil {
			http.Error(w, "exists", http.StatusForbidden)
			return
		}
		if err := s.save(path, r.Body); err != nil {
			storeError(w, err)
		}
	case http.MethodDelete:
		if err := os.Remove(path); err != nil {
			storeError(w, err)
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// save writes one object durably: temp beside it, fsync, the folder's owner, rename, the
// directory flushed. A failure at any step leaves no object, only a temp that is removed.
func (s *backupStore) save(path string, body io.Reader) error {
	dir := filepath.Dir(path)
	if err := s.mkdir(dir); err != nil { // a data/xx a hand-made repository lacks
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, body); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := s.own(tmp.Name()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	ok = true
	return atomicfile.SyncDir(dir)
}

// list answers a directory listing, in the protocol version the client asked for.
func (s *backupStore) list(w http.ResponseWriter, r *http.Request, typ string) {
	type entry struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	dirs := []string{filepath.Join(s.dir, typ)}
	if typ == "data" {
		subs, err := os.ReadDir(dirs[0])
		if err != nil {
			storeError(w, err)
			return
		}
		dirs = dirs[:0]
		for _, d := range subs {
			if d.IsDir() {
				dirs = append(dirs, filepath.Join(s.dir, typ, d.Name()))
			}
		}
	}
	entries := []entry{}
	for _, d := range dirs {
		files, err := os.ReadDir(d)
		if err != nil {
			storeError(w, err)
			return
		}
		for _, f := range files {
			if !backupObjectName.MatchString(f.Name()) { // temps, and anything not restic's
				continue
			}
			fi, err := f.Info()
			if err != nil {
				continue // removed between the listing and the stat
			}
			entries = append(entries, entry{Name: f.Name(), Size: fi.Size()})
		}
	}
	var body any = entries
	if r.Header.Get("Accept") == resticV2 {
		w.Header().Set("Content-Type", resticV2)
	} else {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name
		}
		body = names
		w.Header().Set("Content-Type", "application/vnd.x.restic.rest.v1")
	}
	_ = json.NewEncoder(w).Encode(body)
}

// storeError maps a filesystem error to the status restic reads: absent is 404, anything else 500.
func storeError(w http.ResponseWriter, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// serveBackupStore serves the repository at dir to the guest until ctx ends. Skipped -- not an
// error -- when there is no folder (backup disabled) or no private link to serve it on. Every
// failure is logged and non-fatal, as the admin socket's is: a node that cannot take tonight's
// backup still serves the household, and the run's report says the store is unreachable.
//
// THE BIND WAITS FOR THE ADDRESS. The network converger puts hostIP on the private link, which may
// not exist yet when the agent starts, so the bind retries until it holds. Once held it outlives
// the address going and coming back with a guest relaunch: a bound socket keeps its address.
func serveBackupStore(ctx context.Context, dir, hostIP, guestIP string, logf func(string, ...any)) {
	if dir == "" || hostIP == "" || guestIP == "" {
		return
	}
	var ln net.Listener
	for said := false; ; said = true {
		var err error
		if ln, err = net.Listen("tcp", net.JoinHostPort(hostIP, backupStorePort)); err == nil {
			break
		}
		if !said {
			logf("backup store: listen: %v (retrying until the private link is up)", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	store := &backupStore{dir: dir, client: guestIP, own: func(p string) error { return platform.ChownLike(dir, p) }}
	srv := &http.Server{Handler: store, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	logf("backup store: serving %s to %s on %s", dir, guestIP, ln.Addr())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logf("backup store: %v", err)
	}
}
