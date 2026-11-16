package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"briard.io/agent/drbd"
	"briard.io/agent/guestagent/deadman"
	"briard.io/agent/guestfirmware"
	"briard.io/agent/hass"
	"briard.io/agent/mosquitto"
	"briard.io/agent/quadlet"
	"briard.io/internal/testsock"
	"briard.io/shared/dashboard"
	"briard.io/shared/manifest"
	"briard.io/shared/nodestorage"
	"briard.io/shared/notify"
)

// fakeExec records writes/runs and returns canned output; stands in for the guest.
// runFn, if set, overrides output/err (e.g. to evolve Status across polls).
type fakeExec struct {
	files    map[string]string
	runs     [][]string
	hostname string
	output   []byte
	err      error
	writeErr error // WriteFile fails: a snapshot sidecar that cannot be written
	runFn    func(name string, args []string) ([]byte, error)
	// qgroups is the volume's quota tree, each id to its parents, which `btrfs qgroup` reads and
	// writes as the kernel does: create and assign refuse an id that exists. Every fake has one,
	// so no test has to stand in for the tree to reach the code it is about.
	qgroups map[string][]string
	qlimits map[string]string // a group's limit as `qgroup limit` set it; none when absent
	qused   map[string]int64  // a group's referenced bytes; 16384 when absent
	// dirs are directories as a test lays them out, each with entries the files map does not
	// hold (a subvolume, a member, a socket); a directory also exists when a file is under it, or
	// once `mkdir -p` made it. Stat and ListDir answer from these and the files map together.
	dirs   map[string][]string
	blocks map[string]bool     // block devices Stat reports as such
	addrs  map[string][]string // a NIC's global IPv4 addresses; a NIC absent here does not exist
}

func (f *fakeExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.runs = append(f.runs, append([]string{name}, args...))
	if name == "btrfs" && len(args) > 0 && args[0] == "qgroup" {
		return f.qgroup(args[1:])
	}
	if name == "mkdir" {
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				f.mkdir(a)
			}
		}
	}
	if f.runFn != nil {
		return f.runFn(name, args)
	}
	return f.output, f.err
}

func (f *fakeExec) qgroup(args []string) ([]byte, error) {
	if f.qgroups == nil {
		f.qgroups = map[string][]string{"0/5": nil}
	}
	exists := errors.New("exit status 1")
	switch {
	case len(args) == 3 && args[0] == "create":
		if _, ok := f.qgroups[args[1]]; ok {
			return []byte("ERROR: unable to create quota group: File exists"), exists
		}
		f.qgroups[args[1]] = nil
	case len(args) == 4 && args[0] == "assign":
		if slices.Contains(f.qgroups[args[1]], args[2]) {
			return []byte("ERROR: unable to assign quota group: File exists"), exists
		}
		f.qgroups[args[1]] = append(f.qgroups[args[1]], args[2])
	case len(args) == 4 && args[0] == "limit":
		if _, ok := f.qgroups[args[2]]; !ok {
			return []byte("ERROR: unable to limit requested quota group: No such file or directory"), exists
		}
		if f.qlimits == nil {
			f.qlimits = map[string]string{}
		}
		f.qlimits[args[2]] = args[1]
	case len(args) > 0 && args[0] == "show":
		out := "Qgroupid Referenced Exclusive  Max referenced Parent Path\n-------- ---------- --------- -------------- ------ ----\n"
		for id, parents := range f.qgroups {
			p := "-"
			if len(parents) > 0 {
				p = strings.Join(parents, ",")
			}
			used := int64(16384)
			if u, ok := f.qused[id]; ok {
				used = u
			}
			limit := "none"
			if l, ok := f.qlimits[id]; ok {
				limit = l
			}
			out += fmt.Sprintf("%s %d %d %s %s x\xff\n", id, used, used, limit, p)
		}
		return []byte(out), nil
	default:
		return nil, errors.New("the fake has no such btrfs qgroup call")
	}
	return nil, nil
}

func (f *fakeExec) WriteFile(path string, data []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = string(data)
	return nil
}

// A file this fake was never given reads back as os.ErrNotExist rather than as "": the
// difference between "no name is published" and "the published name is empty" is the whole
// reason the published name is read instead of assumed.
func (f *fakeExec) ReadFile(path string) ([]byte, error) {
	v, ok := f.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(v), nil
}

func (f *fakeExec) Sethostname(name string) error {
	f.hostname = name
	return nil
}

// mkdir records a directory as made, with no entries of its own.
func (f *fakeExec) mkdir(dir string) {
	dir = filepath.Clean(dir)
	if f.dirs == nil {
		f.dirs = map[string][]string{}
	}
	if _, ok := f.dirs[dir]; !ok {
		f.dirs[dir] = nil
	}
}

// isDir says whether dir exists as a directory: laid out, made, or holding a file or directory.
func (f *fakeExec) isDir(dir string) bool {
	dir = filepath.Clean(dir)
	if _, ok := f.dirs[dir]; ok {
		return true
	}
	for p := range f.files {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	for d := range f.dirs {
		if strings.HasPrefix(d, dir+"/") {
			return true
		}
	}
	return false
}

// fakeInfo is the FileInfo the fake's Stat hands back: only the name, the mode and the size
// carry anything.
type fakeInfo struct {
	name string
	mode fs.FileMode
	size int64
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return i.size }
func (i fakeInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }

func (f *fakeExec) Stat(path string) (fs.FileInfo, error) {
	path = filepath.Clean(path)
	name := filepath.Base(path)
	switch {
	case f.blocks[path]:
		return fakeInfo{name: name, mode: fs.ModeDevice}, nil
	case f.isDir(path):
		return fakeInfo{name: name, mode: fs.ModeDir | 0o755}, nil
	}
	if v, ok := f.files[path]; ok {
		return fakeInfo{name: name, mode: 0o644, size: int64(len(v))}, nil
	}
	// An entry a test laid out in its directory exists, as a directory unless the files map
	// holds it.
	if slices.Contains(f.dirs[filepath.Dir(path)], name) {
		return fakeInfo{name: name, mode: fs.ModeDir | 0o755}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
}

// ListDir lists what the real one would: the entries laid out, every file or directory below,
// by its first name under dir, sorted, without the dot-names.
func (f *fakeExec) ListDir(dir string) ([]string, error) {
	dir = filepath.Clean(dir)
	if !f.isDir(dir) {
		return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrNotExist}
	}
	seen := map[string]bool{}
	for _, e := range f.dirs[dir] {
		seen[e] = true
	}
	under := func(p string) {
		if rest, ok := strings.CutPrefix(p, dir+"/"); ok && rest != "" {
			seen[strings.SplitN(rest, "/", 2)[0]] = true
		}
	}
	for p := range f.files {
		under(p)
	}
	for d := range f.dirs {
		under(d)
	}
	var names []string
	for n := range seen {
		if !strings.HasPrefix(n, ".") {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names, nil
}

func (f *fakeExec) IPv4Addrs(dev string) ([]string, error) {
	held, ok := f.addrs[dev]
	if !ok {
		return nil, fmt.Errorf("no such network interface %s", dev)
	}
	return held, nil
}

// mountinfo is /proc/self/mountinfo with a filesystem mounted at each of paths.
func mountinfo(paths ...string) string {
	var b strings.Builder
	for i, p := range paths {
		fmt.Fprintf(&b, "%d 1 0:%d / %s rw,relatime shared:1 - btrfs /dev/vdb rw\n", 30+i, 40+i, p)
	}
	return b.String()
}

// existingMetadata is a runFn simulating a data disk that already holds a DRBD replica (a
// rebooted node): `create-md` (no --force) refuses to overwrite -> non-zero. Everything else
// succeeds.
func existingMetadata(name string, args []string) ([]byte, error) {
	if name == "drbdadm" && len(args) > 0 && args[0] == "create-md" {
		return []byte("v09 meta data already in place\n[need to type 'yes' to confirm]"), errors.New("exit status 20")
	}
	return nil, nil
}

// dial wires a host Client to a guest Serve(fake) over an in-memory pipe.
func dial(t *testing.T, x Executor) *Client {
	t.Helper()
	cconn, sconn := net.Pipe()
	go Serve(context.Background(), sconn, x)
	g := NewClient(cconn)
	t.Cleanup(func() { g.Close() })
	return g
}

// The storage verb's own job is small and exact: land the spec where the unit will read it, and
// start that unit. The block work is the unit's, and nodestorage_test.go tests it directly.
func TestNodeStorageVerbLandsTheSpecAndStartsTheUnit(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	spec := nodestorage.Spec{
		Tiers: []nodestorage.Tier{{
			Name: nodestorage.TierData, Device: "/dev/vdb",
			VG: "briardvol", LV: "data", MetaLV: "metadata", Mode: nodestorage.ModeAdiantum,
		}},
		Resource: nodestorage.Resource{Name: "r0", Device: "/dev/drbd0", Replicated: true, Config: "RES", FreshInit: true, MaxPeers: 4},
	}
	if err := g.NodeStorage(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	landed, err := nodestorage.Parse([]byte(f.files[nodestorage.Path]))
	if err != nil {
		t.Fatalf("what landed does not parse: %v", err)
	}
	if tier, ok := landed.Tier(nodestorage.TierData); !ok || tier.Mode != nodestorage.ModeAdiantum {
		t.Errorf("the host's encryption policy did not survive the wire: %+v", landed)
	}
	want := [][]string{{"systemctl", "start", nodeStorageUnit}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

// A spec the guest cannot understand is refused at the verb, with the host still listening,
// rather than inside a unit whose only report is an exit code. Unknown fields included: the host
// and the pushed agent ship together, so a field one side does not know means they have drifted.
func TestNodeStorageVerbRefusesADriftedSpec(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.c.Call(context.Background(), verbNodeStorage, map[string]any{
		"tiers": []map[string]any{{
			"name": "data", "device": "/dev/vdb", "vg": "briardvol", "lv": "data", "metaLV": "metadata",
			"mode": "auto", "stripes": 4,
		}},
		"resource": map[string]any{"name": "r0", "device": "/dev/drbd0", "config": "RES", "maxPeers": 4},
	}, nil); err == nil {
		t.Fatal("a spec with an unknown field was accepted; a second dm layer would arrive unnoticed")
	}
	if len(f.runs) != 0 {
		t.Errorf("the unit was started anyway: %v", f.runs)
	}
	if _, ok := f.files[nodestorage.Path]; ok {
		t.Error("a refused spec was still written to disk")
	}
}

// Adjust (runtime mesh growth) rewrites the .res with the new peer set and runs
// `drbdadm adjust` -- and crucially never create-md's: the serving primary's disk is never
// touched when a second anchor joins, so its UpToDate replica can't be wiped or re-seeded.
func TestAdjustRewritesConfigAndAdjustsNeverCreatesMD(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	req := ProvisionRequest{Resource: "r0", ResConfig: "THREE-PEER-RES", ReactorConfig: "REACTOR"}
	if err := g.Adjust(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if f.files["/run/briard/drbd.d/r0.res"] != "THREE-PEER-RES" {
		t.Errorf(".res = %q, want the rewritten peer set", f.files["/run/briard/drbd.d/r0.res"])
	}
	if f.files["/run/briard/drbd-reactor.d/briard.toml"] != "REACTOR" {
		t.Errorf("reactor toml = %q", f.files["/run/briard/drbd-reactor.d/briard.toml"])
	}
	// Exactly `drbdadm adjust r0` -- no create-md (the load-bearing negative: adjust must not be
	// able to wipe the primary's replica the way a stray create-md --force could).
	want := [][]string{{"drbdadm", "adjust", "r0"}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v (adjust only, never create-md)", f.runs, want)
	}
}

// A single node has no DRBD address (a lone node runs no DRBD) -- ConfigureNet with an
// empty dev/CIDR must skip addressing entirely and only record the VIP device (the unified layout:
// eth1 is the idle DRBD NIC, the VIP lives on eth2). Failable: any `ip` call would be wrong.
func TestConfigureNetVIPOnlySkipsAddressing(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{Dev: "", CIDR: "", VIPDev: "eth2", VIPAddr: ""}); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 0 {
		t.Errorf("single-node VIP-only config must run no ip commands, got %v", f.runs)
	}
	if f.files[vipEnvPath] != "VIP_DEV=eth2\n" {
		t.Errorf("vip.env = %q, want VIP_DEV=eth2", f.files[vipEnvPath])
	}
}

// ConfigureNet addresses the system NIC (eth1): an idempotent `addr replace`, a read of what the
// NIC now holds, then a link up -- all with an explicit `dev`. With no vipDev it writes
// no VIP file (the guest keeps its baked default). The NIC here holds only the wanted address, so
// the read-back prunes nothing; TestConfigureNetPrunesStaleAddress is the failable half.
func TestConfigureNet(t *testing.T) {
	f := &fakeExec{addrs: map[string][]string{"eth1": {"10.0.0.2/24"}}}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{Dev: "eth1", CIDR: "10.0.0.2/24", VIPDev: "", VIPAddr: ""}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"ip", "addr", "replace", "10.0.0.2/24", "dev", "eth1"},
		{"ip", "link", "set", "dev", "eth1", "up"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
	if _, ok := f.files[vipEnvPath]; ok {
		t.Errorf("no vipDev -> must not write %s, got %q", vipEnvPath, f.files[vipEnvPath])
	}
}

// Renumbering: the NIC holds an old address (the node's island subnet) and ConfigureNet is called
// with the new one (the adopter's: identity is flock-scoped). `ip addr replace` alone would leave
// both, so the stale one must be deleted, and the new one must be on the NIC before it goes.
// Failable by construction: with the prune removed this test sees two commands instead of three
// and no `addr del`. It is the only place that catches it: on a node that never had an address,
// add-without-remove and add-with-remove are the same thing, and the rest of the suite agrees
// with both.
func TestConfigureNetPrunesStaleAddress(t *testing.T) {
	f := &fakeExec{addrs: map[string][]string{"eth1": {"10.0.0.2/24", "10.9.1.7/24"}}}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{Dev: "eth1", CIDR: "10.0.0.2/24", VIPDev: "", VIPAddr: ""}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"ip", "addr", "replace", "10.0.0.2/24", "dev", "eth1"},
		{"ip", "addr", "del", "10.9.1.7/24", "dev", "eth1"}, // the island address, gone
		{"ip", "link", "set", "dev", "eth1", "up"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v,\nwant %v", f.runs, want)
	}
}

// AppHealth is the in-guest readiness probe (macvtap-safe): the guest GETs the app's
// health URL itself and reports 200. Exercises the real raw-net.Dial HTTP/1.0 path end to end
// (Client -> dispatch -> probeHTTPOK), since it deliberately avoids net/http in the guest binary.
func TestAppHealth(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer ok.Close()
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer sick.Close()

	f := &fakeExec{runFn: func(string, []string) ([]byte, error) { return nil, nil }} // health path doesn't shell out
	if got, err := dial(t, f).AppHealth(context.Background(), ok.URL); err != nil || !got {
		t.Errorf("200 payload: got (%v,%v), want (true,nil)", got, err)
	}
	if got, err := dial(t, f).AppHealth(context.Background(), sick.URL); err != nil || got {
		t.Errorf("503 payload: got (%v,%v), want (false,nil)", got, err)
	}
	// Unreachable endpoint -> not healthy, still no transport error (the probe swallows dial errors).
	if got, err := dial(t, f).AppHealth(context.Background(), "http://127.0.0.1:1/healthz"); err != nil || got {
		t.Errorf("unreachable: got (%v,%v), want (false,nil)", got, err)
	}
	// An app that takes the connection and does not answer -> not healthy, and inside the
	// five seconds the host's status cycle gives its calls, with no deadline from the caller:
	// a verdict, never the silence the host counts toward a dead guest.
	held := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-held }))
	defer slow.Close()
	defer close(held)
	start := time.Now()
	if got, err := dial(t, f).AppHealth(context.Background(), slow.URL); err != nil || got {
		t.Errorf("an app that never answers: got (%v,%v), want (false,nil)", got, err)
	}
	if took := time.Since(start); took >= 4*time.Second {
		t.Errorf("an app that never answers took %s to read unhealthy, want well inside the host's 5s", took)
	}
}

// The handshake records the advertised capability set, which is the whole of the negotiation:
// what the host checks with Supports, one path at a time.
func TestHandshake(t *testing.T) {
	g := dial(t, &fakeExec{})
	h, err := g.Handshake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Capabilities) != len(guestCapabilities) {
		t.Errorf("capabilities = %d, want the guest agent's %d", len(h.Capabilities), len(guestCapabilities))
	}
	if !g.Supports(verbOSSystem) {
		t.Error("guest should advertise os.system after the handshake")
	}
	if g.Supports("bogus.verb") {
		t.Error("guest must not advertise a bogus verb")
	}
}

// The successor of an agent that died mid-call re-attaches to the same channel with the dead
// session's reply still in it, and guestfirmware's resync separates the two sessions by id, so
// the frame that decides it is the one every session has in common: the reply to its first
// request, the hello. Two real sessions over one buffered stream, which is what QEMU's chardev is
// (the guest port stays open, so bytes outlive the host process that was going to read them).
func TestHandshakeResyncsPastADeadSessionsHelloReply(t *testing.T) {
	host, guest := socketPair(t)
	taken := make(chan struct{}, 2) // buffered: session 2's signal is never read
	go guestfirmware.ServeFrames(context.Background(), guest, func(_ context.Context, verb string, _ json.RawMessage) (any, error) {
		if verb != guestfirmware.VerbHello {
			return nil, nil
		}
		taken <- struct{}{} // the request is read; its reply is written next
		return guestfirmware.Hello{Capabilities: []string{guestfirmware.VerbHello, verbReactor}}, nil
	}, isAct)

	// Session 1 dies with its hello on the wire: the guest answers into a stream nobody
	// is reading. Driven through a real Client, because which ids a real session picks is
	// the whole question.
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	go NewClient(deadReader{Writer: host, blocked: stuck}).Handshake(context.Background())
	<-taken

	// Session 2 re-adopts the same channel.
	g := NewClient(host)
	t.Cleanup(func() { g.Close() })
	if _, err := g.Handshake(context.Background()); err != nil {
		t.Fatalf("re-adopted handshake: %v", err)
	}
	// The verb after the handshake is where a wrong match surfaces: a handshake that matched
	// the orphan leaves the real reply for this call to trip over.
	if err := g.ReactorStart(context.Background(), "r0", ""); err != nil {
		t.Fatalf("first verb after a re-adopted handshake: %v", err)
	}
}

// deadReader is the host end of a session whose process was killed with a request on the
// wire: writes reach the guest, reads never return. Close is a no-op -- the channel
// outlives the process, which is the premise of the whole resync.
type deadReader struct {
	io.Writer
	blocked chan struct{}
}

func (d deadReader) Read([]byte) (int, error) { <-d.blocked; return 0, io.EOF }
func (d deadReader) Close() error             { return nil }

// socketPair connects two ends over a real unix socket. Buffered, unlike net.Pipe: a
// reply nobody has read yet must not block the guest's next read, which is exactly what
// the orphaned-reply case needs (and what QEMU's chardev does).
func socketPair(t *testing.T) (host, guest net.Conn) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(testsock.Dir(t), "pair.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, aerr := ln.Accept(); aerr == nil {
			accepted <- c
		} else {
			close(accepted)
		}
	}()
	if host, err = net.Dial("unix", ln.Addr().String()); err != nil {
		t.Fatalf("dial: %v", err)
	}
	guest = <-accepted
	if guest == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { host.Close(); guest.Close() })
	return host, guest
}

