package host

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// storeRig serves an empty folder through the store to this process (httptest's client is
// 127.0.0.1), recording every path given the folder's owner.
func storeRig(t *testing.T) (*backupStore, *httptest.Server, map[string]bool) {
	t.Helper()
	var mu sync.Mutex
	owned := map[string]bool{}
	s := &backupStore{dir: t.TempDir(), client: "127.0.0.1", own: func(_, p string) error {
		mu.Lock()
		defer mu.Unlock()
		owned[p] = true
		return nil
	}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv, owned
}

// restic runs the real client against the store. NOT SKIPPED WHEN MISSING: the dev shell carries
// it, and a test that passes by not running proves nothing.
func restic(t *testing.T, url string, args ...string) string {
	t.Helper()
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("restic is not on PATH (it is in the dev shell): %v", err)
	}
	cmd := exec.Command(bin, append([]string{"--repo", "rest:" + url + "/", "--no-cache", "--quiet"}, args...)...)
	cmd.Env = append(os.Environ(), "RESTIC_PASSWORD=correct horse")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("restic %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestRealResticRoundTripsThroughTheStore: init, two backups, a full data check, forget + prune
// (deletes), and a restore that matches -- the protocol as restic itself speaks it, v2 listings
// and Range reads included. And every file it left in the folder was given the folder's owner.
func TestRealResticRoundTripsThroughTheStore(t *testing.T) {
	s, srv, owned := storeRig(t)
	src := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a", strings.Repeat("first ", 100000))
	write("b", "small")

	restic(t, srv.URL, "init")
	restic(t, srv.URL, "backup", "--host", "briard", src)
	write("b", "changed")
	restic(t, srv.URL, "backup", "--host", "briard", src)
	restic(t, srv.URL, "check", "--read-data")
	restic(t, srv.URL, "forget", "--keep-last", "1", "--prune")
	if n := strings.Count(restic(t, srv.URL, "snapshots", "--json"), `"id"`); n != 1 {
		t.Fatalf("%d snapshots after forget --keep-last 1, want 1", n)
	}
	dst := t.TempDir()
	restic(t, srv.URL, "restore", "latest", "--target", dst)
	for name, want := range map[string]string{"a": strings.Repeat("first ", 100000), "b": "changed"} {
		got, err := os.ReadFile(filepath.Join(dst, src, name))
		if err != nil || string(got) != want {
			t.Fatalf("restored %s: %v (%d bytes, want %d)", name, err, len(got), len(want))
		}
	}

	// THE EXIT GUARANTEE: the folder is a standard repository on its own, no briard serving it.
	cmd := exec.Command("restic", "--repo", s.dir, "--no-cache", "--quiet", "check", "--read-data")
	cmd.Env = append(os.Environ(), "RESTIC_PASSWORD=correct horse")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the folder does not open as a plain restic repository: %v\n%s", err, out)
	}

	// Every object restic left is a file the folder's owner was given, under the name it landed
	// with; so is every directory. Temps are given it before their rename, so the final name is
	// what must be checked: the rename carries the owner, and a temp left behind is a leak.
	var objects int
	err := filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == s.dir {
			return err
		}
		if strings.HasPrefix(d.Name(), ".tmp-") {
			t.Errorf("a temp was left behind: %s", p)
		}
		if d.IsDir() && !owned[p] {
			t.Errorf("directory %s was not given the folder's owner", p)
		}
		if !d.IsDir() {
			objects++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if objects == 0 {
		t.Fatal("restic left nothing in the folder")
	}
	var temps int
	for p := range owned {
		if strings.HasPrefix(filepath.Base(p), ".tmp-") {
			temps++
		}
	}
	if temps < objects {
		t.Errorf("%d files written but only %d given the folder's owner before landing", objects, temps)
	}
}

// TestTheStoreAnswersOnlyTheGuest: any other source is refused before anything is read or written.
func TestTheStoreAnswersOnlyTheGuest(t *testing.T) {
	s, srv, _ := storeRig(t)
	s.client = "10.9.9.9"
	resp, err := http.Post(srv.URL+"/?create=true", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d from a stranger, want 403", resp.StatusCode)
	}
	if entries, _ := os.ReadDir(s.dir); len(entries) != 0 {
		t.Fatalf("a stranger's request wrote %v", entries)
	}
}

// TestTheStoreNeverCreatesTheFolder: a missing folder may be an unmounted disk; the store says so
// and writes nothing where it was.
func TestTheStoreNeverCreatesTheFolder(t *testing.T) {
	s, srv, _ := storeRig(t)
	s.dir = filepath.Join(s.dir, "unmounted")
	resp, err := http.Post(srv.URL+"/?create=true", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d for a missing folder, want 503", resp.StatusCode)
	}
	if _, err := os.Stat(s.dir); err == nil {
		t.Fatal("the store created the missing folder")
	}
}

// TestTheStoreRefusesNamesAndOverwrites: a name that is not a hash never becomes a path, and an
// object that exists is never replaced.
func TestTheStoreRefusesNamesAndOverwrites(t *testing.T) {
	_, srv, _ := storeRig(t)
	post := func(path, body string) int {
		resp, err := http.Post(srv.URL+path, "application/octet-stream", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("/?create=true", ""); c != http.StatusOK {
		t.Fatalf("create: %d", c)
	}
	for _, p := range []string{"/keys/..%2f..%2fetc", "/keys/ABC", "/keys/" + strings.Repeat("a", 63), "/other/" + strings.Repeat("a", 64)} {
		if c := post(p, "x"); c != http.StatusBadRequest && c != http.StatusNotFound {
			t.Errorf("POST %s: %d, want a refusal", p, c)
		}
	}
	name := "/snapshots/" + strings.Repeat("ab", 32)
	if c := post(name, "one"); c != http.StatusOK {
		t.Fatalf("first write: %d", c)
	}
	if c := post(name, "two"); c != http.StatusForbidden {
		t.Fatalf("overwrite: %d, want 403", c)
	}
	resp, err := http.Get(srv.URL + name)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got bytes.Buffer
	got.ReadFrom(resp.Body)
	if got.String() != "one" {
		t.Fatalf("object holds %q after a refused overwrite", got.String())
	}
}

// TestTheStoreListsInTheVersionAsked: v2 (what restic asks for) carries each object's size, so
// restic does not HEAD every file; v1 is the bare names.
func TestTheStoreListsInTheVersionAsked(t *testing.T) {
	_, srv, _ := storeRig(t)
	for _, p := range []string{"/?create=true", "/keys/" + strings.Repeat("cd", 32)} {
		resp, err := http.Post(srv.URL+p, "", bytes.NewBufferString("12345"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	get := func(accept string) (string, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/keys/", nil)
		req.Header.Set("Accept", accept)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return resp.Header.Get("Content-Type"), strings.TrimSpace(b.String())
	}
	if ct, body := get(resticV2); ct != resticV2 || body != `[{"name":"`+strings.Repeat("cd", 32)+`","size":5}]` {
		t.Errorf("v2: %s %s", ct, body)
	}
	if ct, body := get(""); ct != "application/vnd.x.restic.rest.v1" || body != `["`+strings.Repeat("cd", 32)+`"]` {
		t.Errorf("v1: %s %s", ct, body)
	}
}

// A repository the store creates carries a README.txt that says what the folder is and how to
// restore it without Briard; a household's own copy is never replaced.
func TestTheStoreLeavesAReadme(t *testing.T) {
	s, srv, owned := storeRig(t)
	resp, err := http.Post(srv.URL+"/?create=true", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	readme := filepath.Join(s.dir, "README.txt")
	b, err := os.ReadFile(readme)
	if err != nil || !strings.Contains(string(b), "restic -r") || !strings.Contains(string(b), "recovery key") {
		t.Fatalf("README.txt: %v\n%s", err, b)
	}
	// Written as every object is -- a temp beside it, given the owner, renamed -- so a temp in the
	// folder's own root is what was given the owner before it became the README.
	rootTemp := false
	for p := range owned {
		rootTemp = rootTemp || (filepath.Dir(p) == s.dir && strings.HasPrefix(filepath.Base(p), ".tmp-"))
	}
	if !rootTemp {
		t.Error("the README was not given the folder's owner")
	}
	if err := os.WriteFile(readme, []byte("ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, _ = http.Post(srv.URL+"/?create=true", "", nil)
	resp.Body.Close()
	if b, _ := os.ReadFile(readme); string(b) != "ours" {
		t.Fatal("the household's README was replaced")
	}
}

// Turned off, the store refuses everything and writes nothing.
func TestTheStoreAnswersNothingWhileTheBackupIsOff(t *testing.T) {
	s, srv, _ := storeRig(t)
	dir := s.dir
	s.folder = func() string { return "" }
	resp, err := http.Post(srv.URL+"/?create=true", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Said as OFF, not as a missing folder: the run's report carries this, and "the folder is not
	// there" would send the household looking for a disk.
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "turned off") {
		t.Fatalf("status %d %q while off, want 503 saying it is off", resp.StatusCode, body)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the store wrote %v while off", entries)
	}
}