// Before a handshake, Supports is optimistic (true) -- preserving the older "just try
// the verb" behaviour for callers that don't negotiate.
func TestSupportsBeforeHandshakeIsOptimistic(t *testing.T) {
	if !(&Client{}).Supports("anything") {
		t.Error("pre-handshake Supports must return true")
	}
}

// A handshake refuses nothing. A guest advertising a verb set the host has never heard
// of -- an older bundle a revert pinned, a firmware from another image -- is still driven, and
// only the paths that need what it does not serve step aside. The channel is what fixes a node,
// so closing it is never the safe answer.
func TestHandshakeAcceptsAnUnrecognisedGuest(t *testing.T) {
	host, guest := socketPair(t)
	go guestfirmware.ServeFrames(context.Background(), guest, func(context.Context, string, json.RawMessage) (any, error) {
		return guestfirmware.Hello{Capabilities: []string{guestfirmware.VerbHello, "verb.from.the.future"}}, nil
	}, isAct)
	g := NewClient(host)
	t.Cleanup(func() { g.Close() })
	if _, err := g.Handshake(context.Background()); err != nil {
		t.Fatalf("a guest serving verbs this host does not know must still be driven: %v", err)
	}
	if !g.Supports("verb.from.the.future") {
		t.Error("the advertised set must be recorded verbatim")
	}
	if g.Supports(verbDataMember) {
		t.Error("a verb this guest did not advertise must be refused by Supports, one path at a time")
	}
}

// WriteCert lands a renewed cert/key on the DRBD volume (key first, then cert, then
// flush) -- so a torn write never leaves a mismatched pair the terminator would serve.
func TestWriteCert(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.WriteCert(context.Background(), "CERTPEM\n", "KEYPEM\n"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"mkdir", "-p", "/var/lib/briard/tls"},
		{"sync", "-f", "/var/lib/briard/tls/fullchain.pem"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
	if f.files["/var/lib/briard/tls/key.pem"] != "KEYPEM\n" || f.files["/var/lib/briard/tls/fullchain.pem"] != "CERTPEM\n" {
		t.Errorf("cert/key not written: %v", f.files)
	}
}

// SetHostname renames the guest so DRBD's `on <name>` matching works.
func TestSetHostname(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.SetHostname(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	if f.hostname != "n1" {
		t.Errorf("hostname = %q, want n1", f.hostname)
	}
	// And it persists nothing, the same rule read the other way round. syscall.Sethostname does
	// not survive a guest reboot, so the hostname and the `.res` naming this node must share one
	// lifetime: a durable `.res` beside an ephemeral name lets a rebooted guest run as "guest"
	// against its own `on n1`, and drbd-reactor promotes into the mismatch at boot, parking the
	// node quorate-but-never-Primary with no VIP and no address. The `.res` is ephemeral, so both
	// are re-derived from the host at every bring-up and nothing can promote before it. Asserting
	// the absence of a persisted name (a node-id file) stops one arriving as a well-meant
	// restore-at-boot: a third copy on disk is a third thing that can be wrong.
	for path := range f.files {
		if strings.Contains(path, "node-id") {
			t.Errorf("sys.hostname wrote %s -- the name is node-scoped and re-pushed at every "+
				"bring-up, so a persisted copy can only ever be a stale one", path)
		}
	}
}

// A data node's ConfigureNet also records the agent-determined VIP device, where
// briard-vip.service reads it -- the two-subnet layout moves the VIP to eth2.
func TestConfigureNetWritesVIPDev(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{Dev: "eth1", CIDR: "10.0.0.2/24", VIPDev: "eth2", VIPAddr: ""}); err != nil {
		t.Fatal(err)
	}
	// SYSTEM_DEV rides along for the VIP unit's stop path: a standby must bring a dedicated
	// service NIC down (the flock MAC on it is shared with the peer, and a Secondary emitting from it
	// teaches the switch the wrong port), while a link-down on the DRBD NIC would take replication
	// with it. The guest cannot tell those apart from VIP_DEV alone, and it may not guess.
	if got := f.files[vipEnvPath]; got != "VIP_DEV=eth2\nSYSTEM_DEV=eth1\n" {
		t.Errorf("%s = %q, want VIP_DEV=eth2 + SYSTEM_DEV=eth1", vipEnvPath, got)
	}
}

// The flock MAC rides beside the device as VIP_MAC: the VIP's DHCP identity is the flock's even
// where the NIC's own MAC is not (on a wireless parent it is the station's).
func TestConfigureNetWritesVIPMAC(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{Dev: "eth1", CIDR: "10.0.0.2/24", VIPDev: "eth2", VIPMAC: "52:54:00:ab:cd:ef"}); err != nil {
		t.Fatal(err)
	}
	if got := f.files[vipEnvPath]; got != "VIP_DEV=eth2\nSYSTEM_DEV=eth1\nVIP_MAC=52:54:00:ab:cd:ef\n" {
		t.Errorf("%s = %q, want VIP_MAC after SYSTEM_DEV", vipEnvPath, got)
	}
}

// A witness claims no VIP, so it gets no file at all -- SYSTEM_DEV must not create one on its own.
// briard-vip.service takes vip.env as a required EnvironmentFile, so a file written for a node
// whose promoter must never run is a unit that has quietly become startable.
func TestConfigureNetWritesNoVIPEnvForAWitness(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{Dev: "eth1", CIDR: "10.0.0.3/24"}); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.files[vipEnvPath]; ok {
		t.Errorf("a witness got %s = %q; it claims no VIP and must get no file", vipEnvPath, got)
	}
}

// The VIP address is the LAN's, not ours: baking it would make the product work only on one
// subnet and fail green everywhere else. ConfigureNet must record it beside the device, not
// apply it: the promoter chain claims the address on promotion, and a guest that configured it
// here would hold the VIP while Secondary.
func TestConfigureNetWritesVIPAddr(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{Dev: "", CIDR: "", VIPDev: "eth2", VIPAddr: "192.168.9.50/24"}); err != nil {
		t.Fatal(err)
	}
	if got := f.files[vipEnvPath]; got != "VIP_DEV=eth2\nVIP_ADDR=192.168.9.50/24\n" {
		t.Errorf("%s = %q, want VIP_DEV then VIP_ADDR", vipEnvPath, got)
	}
	if len(f.runs) != 0 {
		t.Errorf("the VIP address must be recorded, not claimed here; ran %v", f.runs)
	}
}

// An unset address omits the line rather than writing an empty one: unset is the ordinary case,
// where briard-vip-up resolves the address from the replicated store or, failing that, DHCP.
func TestConfigureNetOmitsUnsetVIPAddr(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{Dev: "", CIDR: "", VIPDev: "eth2", VIPAddr: ""}); err != nil {
		t.Fatal(err)
	}
	if got := f.files[vipEnvPath]; strings.Contains(got, "VIP_ADDR") {
		t.Errorf("%s = %q, must not mention VIP_ADDR when unset", vipEnvPath, got)
	}
}

// The flock's visible name goes to its own file, which the reverse proxy re-reads to republish
// it, and touches nothing about addressing: the whole point of the three-way identifier split.
// A name is a label, an address is an identity, so a rename must never be an addressing call.
func TestSetMDNSNameWritesNameAndRepublishes(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.SetMDNSName(context.Background(), "brave-elf"); err != nil {
		t.Fatal(err)
	}
	if got := f.files[mdnsEnvPath]; got != "FLOCK_NAME=brave-elf\n" {
		t.Errorf("%s = %q, want FLOCK_NAME=brave-elf", mdnsEnvPath, got)
	}
	// A rename runs nothing. The reverse proxy re-reads this file on its own tick, so shelling
	// out to make the rename take effect would be a second way to do it -- and the one that can
	// fire on a Secondary, which runs no reverse proxy and holds no address to publish a name for.
	if len(f.runs) != 0 {
		t.Errorf("a rename ran %v; renaming writes two files and does nothing else", f.runs)
	}
	// The VIP's own file must be untouched -- a rename that rewrote it would put every rename
	// through the addressing path this design exists to keep it out of.
	if _, touched := f.files[vipEnvPath]; touched {
		t.Errorf("a rename wrote %s; renaming must not touch addressing", vipEnvPath)
	}
}

// An empty name publishes nothing rather than `briard-.local`. Every agent-less harness and every
// node with no minted flock name sends "", and a guess is worse than silence.
func TestSetMDNSNameIgnoresEmpty(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.SetMDNSName(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.files[mdnsEnvPath]; ok {
		t.Errorf("an empty name wrote %s = %q", mdnsEnvPath, f.files[mdnsEnvPath])
	}
	if len(f.runs) != 0 {
		t.Errorf("an empty name restarted something: %v", f.runs)
	}
}

// The assertion this feature exists for: the name we asked for is not evidence of the name in
// force, so MDNSPublished must report what the reverse proxy records it is answering for (here
// `brave-elf-2`, while brave-elf was asked for), never echo the request. Echoing it would rebuild
// the baked-name defect exactly: a name present, plausible, and not what anyone thinks it is.
func TestMDNSPublishedReportsTheRenamedName(t *testing.T) {
	f := &fakeExec{files: map[string]string{mdnsPublishedPath: "brave-elf-2\n"}}
	g := dial(t, f)
	if err := g.SetMDNSName(context.Background(), "brave-elf"); err != nil {
		t.Fatal(err)
	}
	got, err := g.MDNSPublished(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "brave-elf-2" {
		t.Errorf("MDNSPublished = %q, want the ESTABLISHED name brave-elf-2 (we asked for brave-elf)", got)
	}
}

// A node publishing nothing -- a Secondary, a witness -- answers "" rather than erroring. The
// host asks whenever its report or doctor is read, and the common case must not read as a fault.
func TestMDNSPublishedIsEmptyWhenNothingIsPublished(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	got, err := g.MDNSPublished(context.Background())
	if err != nil {
		t.Fatalf("a missing published-name file must be %q, not an error: %v", "", err)
	}
	if got != "" {
		t.Errorf("MDNSPublished = %q, want empty", got)
	}
}

// Arming the promoter lands the snippet and starts the daemon, in that order: a drbd-reactor that
// started before its snippet existed would promote with no chain at all.
func TestReactorStartLandsTheSnippet(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.ReactorStart(context.Background(), "r0", "PROMOTER"); err != nil {
		t.Fatal(err)
	}
	if f.files[reactorPath] != "PROMOTER" {
		t.Errorf("promoter snippet = %q", f.files[reactorPath])
	}
	want := [][]string{{"systemctl", "start", "drbd-reactor.service"}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

// The full stack: Client.Status -> channel -> Handler -> drbdsetup -> ParseStatus.
func TestStatusParsesQuorumState(t *testing.T) {
	f := &fakeExec{output: []byte(`[{"name":"r0","role":"Primary",` +
		`"devices":[{"quorum":true}],"connections":[{"connection-state":"Connected"}]}]`)}
	g := dial(t, f)
	qs, err := g.Status(context.Background(), "r0")
	if err != nil {
		t.Fatal(err)
	}
	if !qs.Primary || !qs.Quorate || qs.Connected != 1 {
		t.Errorf("QuorumState = %+v; want Primary=true Quorate=true Connected=1", qs)
	}
}

func TestExecErrorPropagatesToHost(t *testing.T) {
	// A failed guest command surfaces to the host wrapped with the command and its
	// output, so a bring-up failure is diagnosable (not a bare "exit status 1").
	f := &fakeExec{output: []byte("no such target\n"), err: errors.New("exit status 1")}
	g := dial(t, f)
	err := g.ReactorStart(context.Background(), "r0", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"systemctl start drbd-reactor.service", "exit status 1", "no such target"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, must contain %q", err, want)
		}
	}
}

// demoResource is the 2-disk + diskless-witness resource (the product topology).
func demoResource() drbd.Resource {
	return drbd.Resource{
		Name: "r0", Device: "/dev/drbd0",
		Peers: []drbd.Peer{
			{Name: "node1", NodeID: 0, Address: "10.0.0.1:7789", Disk: "/dev/vdb"},
			{Name: "node2", NodeID: 1, Address: "10.0.0.2:7789", Disk: "/dev/vdb"},
			{Name: "witness", NodeID: 2, Address: "10.0.0.3:7789"},
		},
	}
}

// demoStorage is the host-rendered spec BringUp carries: one data tier and the resource on
// top of it. What the guest does with it is briard-node-storage.service's business and is tested
// against the real steps in nodestorage_test.go; here the question is what crosses the channel.
func demoStorage(fresh bool) nodestorage.Spec {
	return nodestorage.Spec{
		Tiers: []nodestorage.Tier{{
			Name: nodestorage.TierData, Device: "/dev/vdb",
			VG: "briardvol", LV: "data", MetaLV: "metadata", Mode: nodestorage.ModeAuto,
		}},
		Resource: nodestorage.Resource{
			Name: "r0", Device: "/dev/drbd0", Replicated: true, Config: demoResource().Config(), FreshInit: fresh, MaxPeers: 4,
		},
	}
}

// A data node's bring-up is two acts, and the order between them is the guarantee:
// every block-level step through one unit, and only then the promoter armed -- so nothing can
// promote before the volume it would mount exists.
func TestBringUpDataNode(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	spec := BringUpSpec{
		Storage:  demoStorage(true),
		Promoter: []string{"briard-primary-storage.service", "briard-vip.service", "briard-apps.service"},
	}
	if err := g.BringUp(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	// The spec lands verbatim, because the guest re-parses it: a document the two halves
	// disagree about is the one thing this verb must not carry.
	landed, err := nodestorage.Parse([]byte(f.files[nodestorage.Path]))
	if err != nil {
		t.Fatalf("the spec that landed does not parse: %v", err)
	}
	if landed.Resource.Config != demoResource().Config() || !landed.Resource.FreshInit {
		t.Errorf("the spec that landed is not the one the host rendered: %+v", landed)
	}
	if f.files[reactorPath] == "" {
		t.Error("a data node got no promoter snippet")
	}
	want := [][]string{
		{"systemctl", "start", nodeStorageUnit},
		{"systemctl", "start", "drbd-reactor.service"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("bring-up sequence = %v, want %v", f.runs, want)
	}
}

// A witness's bring-up: the same storage call (its spec builds no tier) and no promoter -- the
// load-bearing negative, since a diskless node that could promote would serve from nothing.
func TestBringUpWitness(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	spec := BringUpSpec{Storage: nodestorage.Spec{
		Resource: nodestorage.Resource{Name: "r0", Device: "/dev/drbd0", Replicated: true, Config: demoResource().Config(), Diskless: true},
	}}
	if err := g.BringUp(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.files[reactorPath]; ok {
		t.Error("witness should not get a reactor config")
	}
	want := [][]string{{"systemctl", "start", nodeStorageUnit}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("witness bring-up = %v, want %v (no reactor)", f.runs, want)
	}
}

// A spec the host could not build never reaches the guest: the client refuses it before the
// call, so a bad document cannot be half carried out by a unit whose only report is an exit code.
func TestBringUpRefusesAnInvalidSpec(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	bad := demoStorage(true)
	bad.Tiers[0].Mode = ""
	if err := g.BringUp(context.Background(), BringUpSpec{Storage: bad}); err == nil {
		t.Fatal("an invalid storage spec was sent to the guest")
	}
	if len(f.runs) != 0 {
		t.Errorf("the guest was asked to do %v anyway", f.runs)
	}
}

func TestAppStartStopCommands(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.AppStart(context.Background(), "briard-dummy-app.service"); err != nil {
		t.Fatal(err)
	}
	if err := g.AppStop(context.Background(), "briard-dummy-app.service"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"systemctl", "start", "briard-dummy-app.service"},
		{"systemctl", "--job-mode=ignore-dependencies", "stop", "briard-dummy-app.service"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

// Is-active prints the state word and exits non-zero when inactive; AppActive
// trusts the word, not the exit code.
func TestAppActiveReadsState(t *testing.T) {
	for _, tc := range []struct {
		out  string
		err  error
		want bool
	}{
		{"active\n", nil, true},
		{"inactive\n", errors.New("exit status 3"), false},
		{"failed\n", errors.New("exit status 3"), false},
	} {
		f := &fakeExec{output: []byte(tc.out), err: tc.err}
		g := dial(t, f)
		active, err := g.AppActive(context.Background(), "briard-dummy-app.service")
		if err != nil {
			t.Fatalf("AppActive(%q): %v", tc.out, err)
		}
		if active != tc.want {
			t.Errorf("AppActive(%q) = %v, want %v", tc.out, active, tc.want)
		}
	}
}

// absentDest makes `btrfs subvolume show` fail, which is what "nothing is at the destination"
// looks like -- the normal case for a ring member, whose name carries a timestamp.
func absentDest() *fakeExec {
	return &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if len(args) > 1 && args[1] == "show" {
			return nil, errors.New("ERROR: not a subvolume")
		}
		return nil, nil
	}}
}

func TestDataSnapshotCommand(t *testing.T) {
	f := absentDest()
	g := dial(t, f)
	if err := g.Snapshot(context.Background(), "/var/lib/briard/data/ha", "/var/lib/briard/data/.snapshots/ha-upgrade-1", `{"title":"x"}`); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"btrfs", "subvolume", "show", "/var/lib/briard/data/.snapshots/ha-upgrade-1"},
		{"btrfs", "subvolume", "snapshot", "-r", "-i", appQgroup(tierData, "ha"), "/var/lib/briard/data/ha", "/var/lib/briard/data/.snapshots/ha-upgrade-1"},
	}
	if !reflect.DeepEqual(withoutQgroups(f.runs), want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
	if got := f.files["/var/lib/briard/data/.snapshots/ha-upgrade-1.json"]; got != `{"title":"x"}` {
		t.Errorf("sidecar = %q, want the host's bytes written beside the member", got)
	}
}

// TestDataSnapshotRefusesAnExistingMember: a collision is refused, never cleared by a delete.
// `btrfs subvolume snapshot` given an existing directory writes inside it, which on a read-only
// member fails with "Read-only file system". Members are a series, so nothing legitimately
// supersedes anything and a collision means two landed in the same second; deleting there would
// destroy history inside the verb whose job is keeping it, so it refuses and the earlier member
// survives.
func TestDataSnapshotRefusesAnExistingMember(t *testing.T) {
	f := &fakeExec{} // `show` succeeds => the destination already holds a subvolume
	g := dial(t, f)
	err := g.Snapshot(context.Background(), "/var/lib/briard/data/ha", "/var/lib/briard/data/.snapshots/ha-upgrade-1", `{"title":"x"}`)
	if err == nil {
		t.Fatal("a collision was accepted; the earlier member would have been destroyed")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want it to name the collision", err)
	}
	for _, r := range f.runs {
		if len(r) > 2 && r[2] == "delete" {
			t.Fatalf("a ring member was deleted to make room: %v", f.runs)
		}
	}
}

// TestDataSnapshotDropsAMemberItCannotLabel: the sidecar cannot live inside a read-only subvolume,
// so it cannot be atomic with it. An unlabelled member is worse than no member -- the picker
// cannot show it and a restore cannot know what code wrote it -- so the half-made one is undone,
// and "every member has a sidecar" stays an invariant rather than a hope.
func TestDataSnapshotDropsAMemberItCannotLabel(t *testing.T) {
	f := absentDest()
	f.writeErr = errors.New("no space left on device")
	g := dial(t, f)
	err := g.Snapshot(context.Background(), "/var/lib/briard/data/ha", "/var/lib/briard/data/.snapshots/ha-upgrade-1", `{"title":"x"}`)
	if err == nil {
		t.Fatal("an unlabelled member was accepted")
	}
	var deleted bool
	for _, r := range f.runs {
		if len(r) > 3 && r[2] == "delete" && r[3] == "/var/lib/briard/data/.snapshots/ha-upgrade-1" {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("the unlabelled member was left behind: %v", f.runs)
	}
}

// TestSidecarPathMatchesTheRenderer: the guest derives the sidecar's name from the member's, and
// so does the host that renders its bytes. Two definitions, one convention -- asserted rather than
// assumed, because a drift here leaves every member silently unlabelled.
func TestSidecarPathMatchesTheRenderer(t *testing.T) {
	member := quadlet.SnapshotMember("ha", quadlet.TriggerAppUpdateBefore, time.Unix(0, 0))
	if got, want := sidecarPath(member), quadlet.SnapshotSidecar(member); got != want {
		t.Errorf("sidecarPath = %q, quadlet.SnapshotSidecar = %q", got, want)
	}
}

// The order is the safety property: verify the restore point, materialise the
// replacement beside the live subvolume, and only then destroy the live one. Asserted as a
// sequence because any reordering is the bug -- deleting first would leave the household with
// nothing at all whenever the restore point was missing or unreadable.
func TestDataRestoreCommands(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.Restore(context.Background(), "/var/lib/briard/data/ha", "/var/lib/briard/data/.snapshots/ha-1", nil); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"btrfs", "subvolume", "show", "/var/lib/briard/data/.snapshots/ha-1"},
		{"btrfs", "subvolume", "show", "/var/lib/briard/cache/ha"}, // no cache: nothing to empty
		{"btrfs", "subvolume", "show", "/var/lib/briard/data/ha.restoring"},
		{"btrfs", "subvolume", "delete", "/var/lib/briard/data/ha.restoring"},
		{"btrfs", "subvolume", "snapshot", "-i", appQgroup(tierData, "ha"), "/var/lib/briard/data/.snapshots/ha-1", "/var/lib/briard/data/ha.restoring"},
		{"btrfs", "subvolume", "delete", "/var/lib/briard/data/ha"},
		{"mv", "/var/lib/briard/data/ha.restoring", "/var/lib/briard/data/ha"},
	}
	if !reflect.DeepEqual(withoutQgroups(f.runs), want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

// Every rollback empties the cache, before the data is touched: the cache's subvolume is
// deleted and made again in the app's data group, holding the directories it held, so the
// containers' mounts find them. A restore point that is not there costs the cache nothing.
func TestARollbackEmptiesTheCacheFirst(t *testing.T) {
	cache := "/var/lib/briard/cache/ha"
	gone := false
	f := &fakeExec{}
	f.dirs = map[string][]string{cache: {"thumbs", "models"}}
	f.runFn = func(name string, args []string) ([]byte, error) {
		switch {
		case name == "btrfs" && args[1] == "show" && args[2] == cache && gone:
			return nil, errors.New("not a subvolume")
		case name == "btrfs" && args[1] == "show" && args[2] == cache:
			return []byte(cache), nil
		case name == "btrfs" && args[1] == "show" && strings.HasSuffix(args[2], ".restoring"):
			return nil, errors.New("not a subvolume")
		case name == "btrfs" && args[1] == "show" && args[2] == "/var/lib/briard/data/.snapshots/ha-gone":
			return nil, errors.New("not a subvolume")
		case name == "btrfs" && args[1] == "show":
			return []byte(args[2]), nil
		case name == "btrfs" && args[1] == "delete" && args[2] == cache:
			gone = true
		case name == "btrfs" && args[1] == "create" && args[len(args)-1] == cache:
			gone = false
		}
		return nil, nil
	}
	g := dial(t, f)
	if err := g.Restore(context.Background(), "/var/lib/briard/data/ha", "/var/lib/briard/data/.snapshots/ha-gone", nil); err == nil {
		t.Fatal("a restore from a missing point succeeded")
	}
	if ran(f, "btrfs", "subvolume", "delete", cache) {
		t.Fatalf("a restore that could not start emptied the cache: %v", f.runs)
	}
	if err := g.Restore(context.Background(), "/var/lib/briard/data/ha", "/var/lib/briard/data/.snapshots/ha-1", nil); err != nil {
		t.Fatal(err)
	}
	del := runIndex(f, 0, "btrfs", "subvolume", "delete", cache)
	made := runIndex(f, 0, "btrfs", "subvolume", "create", "-i", appQgroup(tierData, "ha"), cache)
	thumbs, models := runIndex(f, 0, "mkdir", "-p", cache+"/thumbs"), runIndex(f, 0, "mkdir", "-p", cache+"/models")
	data := runIndex(f, 0, "btrfs", "subvolume", "delete", "/var/lib/briard/data/ha")
	if del < 0 || made < del || thumbs < made || models < made || data < max(thumbs, models) {
		t.Errorf("the cache was not emptied and made again before the data: %v", f.runs)
	}
}

// TestDataReplaceSweepsTheStagedCopyBeforeItGoesLive, and the order is the property: a
// member taken around a household's own backup restore carries the request that started it, and
// HA's wipe deliberately keeps the tar -- so an unswept restore hands HA both again and the
// household lands straight back where they were trying to leave. The sweep runs on the staged
// copy, never the live one: the rename is what makes any of it visible, so a crash leaves either
// the old subvolume or a swept one, never a half-swept live one.
func TestDataReplaceSweepsTheStagedCopyBeforeItGoesLive(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.Restore(context.Background(), "/var/lib/briard/data/ha", "/var/lib/briard/data/.snapshots/ha-1", []string{"app/.HA_RESTORE"}); err != nil {
		t.Fatal(err)
	}
	sweep, mv := -1, -1
	for i, r := range f.runs {
		if r[0] == "rm" {
			if got, want := r[len(r)-1], "/var/lib/briard/data/ha.restoring/app/.HA_RESTORE"; got != want {
				t.Errorf("swept %q, want the STAGED copy's %q", got, want)
			}
			sweep = i
		}
		if r[0] == "mv" {
			mv = i
		}
	}
	if sweep < 0 {
		t.Fatalf("the marker was never swept: %v", f.runs)
	}
	if mv < 0 || sweep > mv {
		t.Errorf("the sweep ran after the copy went live: %v", f.runs)
	}
}

// TestDataReplaceRefusesAPathOutsideTheMember: the sweep list arrives over the channel, so it is
// the one place this verb could be talked into unlinking something else on the volume. Relative
// and inside the member, or refused.
func TestDataReplaceRefusesAPathOutsideTheMember(t *testing.T) {
	for _, bad := range []string{"/etc/passwd", "../../etc/passwd", "app/../../x", ""} {
		f := &fakeExec{}
		g := dial(t, f)
		if err := g.Restore(context.Background(), "/var/lib/briard/data/ha", "/snap", []string{bad}); err == nil {
			t.Errorf("sweep %q was accepted", bad)
		}
		for _, r := range f.runs {
			if r[0] == "mv" {
				t.Errorf("sweep %q: the copy went live anyway: %v", bad, f.runs)
			}
		}
	}
}

// And the live subvolume survives a bad restore point, which is the whole point of the order: an
// unusable source must cost the household nothing.
//
// Only the source probe fails here, and that is deliberate -- a fake that errors on everything
// would pass this assertion no matter which order the code ran in, because nothing would get far
// enough to delete. This is the shape that actually distinguishes verify-first from delete-first.
func TestDataRestoreRefusesAnUnusableRestorePoint(t *testing.T) {
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if len(args) >= 3 && args[0] == "subvolume" && args[1] == "show" && args[2] == "/snap" {
			return nil, errors.New("No such file or directory")
		}
		return nil, nil
	}}
	g := dial(t, f)
	if err := g.Restore(context.Background(), "/var/lib/briard/data/ha", "/snap", nil); err == nil {
		t.Fatal("restore accepted a restore point it could not read")
	}
	for _, r := range f.runs {
		if len(r) >= 3 && r[1] == "subvolume" && r[2] == "delete" {
			t.Errorf("restore deleted something before validating the source: %v", f.runs)
		}
		if r[0] == "mv" {
			t.Errorf("restore moved something despite an unusable source: %v", f.runs)
		}
	}
}

func TestSystemPathReadsCurrentSystem(t *testing.T) {
	f := &fakeExec{output: []byte("/nix/store/abc123-nixos-system-guest-25.05\n")}
	g := dial(t, f)
	path, err := g.SystemPath(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if path != "/nix/store/abc123-nixos-system-guest-25.05" {
		t.Errorf("SystemPath = %q", path)
	}
	want := [][]string{{"readlink", "-f", "/run/current-system"}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

// os.poweroff must return before the machine goes down, or the reply races the shutdown and the
// host sees a dead channel, indistinguishable from a guest that crashed -- the one thing a
// clean-shutdown path must never look like. --no-block buys that, so it is asserted.
func TestPowerOffDoesNotBlockOnTheShutdown(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.PowerOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"systemctl", "poweroff", "--no-block"}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

// cancellingExec is the guest's own shutdown, modelled at the only point that matters: the
// command cancels the serve context while it is still running, exactly as `systemctl poweroff`
// makes PID 1 SIGTERM this agent before systemctl itself has exited. It then reports its own
// context the way exec.CommandContext does -- a child killed by cancellation surfaces as
// ctx.Err(), not as an exit status -- so an attached handler fails here and a detached one
// does not.
type cancellingExec struct {
	fakeExec
	cancel context.CancelFunc
	cmdErr error // what the command's context said once the agent had been told to stop
}

func (c *cancellingExec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	c.runs = append(c.runs, append([]string{name}, args...))
	c.cancel() // synchronous: the serve context is done by the time this returns
	c.cmdErr = ctx.Err()
	return nil, c.cmdErr
}

// os.poweroff must survive the cancellation it causes. The shutdown it asks for stops this agent,
// so a handler on the dispatch context is killed by its own success and answers "context
// canceled" for a request that worked; the host cannot tell that from a refusal, escalates to the
// ACPI power button, and the clean-shutdown assertion in guest-rescue goes red. That error reaches
// the host because the serve loop holds the port open until the reply is written. Both halves are
// asserted: the command outlives the cancellation (cmdErr), and the answer still reaches the host
// (PowerOff's return).
func TestPowerOffOutlivesTheCancellationItCauses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	x := &cancellingExec{cancel: cancel}
	cconn, sconn := net.Pipe()
	go Serve(ctx, sconn, x)
	g := NewClient(cconn)
	t.Cleanup(func() { g.Close() })

	if err := g.PowerOff(context.Background()); err != nil {
		t.Errorf("os.poweroff must succeed through the cancellation it causes, got %v", err)
	}
	if x.cmdErr != nil {
		t.Errorf("the poweroff command was killed by the agent's own shutdown: %v", x.cmdErr)
	}
}

// signalledExec is the second way the shutdown kills the command that asked for it, the one
// detaching the handler from the dispatch context does not cover: systemd still tears the unit
// down, and with the default KillMode=control-group its SIGTERM reaches `systemctl` directly, so
// the child dies by signal rather than by cancellation. That is no refusal -- the machine really
// is going down, which is precisely why the signal came. The manager says so the awkward way:
// `is-system-running` prints `stopping` on stdout and exits non-zero (0 only for `running`), and
// the fixture does both on purpose. A handler that reads the probe's exit status instead of its
// output sees a failed probe on top of a failed command and reports a refusal -- the bug this
// fixture exists to catch.
type signalledExec struct {
	fakeExec
	state string // what the manager reports: "stopping" for the real shutdown, "running" for a refusal
}

func (s *signalledExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.runs = append(s.runs, append([]string{name}, args...))
	switch strings.Join(args, " ") {
	case "poweroff --no-block":
		// The error text of a child killed by a signal: not a context error and not an exit
		// status -- a signal, which is what makes it invisible to the detached context.
		return nil, errors.New("signal: terminated")
	case "is-system-running":
		return []byte(s.state + "\n"), errors.New("exit status 1")
	}
	return nil, nil
}

func (s *signalledExec) asked(what string) bool {
	for _, r := range s.runs {
		if strings.Join(r, " ") == what {
			return true
		}
	}
	return false
}

// The command died and the request worked, and only the manager can say so. Without this the
// host reads `signal: terminated` as a refusal and presses the ACPI button on a guest already
// shutting down, and the clean-shutdown assertion goes red. Fails if the handler returns the
// command's error, which PowerOff then reports.
func TestPowerOffTrustsTheManagerNotTheExitStatus(t *testing.T) {
	x := &signalledExec{state: "stopping"}
	cconn, sconn := net.Pipe()
	go Serve(context.Background(), sconn, x)
	g := NewClient(cconn)
	t.Cleanup(func() { g.Close() })

	if err := g.PowerOff(context.Background()); err != nil {
		t.Errorf("a command killed by the shutdown it started is not a refusal, got %v", err)
	}
	// It must have asked. Without this the test passes just as well for a handler that swallows
	// every error unconditionally -- the other way to make it green, and the wrong one, since
	// that reports a shutdown which never started with exactly as much confidence.
	if !x.asked("systemctl is-system-running") {
		t.Errorf("the verb decided the outcome without asking the manager: %v", x.runs)
	}
}

// And the alarm must survive. A machine that is not stopping is a refusal the host has to hear,
// whatever the command did, and it has to keep the original text: "which route refused" is the
// first thing anyone reading the escalation needs (TestStopCleanlyReportsAGuestThatWillNotGoDown
// in agent/host pins the same property one level up). It stops the behaviour above (trusting the
// manager) from degenerating into "ignore every failure".
func TestPowerOffStillRefusesWhenNothingIsStopping(t *testing.T) {
	x := &signalledExec{state: "running"}
	cconn, sconn := net.Pipe()
	go Serve(context.Background(), sconn, x)
	g := NewClient(cconn)
	t.Cleanup(func() { g.Close() })

	err := g.PowerOff(context.Background())
	if err == nil {
		t.Fatal("a guest that is not shutting down reported a clean poweroff")
	}
	if !strings.Contains(err.Error(), "signal: terminated") {
		t.Errorf("the refusal lost the error that says why: %v", err)
	}
}

func TestReactorPauseResumeCommands(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.ReactorPause(context.Background(), "briard"); err != nil {
		t.Fatal(err)
	}
	if err := g.ReactorResume(context.Background(), "briard"); err != nil {
		t.Fatal(err)
	}
	// No maintenance marker: nothing in the guest switches the OS on its own, so there is nothing
	// autonomous to hold off during a managed op. Pause stops the daemon (apps + Primary stay
	// up); resume restarts it (re-adopts, no demote). Two commands, not four, and the absence is
	// the assertion: pause does not `rm` drbd-reactor's `Before=` drop-in and daemon-reload to
	// defuse the promote-vs-stop deadlock. That defusal lives on drbd-reactor.service's ExecStop,
	// which also covers the stops this verb never sees (a shutdown, the deadman's reboot, a host
	// reboot), so a copy here would only be a second place to keep right. If these commands come
	// back, they came back for a reason that needs writing down.
	want := [][]string{
		{"systemctl", "stop", "drbd-reactor.service"},
		{"systemctl", "start", "drbd-reactor.service"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

const (
	statusSecondary = `[{"name":"r0","role":"Secondary","devices":[{"quorum":true}],"connections":[{"connection-state":"Connected"}]}]`
	statusPrimary   = `[{"name":"r0","role":"Primary","devices":[{"quorum":true}],"connections":[{"connection-state":"Connected"}]}]`
)

// WaitPrimary polls until the reactor has promoted this node (Secondary -> Primary).
func TestWaitPrimaryPollsUntilConverged(t *testing.T) {
	polls := 0
	f := &fakeExec{runFn: func(_ string, _ []string) ([]byte, error) {
		polls++
		if polls < 3 {
			return []byte(statusSecondary), nil // not promoted yet
		}
		return []byte(statusPrimary), nil // reactor promoted
	}}
	g := dial(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := g.WaitPrimary(ctx, "r0", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if polls < 3 {
		t.Errorf("converged after %d polls, expected >= 3", polls)
	}
}

// If the node never becomes primary, WaitPrimary returns when ctx expires.
func TestWaitPrimaryTimesOut(t *testing.T) {
	f := &fakeExec{output: []byte(statusSecondary)}
	g := dial(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := g.WaitPrimary(ctx, "r0", time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

// BringUpGuest end-to-end over a real unix socket (the prod transport shape): a
// fake guest listens, serves, and reports Primary -> bring-up converges.
func TestBringUpGuestOverUnixSocket(t *testing.T) {
	sock := filepath.Join(testsock.Dir(t), "ctl.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	f := &fakeExec{output: []byte(statusPrimary)} // already primary -> WaitPrimary returns at once
	go func() {
		if conn, err := ln.Accept(); err == nil {
			Serve(context.Background(), conn, f)
		}
	}()
	spec := BringUpSpec{Storage: demoStorage(false), Promoter: []string{"briard-vip.service"}}
	if err := BringUpGuest(context.Background(), sock, spec); err != nil {
		t.Fatal(err)
	}
	if f.files[nodestorage.Path] == "" {
		t.Error("bring-up landed no storage spec over the socket")
	}
}

// Bring-up must put a runtime-installed app's units back before it starts the promoter. The units
// live on the guest's tmpfs, so a reboot erases them while the host's manifest cache survives, and
// drbd-reactor, started at the end of BringUp, would otherwise promote into a chain naming units
// that do not exist. The ordering is the assertion, not merely that both happened: a render that
// lands after ReactorStart is the bug with extra steps.
func TestBringUpRendersAppUnitsBeforeStartingThePromoter(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)

	err := g.BringUp(context.Background(), BringUpSpec{
		Storage:   demoStorage(false),
		Promoter:  []string{"briard-app.service"},
		AppUnits:  map[string]string{"briard-app.container": "[Container]\nImage=x\n"},
		AppImages: map[string]string{"briard-app-img.service": "ghcr.io/x/y@sha256:abc"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := f.files[quadletDir+"/briard-app.container"]; got == "" {
		t.Fatalf("the app unit was never written under %s: %v", quadletDir, f.files)
	}
	idx := func(want ...string) int {
		for i, r := range f.runs {
			if len(r) < len(want) {
				continue
			}
			match := true
			for j, w := range want {
				if r[j] != w {
					match = false
					break
				}
			}
			if match {
				return i
			}
		}
		return -1
	}
	reload := idx("systemctl", "daemon-reload")
	// The warm is an exists check, not a start: starting the .image unit is `podman image pull`,
	// so an unconditional start makes every reboot need a registry. The pull path is asserted
	// separately below.
	warm := idx("podman", "image", "exists", "ghcr.io/x/y@sha256:abc")
	reactor := idx("systemctl", "start", "drbd-reactor.service")
	if reload < 0 || warm < 0 || reactor < 0 {
		t.Fatalf("missing a step: daemon-reload=%d warm=%d reactor=%d (%v)", reload, warm, reactor, f.runs)
	}
	if !(reload < warm && warm < reactor) {
		t.Errorf("wrong order: daemon-reload=%d, warm=%d, reactor=%d — units and images must both precede the promoter", reload, warm, reactor)
	}
	// A present image must not be pulled. This is the whole of app.warm: bring-up runs after
	// every guest reboot, and the doctrine is that running never needs network.
	if got := idx("systemctl", "restart", "briard-app-img.service"); got >= 0 {
		t.Errorf("bring-up started the .image unit for an image already present (step %d) — that is a registry pull on the reboot path: %v", got, f.runs)
	}
}

// ...and when the image is genuinely missing, bring-up pulls it rather than refusing. Absence
// should not arise -- install warms, prewarm puts the image on every standby -- but if it does, a
// short wait beats a node that will not come up. The two halves are asserted together because
// either alone is satisfiable by a wrong implementation: skip-always passes the first, pull-always
// passes the second.
func TestBringUpPullsOnlyAnImageThatIsMissing(t *testing.T) {
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if name == "podman" && len(args) >= 2 && args[0] == "image" && args[1] == "exists" {
			return nil, errors.New("no such image")
		}
		return nil, nil
	}}
	g := dial(t, f)
	err := g.BringUp(context.Background(), BringUpSpec{
		Storage:   demoStorage(false),
		Promoter:  []string{"briard-app.service"},
		AppUnits:  map[string]string{"briard-app.container": "[Container]\nImage=x\n"},
		AppImages: map[string]string{"briard-app-img.service": "ghcr.io/x/y@sha256:abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var pulled bool
	for _, r := range f.runs {
		if len(r) >= 3 && r[0] == "systemctl" && r[1] == "restart" && r[2] == "briard-app-img.service" {
			pulled = true
		}
	}
	if !pulled {
		t.Errorf("a MISSING image was not warmed; absence must be pulled, not refused: %v", f.runs)
	}
}

// ...and a node with no installed app renders nothing, so the shipped zero-app node is
// untouched by the above. Without this, "renders before the promoter" could be satisfied by
// rendering unconditionally, which would write an empty unit set on every ordinary bring-up.
func TestBringUpRendersNothingWithNoInstalledApp(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)

	err := g.BringUp(context.Background(), BringUpSpec{
		Storage:  demoStorage(false),
		Promoter: []string{"briard-app.service"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for path := range f.files {
		if strings.HasPrefix(path, quadletDir+"/") {
			t.Errorf("a node with no installed app wrote a quadlet unit: %s", path)
		}
	}
}

// A mount point is a path with a filesystem mounted at it, read from the mount table: not a path
// under one (a Secondary's data root sits on the root filesystem), and the kernel's octal escapes
// undone (a space is "\040" there).
func TestIsMountPointReadsTheMountTable(t *testing.T) {
	f := &fakeExec{files: map[string]string{mountinfoPath: mountinfo("/", "/var/lib", `/srv/a\040b`)}}
	for p, want := range map[string]bool{
		"/":                true,
		"/var/lib":         true,
		dataMountRoot:      false,
		"/srv/a b":         true,
		`/srv/a\040b`:      false,
		"/var/lib/briard/": false,
	} {
		if got := isMountPoint(f, p); got != want {
			t.Errorf("isMountPoint(%q) = %v, want %v", p, got, want)
		}
	}
	if isMountPoint(&fakeExec{}, "/") {
		t.Error("an unreadable mount table read as mounted")
	}
}

// Under DHCP the host stops choosing the VIP, so it has to be able to ask what the address turned
// out to be. Ground truth is the interface, and the answer for an unaddressed device is "" -- not
// an error, because the host asks every cycle and a Secondary holding no VIP is the normal case.
// An error there would read as a dead channel and trigger a reconnect every poll.
func TestVIPReadsTheLiveAddress(t *testing.T) {
	// The NIC's own address first, then a secondary: the VIP is the first.
	f := &fakeExec{addrs: map[string][]string{"eth2": {"192.168.9.50/24", "192.168.9.51/24"}}}
	g := dial(t, f)
	got, err := g.VIP(context.Background(), "eth2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.9.50/24" {
		t.Errorf("VIP = %q, want 192.168.9.50/24", got)
	}
}

// A device with no global address (Secondary) answers "" and no error, and so does a device that
// is not there at all: a transient absence must not read as a dead channel.
func TestVIPUnaddressedIsEmptyNotAnError(t *testing.T) {
	for name, addrs := range map[string]map[string][]string{
		"unaddressed": {"eth2": nil},
		"absent":      nil,
	} {
		g := dial(t, &fakeExec{addrs: addrs})
		got, err := g.VIP(context.Background(), "eth2")
		if err != nil {
			t.Fatalf("%s: must not be an error: %v", name, err)
		}
		if got != "" {
			t.Errorf("%s: VIP = %q, want empty", name, got)
		}
	}
}

// A witness has no VIP device at all; asking must be answerable, not an error.
func TestVIPNoDeviceIsEmptyNotAnError(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	got, err := g.VIP(context.Background(), "")
	if err != nil {
		t.Fatalf("a node with no VIP device must not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("VIP = %q, want empty", got)
	}
	if len(f.runs) != 0 {
		t.Errorf("no device means no ip call; ran %v", f.runs)
	}
}

// The boot id is what lets the host tell a bounced in-guest agent from a rebooted guest, so the
// handshake has to carry it end to end -- read in the guest, over the wire, onto the Client.
func TestHandshakeReportsBootID(t *testing.T) {
	f := &fakeExec{files: map[string]string{"/proc/sys/kernel/random/boot_id": "0f9c2b1e-3d4a-4c5b-8e7f-1a2b3c4d5e6f\n"}}
	g := dial(t, f)
	h, err := g.Handshake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const want = "0f9c2b1e-3d4a-4c5b-8e7f-1a2b3c4d5e6f" // trailing newline trimmed
	if h.BootID != want {
		t.Errorf("hello boot id = %q, want %q", h.BootID, want)
	}
	if g.BootID() != want {
		t.Errorf("Client.BootID() = %q, want %q", g.BootID(), want)
	}
}

// A guest that cannot produce a boot id must still handshake. The handshake is what proves the
// channel is live and what carries the verb set the host drives the guest by; failing it over a
// field no host has ever needed to drive a guest would turn a missing diagnostic into an
// unusable node. Empty then reads as "no evidence", which is what the host's reboot check
// requires.
func TestHandshakeWithoutBootIDStillSucceeds(t *testing.T) {
	g := dial(t, &fakeExec{}) // ReadFile of an unseeded path is os.ErrNotExist
	h, err := g.Handshake(context.Background())
	if err != nil {
		t.Fatalf("a guest with no readable boot id must still handshake: %v", err)
	}
	if h.BootID != "" || g.BootID() != "" {
		t.Errorf("boot id = %q/%q, want empty", h.BootID, g.BootID())
	}
	if !g.Supports(verbOSSystem) {
		t.Error("the capability set must still arrive: a missing boot id is a missing diagnostic, not a missing handshake")
	}
}

// The bridge substrate's service identity: the host hands the guest one NIC, so the guest builds
// the second MAC itself. Three properties, and each one is a defect if it goes.
func TestConfigureNetMakesTheServiceNIC(t *testing.T) {
	// `ip link show dev eth2` must fail for the create branch to be reached -- that is the
	// existence check, and a fake that succeeds at everything would silently test the other half.
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if len(args) >= 4 && args[0] == "link" && args[1] == "show" && args[3] == "eth2" {
			return nil, errors.New("Device \"eth2\" does not exist.")
		}
		return []byte("2: eth1    inet 10.7.7.1/24 scope global eth1\\       valid_lft forever"), nil
	}}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{
		Dev: "eth1", CIDR: "10.7.7.1/24", VIPDev: "eth2", VIPAddr: "192.168.9.50/24",
		VIPParent: "eth1", VIPMAC: "52:54:00:ab:cd:ef",
	}); err != nil {
		t.Fatal(err)
	}
	var add []string
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "link" && r[2] == "add" {
			add = r
		}
		// (1) Never brought up here. `ip link add` leaves the device down and that is the standby
		// discipline: a Secondary holding the flock MAC up teaches the switch the wrong port for
		// the VIP. briard-vip.service owns the up, on promotion.
		if len(r) >= 6 && r[1] == "link" && r[2] == "set" && r[4] == "eth2" && r[5] == "up" {
			t.Errorf("brought the service NIC up at configure time (%v) -- that is the promoter's call", r)
		}
	}
	if add == nil {
		t.Fatalf("no `ip link add` for the service NIC; runs = %v", f.runs)
	}
	// (2) A macvlan child of the NIC the host named, in bridge mode so child and parent can talk.
	want := []string{"ip", "link", "add", "eth2", "link", "eth1", "address", "52:54:00:ab:cd:ef", "type", "macvlan", "mode", "bridge"}
	if !reflect.DeepEqual(add, want) {
		t.Errorf("create = %v, want %v", add, want)
	}
	// (3) The MAC is set at creation. A macvlan comes up holding a kernel-random MAC, and a frame
	// emitted before a follow-up `link set address` teaches the switch a port for an address
	// nobody owns -- so an argv that sets it afterwards is a real, narrow defect.
	for i, a := range add {
		if a == "address" && i < len(add)-1 && add[i+1] == "52:54:00:ab:cd:ef" {
			return
		}
	}
	t.Errorf("the flock MAC is not set in the create call: %v", add)
}

// Under macvtap the host built the device, so the guest must create nothing -- the failable half
// of the test above, and the one that keeps this from firing on the default substrate.
func TestConfigureNetMakesNoNICUnderMacvtap(t *testing.T) {
	f := &fakeExec{output: []byte("2: eth1    inet 10.7.7.1/24 scope global eth1\\       valid_lft forever")}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{
		Dev: "eth1", CIDR: "10.7.7.1/24", VIPDev: "eth2", VIPAddr: "192.168.9.50/24",
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "link" && r[2] == "add" {
			t.Errorf("created %v with no VIPParent -- under macvtap the host owns that device", r)
		}
	}
}

// An existing service NIC has its MAC re-asserted rather than assumed. The flock MAC is
// flock-scoped, so an adoption changes it under a device that outlives the change;
// without this the joiner keeps presenting its old flock's identity on the LAN.
func TestConfigureNetReassertsTheFlockMAC(t *testing.T) {
	f := &fakeExec{output: []byte("2: eth1    inet 10.7.7.1/24 scope global eth1\\       valid_lft forever")}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{
		Dev: "eth1", CIDR: "10.7.7.1/24", VIPDev: "eth2", VIPAddr: "192.168.9.50/24",
		VIPParent: "eth1", VIPMAC: "52:54:00:11:22:33",
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"ip", "link", "set", "dev", "eth2", "address", "52:54:00:11:22:33"}
	for _, r := range f.runs {
		if reflect.DeepEqual(r, want) {
			return
		}
	}
	t.Errorf("an existing service NIC kept whatever MAC it had; runs = %v", f.runs)
}

// TestTheAppSpecificVerbsAreAdvertisedAndLandOnTheirOwnApp. Each has to be in the handshake or a
// capability-checking host cannot see it however well the switch handles it, and each has to
// reach the one app it is named for. The request carries no app name (the app is in the verb), so
// naming an app the verb is not for cannot be expressed — the better version of refusing it.
func TestTheAppSpecificVerbsAreAdvertisedAndLandOnTheirOwnApp(t *testing.T) {
	g := dial(t, &fakeExec{})
	if _, err := g.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{verbHassReadiness, verbHassNudge, verbMosquittoProbe} {
		if !g.Supports(verb) {
			t.Fatalf("guest does not advertise %s, so no host will ever call it", verb)
		}
	}
	// Each reaches its own app's code, and fails there for that app's own honest reason:
	// this fake guest has no control token, and no manifest on its volume.
	_, err := g.HassReadiness(context.Background(), 8123)
	if err == nil {
		t.Fatal("a guest with no control token reported a readiness sample")
	}
	if !strings.Contains(err.Error(), hass.TokenPath) {
		t.Fatalf("the readiness verb did not reach Home Assistant's sampler: %v", err)
	}
	_, err = g.MosquittoProbe(context.Background(), "")
	if err == nil {
		t.Fatal("a guest with no installed broker reported a probe")
	}
	if !strings.Contains(err.Error(), mosquitto.Name) {
		t.Fatalf("the probe verb did not reach the broker's probe: %v", err)
	}
	// The nudge alone must not fail here: its caller fires it after every install without knowing
	// what the node runs, so a volume with no Home Assistant is an answer (nobody to tell), not the
	// failure the two sampling verbs above correctly report. Reporting it as one would put a scary
	// line in the log of every broker-only install.
	told, err := g.HassNudge(context.Background())
	if err != nil {
		t.Fatalf("a guest with no Home Assistant failed the nudge instead of answering it: %v", err)
	}
	if told {
		t.Fatal("a guest with no Home Assistant claimed it told one")
	}
}

// dashboard.handoff writes the code the household dashboard admits a browser on:
// beside its final name, 0600 before the move, then moved in -- so the dashboard never reads half
// a code and nothing but root ever sees one.
func TestDashboardHandoffIsWrittenPrivatelyThenMovedIn(t *testing.T) {
	x := &fakeExec{}
	raw, _ := json.Marshal(dashboard.Handoff{Code: "abc", Name: "Kostas", Username: "kostas", Language: "el", Issued: time.Now()})
	if _, err := dispatch(x)(context.Background(), verbDashboardHandoff, raw); err != nil {
		t.Fatal(err)
	}
	tmp := dashboard.HandoffPath + ".new"
	if got := x.files[tmp]; !strings.Contains(got, `"code":"abc"`) || !strings.Contains(got, `"username":"kostas"`) {
		t.Errorf("written %q; want the handoff, code and account included", got)
	}
	want := [][]string{
		{"mkdir", "-p", "-m", "0700", dashboard.Dir},
		{"chmod", "0600", tmp},
		{"mv", "-f", tmp, dashboard.HandoffPath},
	}
	if !reflect.DeepEqual(x.runs, want) {
		t.Errorf("runs = %v, want %v", x.runs, want)
	}
	if _, err := dispatch(x)(context.Background(), verbDashboardHandoff, []byte(`{"name":"x"}`)); err == nil {
		t.Error("a handoff with no code was accepted")
	}
}

// app.pulling records a pull for the dashboard's bar and clears it; storage.free measures
// the image store through df. The guest keeps a number and measures a filesystem;
// deciding is the host's.
func TestAppPullingRecordsAndClearsTheBarsTotal(t *testing.T) {
	x := &fakeExec{}
	raw, _ := json.Marshal(appPullingRequest{App: "home-assistant", Size: 621628919, InstalledSize: 2486168064})
	if _, err := dispatch(x)(context.Background(), verbAppPulling, raw); err != nil {
		t.Fatal(err)
	}
	got := x.files[dashboard.PullPath("home-assistant")]
	if !strings.Contains(got, `"size":621628919`) || !strings.Contains(got, `"installedSize":2486168064`) || !strings.Contains(got, `"started":"`) {
		t.Errorf("recorded %q; want the two sizes and a start time", got)
	}
	if !reflect.DeepEqual(x.runs, [][]string{{"mkdir", "-p", "-m", "0700", dashboard.Dir}}) {
		t.Errorf("runs = %v", x.runs)
	}
	// A name that is a path is refused before it becomes one.
	raw, _ = json.Marshal(appPullingRequest{App: "../etc", Size: 1, InstalledSize: 1})
	if _, err := dispatch(x)(context.Background(), verbAppPulling, raw); err == nil {
		t.Error("a path-shaped app name was accepted")
	}
	// Done clears it -- rm -f, so an absent record is not an error.
	x = &fakeExec{}
	raw, _ = json.Marshal(appPullingRequest{App: "home-assistant", Done: true})
	if _, err := dispatch(x)(context.Background(), verbAppPulling, raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(x.runs, [][]string{{"rm", "-f", dashboard.PullPath("home-assistant")}}) {
		t.Errorf("runs = %v", x.runs)
	}
}

func TestStorageFreeReadsDf(t *testing.T) {
	x := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if name != "df" || !reflect.DeepEqual(args, []string{"-B1", "--output=avail,size", storageRoot}) {
			t.Errorf("ran %s %v", name, args)
		}
		return []byte("     Avail        Size\n9876543210 17179869184\n"), nil
	}}
	out, err := dispatch(x)(context.Background(), verbStorageFree, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	r, ok := out.(storageFreeReply)
	if !ok || r.Free != 9876543210 || r.Total != 17179869184 || r.Path != storageRoot {
		t.Errorf("reply = %+v", out)
	}
	x = &fakeExec{output: []byte("df: /var/lib/containers/storage: No such file or directory"), err: errors.New("exit 1")}
	if _, err := dispatch(x)(context.Background(), verbStorageFree, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "No such file") {
		t.Errorf("a failed df = %v; want the error with df's own words", err)
	}
}

// storage.grow waits for the kernel to see the size the host resized the disk to, and only then
// runs resize2fs: run first, it would find nothing to grow and the host's grow would be a no-op.
func TestStorageGrowWaitsForTheSizeThenResizes(t *testing.T) {
	polls := 0
	x := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		switch name {
		case "blockdev":
			polls++
			if polls < 3 {
				return []byte("1073741824\n"), nil // the kernel has not seen the new capacity yet
			}
			return []byte("5368709120\n"), nil
		case "resize2fs":
			if polls < 3 {
				t.Errorf("resize2fs ran at poll %d, before the kernel saw the new size", polls)
			}
			if !reflect.DeepEqual(args, []string{stateDiskDev}) {
				t.Errorf("resize2fs %v", args)
			}
			return nil, nil
		}
		t.Errorf("ran %s %v", name, args)
		return nil, nil
	}}
	if _, err := dispatch(x)(context.Background(), verbStorageGrow, []byte(`{"size":5368709120}`)); err != nil {
		t.Fatal(err)
	}
	if !x.ranArgv("resize2fs", stateDiskDev) {
		t.Error("resize2fs never ran")
	}
}

// A disk that never reaches the size is an error naming both numbers, and the filesystem is not
// touched: growing it to a size the device does not have is not a thing to attempt.
func TestStorageGrowRefusesADiskThatNeverGrew(t *testing.T) {
	x := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if name == "resize2fs" {
			t.Error("resize2fs ran on a disk that never reached the size")
		}
		return []byte("1073741824\n"), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := dispatch(x)(ctx, verbStorageGrow, []byte(`{"size":5368709120}`)); err == nil {
		t.Error("a disk stuck at 1 GiB grew to 5 GiB without an error")
	}
}

// image.remove deletes what is there and treats what is not as already gone: the host asks after
// an app moved off an image, and an image something else already removed is the same outcome.
func TestImageRemoveDeletesWhatIsThereOnly(t *testing.T) {
	const ref = "ghcr.io/home-assistant/home-assistant@sha256:aa"
	present := &fakeExec{runFn: func(name string, args []string) ([]byte, error) { return nil, nil }}
	if _, err := dispatch(present)(context.Background(), verbImageRemove, []byte(`{"ref":"`+ref+`"}`)); err != nil {
		t.Fatal(err)
	}
	if !present.ranArgv("podman", "rmi", ref) {
		t.Errorf("a resident image was not removed: %v", present.runs)
	}
	absent := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "image" {
			return nil, errors.New("exit 1") // podman image exists: not here
		}
		return nil, nil
	}}
	if _, err := dispatch(absent)(context.Background(), verbImageRemove, []byte(`{"ref":"`+ref+`"}`)); err != nil {
		t.Errorf("removing an absent image = %v; want nil", err)
	}
	if absent.ranArgv("podman", "rmi", ref) {
		t.Error("rmi ran for an image that is not here")
	}
}

// loneStorage is a node that runs no DRBD.
func loneStorage() nodestorage.Spec {
	s := demoStorage(true)
	s.Resource.Replicated = false
	s.Resource.Config = ""
	s.Resource.Device = s.Tiers[0].Mapper()
	return s
}

// A lone node's bring-up ends in its static chain target, not in drbd-reactor, and lands no
// promoter snippet: the same chain, a different trigger.
func TestBringUpLoneNode(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	spec := BringUpSpec{Storage: loneStorage(), Promoter: []string{"briard-primary-storage.service", "briard-apps.service"}}
	if err := g.BringUp(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if f.files[reactorPath] != "" {
		t.Error("a lone node got a promoter snippet for a reactor it does not run")
	}
	want := [][]string{
		{"systemctl", "start", nodeStorageUnit},
		{"systemctl", "start", chainTarget},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("bring-up sequence = %v, want %v", f.runs, want)
	}
}

// ★ The second branch of the one predicate: a lone node reads as Primary and quorate
// when its chain target is active, Diskful and UpToDate always, with nobody connected -- so
// Serving() follows the chain and the standby rule's PeerCanTakeOver() says no.
func TestStatusLoneNodeReadsFromTheChain(t *testing.T) {
	for _, tc := range []struct {
		state   string
		serving bool
	}{{"active", true}, {"inactive", false}} {
		f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
			if name == "drbdsetup" {
				return nil, errors.New("drbdsetup: a lone node asked DRBD")
			}
			if name == "systemctl" && len(args) == 2 && args[0] == "is-active" && args[1] == chainTarget {
				if tc.state != "active" {
					return []byte(tc.state + "\n"), errors.New("exit status 3")
				}
				return []byte("active\n"), nil
			}
			return nil, nil
		}}
		raw, _ := loneStorage().Marshal()
		_ = f.WriteFile(nodestorage.Path, raw)
		g := dial(t, f)
		cl, err := g.Cluster(context.Background(), "r0")
		if err != nil {
			t.Fatalf("chain %s: %v", tc.state, err)
		}
		if cl.Serving() != tc.serving || !cl.Quorate || !cl.Diskful || !cl.UpToDate || cl.Connected != 0 || cl.PeerCanTakeOver() {
			t.Errorf("chain %s: status = %+v", tc.state, cl)
		}
	}
}

// The promoter verbs on a lone node: nothing is ever paused (so the overlap guard passes), pause
// and resume touch nothing, and evict is the same gesture on the chain target -- keep-masked
// stops the chain (the target and its root, which every member requires) and masks it, unmask
// releases and starts it, a plain evict is a restart. Never drbd-reactorctl.
func TestReactorVerbsOnALoneNode(t *testing.T) {
	f := &fakeExec{}
	raw, _ := loneStorage().Marshal()
	_ = f.WriteFile(nodestorage.Path, raw)
	g := dial(t, f)
	ctx := context.Background()
	if active, err := g.ReactorActive(ctx); err != nil || !active {
		t.Errorf("ReactorActive = %t, %v; the overlap guard would refuse every app install", active, err)
	}
	if err := g.ReactorPause(ctx, "r0"); err != nil {
		t.Errorf("pause: %v", err)
	}
	if err := g.ReactorResume(ctx, "r0"); err != nil {
		t.Errorf("resume: %v", err)
	}
	for _, r := range f.runs {
		if r[0] == "systemctl" || r[0] == "drbd-reactorctl" {
			t.Errorf("a lone node's pause/resume ran %v", r)
		}
	}
	// ⚠️ Both mask directions go through the renderer: the chain target is a file this agent
	// writes into the very directory `mask --runtime` puts its /dev/null symlink in. Masking clears
	// the file first and reloads (systemd caches the loaded unit, and refuses to mask over an
	// existing file at all); unmasking re-renders, or dropping the symlink leaves no unit for the
	// start to find. The extra reloads in the want-lists are the assertion: an evict that stopped
	// reloading would mask a node that systemd still happily starts.
	guestWithTools(t)
	for _, tc := range []struct {
		name               string
		keepMasked, unmask bool
		want               [][]string
	}{
		{"keep-masked", true, false, [][]string{{"systemctl", "stop", chainTarget, chainRoot}, {"systemctl", "mask", "--runtime", chainTarget}, {"systemctl", "daemon-reload"}}},
		{"unmask", false, true, [][]string{{"systemctl", "unmask", "--runtime", chainTarget}, {"systemctl", "daemon-reload"}, {"systemctl", "start", chainTarget}}},
		{"plain", false, false, [][]string{{"systemctl", "stop", chainTarget, chainRoot}, {"systemctl", "start", chainTarget}}},
	} {
		f.runs = nil
		if err := g.ReactorEvict(ctx, tc.keepMasked, tc.unmask); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !reflect.DeepEqual(f.runs, tc.want) {
			t.Errorf("%s: ran %v, want %v", tc.name, f.runs, tc.want)
		}
	}
}

// TestRingTakeIsAdvertised: the ring's take is reachable only by a name an old guest does not
// advertise, because the field it adds is one whose absence would be silent -- an older guest
// would take the call, ignore the field and report success while the volume filled with
// unlabelled subvolumes. A name is the whole instrument here: Supports then refuses this
// one path and leaves every other working.
func TestRingTakeIsAdvertised(t *testing.T) {
	var advertised bool
	for _, c := range guestCapabilities {
		if c == verbDataMember {
			advertised = true
		}
	}
	if !advertised {
		t.Fatal("data.member is not advertised; a capability-checking host would never use it")
	}
}

// TestHandshakeCarriesNoVersionNumber, and ⚠️ adding one is a decision, not a tidy-up. A number
// can only refuse the whole channel, and the channel is what fixes a node: a vm release is
// applied through the guest's os.* verbs, so a host that refuses the handshake can never reach the
// image that would satisfy it. Nor would it measure anything: the guest agent is pushed from the
// host's own tree, so its constants are the host's. The absence is pinned on the wire, where an
// added field would land.
func TestHandshakeCarriesNoVersionNumber(t *testing.T) {
	b, err := json.Marshal(guestfirmware.HelloReply(&fakeExec{}, guestCapabilities))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["version"]; ok {
		t.Errorf("the handshake carries a version field again (%s); see this test's comment", b)
	}
	// The assertion is only worth anything while the reply it inspects is really a handshake.
	if _, ok := wire["capabilities"]; !ok {
		t.Errorf("the handshake no longer carries a capability set, so this test proves nothing: %s", b)
	}
}

// TestImageEnsureIsResidentOrPulls, and ⚠️ it must not start a unit. app.warm starts a rendered
// `.image` unit, which only exists for a manifest the node has already rendered. A restore asks
// about a member's pinned identity -- possibly months old, quite possibly never rendered here --
// so the question has to be answerable by ref alone.
func TestImageEnsureIsResidentOrPulls(t *testing.T) {
	const ref = "ghcr.io/x/ha@sha256:abc"

	// Resident: the check is the whole answer, and nothing is fetched.
	f := &fakeExec{} // every command succeeds, so `image exists` does
	if err := dial(t, f).EnsureImage(context.Background(), ref); err != nil {
		t.Fatalf("a resident image was refused: %v", err)
	}
	if !reflect.DeepEqual(f.runs, [][]string{{"podman", "image", "exists", ref}}) {
		t.Errorf("runs = %v, want the existence check alone", f.runs)
	}

	// Absent: fetched by ref. Never `systemctl start` -- that is the unit path, and there is no
	// unit for a manifest this node has not rendered.
	f = &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if len(args) > 1 && args[1] == "exists" {
			return nil, errors.New("image not known")
		}
		return nil, nil
	}}
	if err := dial(t, f).EnsureImage(context.Background(), ref); err != nil {
		t.Fatalf("an absent image was not fetched: %v", err)
	}
	if !f.ranArgv("podman", "pull", ref) {
		t.Errorf("runs = %v, want a pull by ref", f.runs)
	}
	for _, r := range f.runs {
		if r[0] == "systemctl" {
			t.Errorf("image.ensure started a unit: %v", f.runs)
		}
	}
}

// TestImageEnsureReportsAPullItCannotDo: the refusal is the feature. A restore that cannot get
// the code its member was taken under must cancel before it stops anything, so this failing
// loudly is what lets the caller leave the household exactly where it was.
func TestImageEnsureReportsAPullItCannotDo(t *testing.T) {
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		return []byte("manifest unknown"), errors.New("exit status 125")
	}}
	err := dial(t, f).EnsureImage(context.Background(), "ghcr.io/x/gone@sha256:abc")
	if err == nil {
		t.Fatal("a pull that could not happen was reported as success")
	}
	if !strings.Contains(err.Error(), "gone@sha256:abc") {
		t.Errorf("error = %v, want it to name the ref it could not get", err)
	}
}

// ranArgv reports whether the fake was asked to run exactly this argv.
func (f *fakeExec) ranArgv(argv ...string) bool {
	for _, r := range f.runs {
		if reflect.DeepEqual(r, argv) {
			return true
		}
	}
	return false
}

// cert.read hands back the cert as it sits on the volume, and a volume holding none answers ""
// -- a node with no Briard account is never issued one, and doctor must read that as "none",
// which it can only do if absence is not an error.
func TestReadCert(t *testing.T) {
	g := dial(t, &fakeExec{files: map[string]string{tlsCertPath: "cert-pem-bytes"}})
	got, err := g.ReadCert(context.Background())
	if err != nil || got != "cert-pem-bytes" {
		t.Errorf("ReadCert = %q, %v; want the file's contents", got, err)
	}
	got, err = dial(t, &fakeExec{}).ReadCert(context.Background())
	if err != nil || got != "" {
		t.Errorf("ReadCert with no cert = %q, %v; want \"\" and no error", got, err)
	}
}

// dashboard.casa writes the host's view of the household's name for the page: beside
// its final name then moved in, like the handoff, so the page never reads half a state. Not
// 0600: it holds no secret, only what the page shows.
func TestDashboardCasaIsWrittenThenMovedIn(t *testing.T) {
	x := &fakeExec{}
	raw, _ := json.Marshal(dashboard.Casa{State: "registered", Name: "alert-fox.briard.casa", Email: "a@x.org"})
	if _, err := dispatch(x)(context.Background(), verbDashboardCasa, raw); err != nil {
		t.Fatal(err)
	}
	tmp := dashboard.CasaPath + ".new"
	if got := x.files[tmp]; !strings.Contains(got, `"state":"registered"`) || !strings.Contains(got, `"name":"alert-fox.briard.casa"`) {
		t.Errorf("written %q; want the view", got)
	}
	// Home Assistant's copy, written the same way and readable by the container: empty, because
	// this name is registered but not known to resolve on the household's network.
	urlTmp := hass.ExternalURLPath + ".new"
	want := [][]string{
		{"mkdir", "-p", "-m", "0700", dashboard.Dir},
		{"mv", "-f", tmp, dashboard.CasaPath},
		{"mkdir", "-p", hass.Dir},
		{"chmod", "0644", urlTmp},
		{"mv", "-f", urlTmp, hass.ExternalURLPath},
	}
	if !reflect.DeepEqual(x.runs, want) {
		t.Errorf("runs = %v, want %v", x.runs, want)
	}
	if got, ok := x.files[urlTmp]; !ok || got != "" {
		t.Errorf("external URL = %q (written %v), want an empty file for a name not known to resolve", got, ok)
	}
	raw, _ = json.Marshal(dashboard.Casa{State: "registered", Name: "alert-fox.briard.casa", Resolves: dashboard.CasaResolves})
	if _, err := dispatch(x)(context.Background(), verbDashboardCasa, raw); err != nil {
		t.Fatal(err)
	}
	if got := x.files[urlTmp]; got != "https://home-assistant.alert-fox.briard.casa" {
		t.Errorf("external URL = %q, want the app's casa name once it resolves", got)
	}
	if _, err := dispatch(x)(context.Background(), verbDashboardCasa, []byte(`not json`)); err == nil {
		t.Error("a malformed view was accepted")
	}
}

// An image the OS image itself carries is kept: it is loaded again at every boot, and on a node
// with no registry it is the only copy a later install of that version can use.
func TestImageRemoveKeepsAnImageTheOSCarries(t *testing.T) {
	const ref = "localhost/briard-dummy@sha256:aa"
	x := &fakeExec{
		files: map[string]string{stagedImagesPath: "1111\n2222\n"},
		runFn: func(name string, args []string) ([]byte, error) {
			if len(args) > 1 && args[0] == "image" && args[1] == "inspect" {
				return []byte("2222\n"), nil
			}
			return nil, nil
		},
	}
	out, err := dispatch(x)(context.Background(), verbImageRemove, []byte(`{"ref":"`+ref+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := out.(imageRemoveReply); !ok || !r.Kept {
		t.Errorf("reply = %+v, want kept", out)
	}
	if x.ranArgv("podman", "rmi", ref) {
		t.Error("a staged image was removed")
	}
}

// MDNSOther reads the reverse proxy's record of another briard heard at bring-up; absent is "" --
// none heard is the common case and must not read as a fault.
func TestMDNSOtherReadsTheProxyRecordAndIsEmptyWhenNoneWasHeard(t *testing.T) {
	g := dial(t, &fakeExec{files: map[string]string{mdnsOtherPath: "192.168.1.50\n"}})
	got, err := g.MDNSOther(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.1.50" {
		t.Errorf("MDNSOther = %q, want the recorded address", got)
	}
	g = dial(t, &fakeExec{})
	if got, err := g.MDNSOther(context.Background()); err != nil || got != "" {
		t.Errorf("MDNSOther with no record = %q, %v; want empty and no error", got, err)
	}
}

// deadman.episode hands over what the deadman left and forgets it: the second read says nothing,
// so the host records an episode once. An absent file is the ordinary answer, not an error.
func TestDeadmanEpisodeIsHandedOverOnce(t *testing.T) {
	deadmanLastEpisodePath = filepath.Join(t.TempDir(), "last-episode.json")
	call := func() deadmanEpisodeReply {
		t.Helper()
		out, err := dispatch(&fakeExec{})(context.Background(), verbDeadmanEpisode, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		return out.(deadmanEpisodeReply)
	}
	if r := call(); r.Found {
		t.Fatalf("nothing waited, yet found: %+v", r)
	}
	last := deadman.LastEpisode{Since: time.Unix(1000, 0).UTC(), Until: time.Unix(4000, 0).UTC(), Reboots: 2}
	if err := (deadman.FileState{Path: filepath.Join(filepath.Dir(deadmanLastEpisodePath), "episode.json")}).SaveLast(last); err != nil {
		t.Fatal(err)
	}
	r := call()
	if !r.Found || r.Episode.Reboots != 2 || !r.Episode.Since.Equal(last.Since) || !r.Episode.Until.Equal(last.Until) {
		t.Fatalf("handed over %+v, want %+v", r, last)
	}
	if r := call(); r.Found {
		t.Fatalf("the episode was handed over twice: %+v", r)
	}
	if _, err := os.Stat(deadmanLastEpisodePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the handed-over episode was not forgotten: %v", err)
	}
}

// dashboard.alerts writes the host's alert store for the page, the same way as the casa view:
// beside its final name and moved in. A payload that is not a list of records is refused before
// anything is written.
func TestDashboardAlertsIsWrittenThenMovedIn(t *testing.T) {
	x := &fakeExec{}
	raw, _ := json.Marshal([]notify.Record{{Alert: notify.Alert{Key: "disk", Kind: notify.Open, Severity: notify.Warning, Title: "Disk space low"}}})
	if _, err := dispatch(x)(context.Background(), verbDashboardAlerts, raw); err != nil {
		t.Fatal(err)
	}
	tmp := dashboard.AlertsPath + ".new"
	if got := x.files[tmp]; !strings.Contains(got, `"key":"disk"`) || !strings.Contains(got, `"title":"Disk space low"`) {
		t.Errorf("written %q; want the records", got)
	}
	want := [][]string{
		{"mkdir", "-p", "-m", "0755", dashboard.AlertsDir},
		{"mv", "-f", tmp, dashboard.AlertsPath},
	}
	if !reflect.DeepEqual(x.runs, want) {
		t.Errorf("runs = %v, want %v", x.runs, want)
	}
	x = &fakeExec{}
	if _, err := dispatch(x)(context.Background(), verbDashboardAlerts, []byte(`{"state":"registered"}`)); err == nil || len(x.runs) != 0 {
		t.Errorf("a casa view sent as alerts was accepted (err %v, runs %v)", err, x.runs)
	}
}

// os.state reads systemd's answer off stdout, never the exit status (non-zero for every state but
// `running`), and names the failed units only when there are some to name.
func TestOSStateReportsTheStateAndTheFailedUnits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  string
		failed string
		want   SystemState
	}{
		{"running", "running\n", "", SystemState{State: "running"}},
		{"starting is reported, not resolved", "starting\n", "", SystemState{State: "starting"}},
		{"degraded names its units", "degraded\n",
			"systemd-modules-load.service loaded failed failed Load Kernel Modules\nfoo.mount loaded failed failed /foo\n",
			SystemState{State: "degraded", Failed: []string{"systemd-modules-load.service", "foo.mount"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
				if name == "systemctl" && args[0] == "is-system-running" {
					if tc.state == "running\n" {
						return []byte(tc.state), nil
					}
					return []byte(tc.state), errors.New("exit status 1")
				}
				if name == "systemctl" && args[0] == "list-units" {
					return []byte(tc.failed), nil
				}
				return nil, errors.New("unexpected " + name + " " + strings.Join(args, " "))
			}}
			g := dial(t, f)
			if !g.Supports(verbOSState) {
				t.Fatal("os.state is not advertised")
			}
			got, err := g.SystemState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("SystemState = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// dashboard.backup writes the host's view of the backup the same way: beside its final name, then
// moved in, in the page's own 0700 directory.
func TestDashboardBackupIsWrittenThenMovedIn(t *testing.T) {
	x := &fakeExec{}
	raw, _ := json.Marshal(dashboard.Backup{Folder: "/f", On: true})
	if _, err := dispatch(x)(context.Background(), verbDashboardBackup, raw); err != nil {
		t.Fatal(err)
	}
	tmp := dashboard.BackupPath + ".new"
	if got := x.files[tmp]; !strings.Contains(got, `"folder":"/f"`) {
		t.Errorf("written %q; want the view", got)
	}
	want := [][]string{
		{"mkdir", "-p", "-m", "0700", dashboard.Dir},
		{"mv", "-f", tmp, dashboard.BackupPath},
	}
	if !reflect.DeepEqual(x.runs, want) {
		t.Errorf("runs = %v, want %v", x.runs, want)
	}
	if !slices.Contains(guestCapabilities, verbDashboardBackup) || isAct(verbDashboardBackup) {
		t.Error("the push is not advertised, or waits behind acts")
	}
}

// dashboard.catalog writes the host's view of the catalog's index the way the casa view is
// written: parsed as its own shape, into the page's directory, moved in whole -- and the
// installed apps' releases beside the alert copy, for Home Assistant, the same way.
func TestDashboardCatalogIsWrittenThenMovedIn(t *testing.T) {
	x := &fakeExec{}
	raw, _ := json.Marshal(dashboard.Catalog{Apps: []dashboard.CatalogApp{
		{App: manifest.App{Name: "paperless-ngx", Title: "Paperless-ngx"}, Wished: true},
		{App: manifest.App{Name: "home-assistant", Title: "Home Assistant", Available: true, Version: "2026.10.1"}, Installed: true, Running: "2026.9.4", Update: true},
	}})
	if _, err := dispatch(x)(context.Background(), verbDashboardCatalog, raw); err != nil {
		t.Fatal(err)
	}
	tmp := dashboard.CatalogPath + ".new"
	if got := x.files[tmp]; !strings.Contains(got, `"name":"paperless-ngx"`) || !strings.Contains(got, `"wished":true`) {
		t.Errorf("written %q; want the view", got)
	}
	apps := dashboard.AppsPath + ".new"
	if got, want := x.files[apps], `[{"name":"home-assistant","title":"Home Assistant","running":"2026.9.4","latest":"2026.10.1","update":true}]`; got != want {
		t.Errorf("releases written %q; want %q", got, want)
	}
	want := [][]string{
		{"mkdir", "-p", "-m", "0700", dashboard.Dir},
		{"mv", "-f", tmp, dashboard.CatalogPath},
		{"mkdir", "-p", "-m", "0755", dashboard.AlertsDir},
		{"mv", "-f", apps, dashboard.AppsPath},
	}
	if !reflect.DeepEqual(x.runs, want) {
		t.Errorf("runs = %v, want %v", x.runs, want)
	}
	if _, err := dispatch(x)(context.Background(), verbDashboardCatalog, []byte(`not json`)); err == nil {
		t.Error("a malformed view was written")
	}
	if !slices.Contains(guestCapabilities, verbDashboardCatalog) || isAct(verbDashboardCatalog) {
		t.Error("the push is not advertised, or waits behind acts")
	}
}

// Only the machine's own IPv4 addresses on a network count, in the order given. IPv4
// link-local above all: it is the address the machine gave itself when nobody answered, and
// reported as the VIP it would pass the node's own health probe while the LAN cannot reach it.
// It must not shadow a real address that follows it either.
func TestGlobalIPv4KeepsOnlyTheMachinesAddresses(t *testing.T) {
	cidr := func(s string) net.Addr {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	got := globalIPv4([]net.Addr{
		cidr("169.254.57.250/16"),
		cidr("192.168.9.50/24"),
		cidr("fe80::1/64"),
		cidr("2001:db8::5/64"),
		cidr("127.0.0.1/8"),
		cidr("10.0.0.2/24"),
		&net.IPAddr{IP: net.ParseIP("192.168.1.1")},
	})
	if want := []string{"192.168.9.50/24", "10.0.0.2/24"}; !reflect.DeepEqual(got, want) {
		t.Errorf("globalIPv4 = %v, want %v", got, want)
	}
	if got := globalIPv4(nil); got != nil {
		t.Errorf("no addresses = %v, want none", got)
	}
}

// The real executor answers what `ls -1` and `test -e` did: a directory's names sorted, without
// the dot-names; a missing path an error os.IsNotExist recognises, for a listing and a stat alike.
func TestTheOSExecutorListsAndStatsAsTheCommandsDid(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"b.json", ".hidden", "a.json"} {
		if err := os.WriteFile(dir+"/"+n, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(dir+"/c", 0o755); err != nil {
		t.Fatal(err)
	}
	x := NewOSExecutor()
	got, err := x.ListDir(dir)
	if want := []string{"a.json", "b.json", "c"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ListDir = %v, %v; want %v", got, err, want)
	}
	if _, err := x.ListDir(dir + "/nope"); !os.IsNotExist(err) {
		t.Errorf("ListDir of a missing directory = %v, want not-exist", err)
	}
	if fi, err := x.Stat(dir + "/c"); err != nil || !fi.IsDir() {
		t.Errorf("Stat of a directory = %v, %v", fi, err)
	}
	if _, err := x.Stat(dir + "/nope"); !os.IsNotExist(err) {
		t.Errorf("Stat of a missing path = %v, want not-exist", err)
	}
	if _, err := x.IPv4Addrs("briard-no-such-nic"); err == nil {
		t.Error("IPv4Addrs of a device that does not exist was not an error")
	}
}
