package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"briard.io/agent/drbd"
	"briard.io/internal/testsock"
	"briard.io/shared/api"
	"briard.io/shared/backup"
)

// fakeExec records writes/runs and returns canned output; stands in for the guest.
// runFn, if set, overrides output/err (e.g. to evolve Status across polls).
type fakeExec struct {
	files    map[string]string
	runs     [][]string
	hostname string
	output   []byte
	err      error
	runFn    func(name string, args []string) ([]byte, error)
}

func (f *fakeExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.runs = append(f.runs, append([]string{name}, args...))
	if f.runFn != nil {
		return f.runFn(name, args)
	}
	return f.output, f.err
}

func (f *fakeExec) WriteFile(path string, data []byte) error {
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

// existingMetadata is a runFn simulating a data disk that already holds a DRBD replica (a
// rebooted node): `create-md` (no --force) refuses to overwrite -> non-zero, so Provision reads
// it as "attach, don't wipe". Everything else succeeds. Fresh-disk cases use the default
// fakeExec (all Run succeed -> create-md writes new metadata).
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

func TestProvisionWritesConfigsAndCreatesMD(t *testing.T) {
	f := &fakeExec{} // fresh disk: create-md (no --force) succeeds
	g := dial(t, f)
	req := ProvisionRequest{Resource: "r0", ResConfig: "RES", ReactorConfig: "REACTOR"}
	res, err := g.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.CreatedMetadata {
		t.Error("a fresh disk must report CreatedMetadata=true")
	}
	if f.files["/run/briard/drbd.d/r0.res"] != "RES" {
		t.Errorf(".res = %q", f.files["/run/briard/drbd.d/r0.res"])
	}
	if f.files["/run/briard/drbd-reactor.d/briard.toml"] != "REACTOR" {
		t.Errorf("reactor toml = %q", f.files["/run/briard/drbd-reactor.d/briard.toml"])
	}
	// Create-md is run WITHOUT --force -- it's the probe (and the create, on a fresh disk).
	want := [][]string{{"drbdadm", "create-md", "r0"}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v (create-md, no --force)", f.runs, want)
	}
}

// A node returning from a reboot already has valid metadata: create-md (no --force) refuses,
// so Provision reports no fresh create and does NOT wipe the persisted replica -- B.22b. The
// key safety property: --force is never passed, so even the probe can't overwrite.
func TestProvisionAttachesWhenMetadataExists(t *testing.T) {
	f := &fakeExec{runFn: existingMetadata} // create-md refuses (metadata present)
	g := dial(t, f)
	res, err := g.Provision(context.Background(), ProvisionRequest{Resource: "r0", ResConfig: "RES"})
	if err != nil {
		t.Fatal(err)
	}
	if res.CreatedMetadata {
		t.Error("existing metadata must not report a fresh create")
	}
	if len(f.runs) != 1 || !reflect.DeepEqual(f.runs[0], []string{"drbdadm", "create-md", "r0"}) {
		t.Errorf("runs = %v, want a single non-forced create-md (which refused, no wipe)", f.runs)
	}
}

// Adjust (runtime mesh growth) rewrites the .res with the new peer set and runs
// `drbdadm adjust` -- and CRUCIALLY never create-md's: the serving primary's disk is never
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

// A single node has no DRBD address yet (it replicates over loopback) -- ConfigureNet with an
// empty dev/CIDR must skip addressing entirely and ONLY record the VIP device (the unified layout:
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

// ConfigureNet addresses the system NIC (eth1): an idempotent `addr replace`, a read-back to see
// what the NIC now holds, then a link up -- all with an explicit `dev`. With no vipDev it writes
// no VIP file (the guest keeps its baked default). The NIC here holds only the wanted address, so
// the read-back prunes nothing; TestConfigureNetPrunesStaleAddress is the failable half.
func TestConfigureNet(t *testing.T) {
	f := &fakeExec{output: []byte("2: eth1    inet 10.0.0.2/24 scope global eth1\\       valid_lft forever")}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{Dev: "eth1", CIDR: "10.0.0.2/24", VIPDev: "", VIPAddr: ""}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"ip", "addr", "replace", "10.0.0.2/24", "dev", "eth1"},
		{"ip", "-o", "-4", "addr", "show", "dev", "eth1", "scope", "global"},
		{"ip", "link", "set", "dev", "eth1", "up"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
	if _, ok := f.files[vipEnvPath]; ok {
		t.Errorf("no vipDev -> must not write %s, got %q", vipEnvPath, f.files[vipEnvPath])
	}
}

// RENUMBERING: the NIC already holds an old address (the node's island subnet) and ConfigureNet is
// called with the new one (the adopter's -- DESIGN §1.2). `ip addr replace` alone would leave BOTH,
// so the stale one must be deleted, and the new one must be on the NIC before it goes.
//
// Failable by construction: with the prune removed this test sees two commands instead of four and
// no `addr del` at all. It is the only place that catches it, because on a node that never had an
// address -- every node before [V3b.26b] -- add-without-remove and add-with-remove are the same
// thing, and the whole existing suite agrees with both.
func TestConfigureNetPrunesStaleAddress(t *testing.T) {
	f := &fakeExec{output: []byte(
		"2: eth1    inet 10.0.0.2/24 scope global eth1\\       valid_lft forever\n" +
			"2: eth1    inet 10.9.1.7/24 scope global secondary eth1\\       valid_lft forever")}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{Dev: "eth1", CIDR: "10.0.0.2/24", VIPDev: "", VIPAddr: ""}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"ip", "addr", "replace", "10.0.0.2/24", "dev", "eth1"},
		{"ip", "-o", "-4", "addr", "show", "dev", "eth1", "scope", "global"},
		{"ip", "addr", "del", "10.9.1.7/24", "dev", "eth1"}, // the island address, gone
		{"ip", "link", "set", "dev", "eth1", "up"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v,\nwant %v", f.runs, want)
	}
}

// A self-assigned link-local is not ours: we never put it on, so we must not take it off. Failable
// -- dropping the 169.254 guard in allCIDRs adds an `addr del` here.
func TestConfigureNetLeavesLinkLocalAlone(t *testing.T) {
	f := &fakeExec{output: []byte(
		"2: eth1    inet 10.0.0.2/24 scope global eth1\\       valid_lft forever\n" +
			"2: eth1    inet 169.254.57.250/16 scope global eth1\\       valid_lft forever")}
	g := dial(t, f)
	if err := g.ConfigureNet(context.Background(), NetConfig{Dev: "eth1", CIDR: "10.0.0.2/24", VIPDev: "", VIPAddr: ""}); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "addr" && r[2] == "del" {
			t.Errorf("deleted a link-local address: %v", r)
		}
	}
}

// GCSnapshots keeps the newest N snapshots and btrfs-deletes the rest (oldest first).
func TestDataGC(t *testing.T) {
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if name == "ls" {
			return []byte("dummy-3\ndummy-1\n.snapshots\ndummy-5\ndummy-2\ndummy-4\n"), nil
		}
		return nil, nil
	}}
	g := dial(t, f)
	if err := g.GCSnapshots(context.Background(), "/s", "dummy-", 2); err != nil {
		t.Fatal(err)
	}
	var deletes []string
	for _, r := range f.runs {
		if len(r) >= 3 && r[0] == "btrfs" && r[1] == "subvolume" && r[2] == "delete" {
			deletes = append(deletes, r[3])
		}
	}
	want := []string{"/s/dummy-1", "/s/dummy-2", "/s/dummy-3"} // keep newest 2 (dummy-4,5)
	if !reflect.DeepEqual(deletes, want) {
		t.Errorf("deleted %v, want %v (oldest three)", deletes, want)
	}
}

// PayloadPin retags the serve image to the pinned ref (image must be staged) and
// records the ref on the volume so a failover converges to it.
func TestPayloadPin(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.PayloadPin(context.Background(), "briard-dummy:v1"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"podman", "image", "exists", "briard-dummy:v1"},
		{"podman", "tag", "briard-dummy:v1", "briard-payload:serve"},
		{"sync", "-f", "/var/lib/briard/.payload-image"}, // flush so the pin replicates before a failover
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
	if got := f.files["/var/lib/briard/.payload-image"]; got != "briard-dummy:v1\n" {
		t.Errorf("pin file = %q, want briard-dummy:v1", got)
	}
}

// PayloadImage reads the pin file (the served identity across a failover); a cat error
// (no pin set) reports "" so the host falls back to the baked default.
func TestPayloadImage(t *testing.T) {
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if name == "cat" && len(args) == 1 && args[0] == "/var/lib/briard/.payload-image" {
			return []byte("briard-dummy:v1\n"), nil
		}
		return nil, nil
	}}
	g := dial(t, f)
	got, err := g.PayloadImage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "briard-dummy:v1" {
		t.Errorf("PayloadImage = %q, want briard-dummy:v1 (trimmed pin)", got)
	}

	// No pin: cat errors -> "".
	f2 := &fakeExec{runFn: func(string, []string) ([]byte, error) { return nil, errors.New("No such file") }}
	if got, err := dial(t, f2).PayloadImage(context.Background()); err != nil || got != "" {
		t.Errorf("absent pin: got (%q,%v), want (\"\",nil)", got, err)
	}
}

// PayloadActiveSince reads the unit's ActiveEnterTimestampMonotonic (usec) as the
// adopt-not-bounce proof for the maintenance contract: unchanged across a pause/resume
// means the promoter re-adopted the running payload. An inactive unit ("") parses to 0.
func TestPayloadActiveSince(t *testing.T) {
	const unit = "podman-briard-payload.service"
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if name == "systemctl" && reflect.DeepEqual(args, []string{"show", "-p", "ActiveEnterTimestampMonotonic", "--value", unit}) {
			return []byte("123456789\n"), nil
		}
		return nil, nil
	}}
	if got, err := dial(t, f).PayloadActiveSince(context.Background(), unit); err != nil || got != 123456789 {
		t.Errorf("PayloadActiveSince = (%d,%v), want (123456789,nil)", got, err)
	}

	// Inactive unit: systemctl prints an empty value -> 0 (never entered active).
	f2 := &fakeExec{runFn: func(string, []string) ([]byte, error) { return []byte("\n"), nil }}
	if got, err := dial(t, f2).PayloadActiveSince(context.Background(), unit); err != nil || got != 0 {
		t.Errorf("inactive: got (%d,%v), want (0,nil)", got, err)
	}
}

// PayloadHealth is the in-guest readiness probe (macvtap-safe): the guest GETs the payload's
// health URL itself and reports 200. Exercises the real raw-net.Dial HTTP/1.0 path end to end
// (Client -> dispatch -> probeHTTPOK), since it deliberately avoids net/http in the guest binary.
func TestPayloadHealth(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer ok.Close()
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer sick.Close()

	f := &fakeExec{runFn: func(string, []string) ([]byte, error) { return nil, nil }} // health path doesn't shell out
	if got, err := dial(t, f).PayloadHealth(context.Background(), ok.URL); err != nil || !got {
		t.Errorf("200 payload: got (%v,%v), want (true,nil)", got, err)
	}
	if got, err := dial(t, f).PayloadHealth(context.Background(), sick.URL); err != nil || got {
		t.Errorf("503 payload: got (%v,%v), want (false,nil)", got, err)
	}
	// Unreachable endpoint -> not healthy, still no transport error (the probe swallows dial errors).
	if got, err := dial(t, f).PayloadHealth(context.Background(), "http://127.0.0.1:1/healthz"); err != nil || got {
		t.Errorf("unreachable: got (%v,%v), want (false,nil)", got, err)
	}
}

// Handshake negotiates the protocol: version + the advertised capability set, which the
// host checks with Supports.
func TestHandshake(t *testing.T) {
	g := dial(t, &fakeExec{})
	h, err := g.Handshake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != api.GuestProtocol {
		t.Errorf("version = %d, want %d", h.Version, api.GuestProtocol)
	}
	if g.ProtocolVersion() != api.GuestProtocol {
		t.Errorf("ProtocolVersion = %d, want %d", g.ProtocolVersion(), api.GuestProtocol)
	}
	if !g.Supports(verbOSSwitch) {
		t.Error("guest should advertise os.switch after the handshake")
	}
	if g.Supports("bogus.verb") {
		t.Error("guest must not advertise a bogus verb")
	}
}

// On a reconnect the still-open virtio-serial stream can carry a stale in-flight reply
// from the *dropped* session ahead of the handshake reply (QEMU keeps the guest port open
// across a host re-dial). Handshake must resync past such stale frames, not fail on
// the id mismatch -- otherwise a reconnect never recovers (the observed agent-bringup
// freeze/thaw failure).
func TestHandshakeResyncsPastStaleFrame(t *testing.T) {
	cc, sc := net.Pipe()
	g := NewClient(cc)
	t.Cleanup(func() { g.Close() })
	go func() {
		var req request
		if err := readFrame(sc, &req); err != nil { // read the verbHello request
			return
		}
		// A leftover reply from the previous session (an unrelated id), THEN the real
		// handshake reply. The host must skip the first and match the second.
		hello, _ := json.Marshal(api.GuestHello{Version: api.GuestProtocol, Capabilities: []string{verbHello}})
		_ = writeFrame(sc, response{ID: req.ID + 42, Payload: json.RawMessage(`"stale reply from a dropped session"`)})
		_ = writeFrame(sc, response{ID: req.ID, Payload: hello})
	}()
	h, err := g.Handshake(context.Background())
	if err != nil {
		t.Fatalf("Handshake must resync past a stale frame, got: %v", err)
	}
	if h.Version != api.GuestProtocol {
		t.Errorf("version = %d, want %d (resynced to the real reply)", h.Version, api.GuestProtocol)
	}
}

// The successor of an agent that died MID-CALL re-attaches to the same channel with the
// dead session's reply still in it, and the resync above separates the two sessions BY
// ID -- so the frame that decides it is the one every session has in common: the reply
// to its first request, the hello. Two real sessions over one BUFFERED stream, which is
// what QEMU's chardev is (the guest port stays open, so bytes outlive the host process
// that was going to read them). [V3b.17]
func TestHandshakeResyncsPastADeadSessionsHelloReply(t *testing.T) {
	host, guest := socketPair(t)
	taken := make(chan struct{}, 2) // buffered: session 2's signal is never read
	go serve(context.Background(), guest, func(_ context.Context, verb string, _ json.RawMessage) (any, error) {
		if verb != verbHello {
			return nil, nil
		}
		taken <- struct{}{} // the request is read; its reply is written next
		return api.GuestHello{Version: api.GuestProtocol, Capabilities: []string{verbHello, verbUp}}, nil
	})

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
	// The verb AFTER the handshake is where the field failure surfaced: the handshake had
	// matched the orphan and left the real reply for this call to trip over.
	if err := g.Up(context.Background(), "r0"); err != nil {
		t.Fatalf("first verb after a re-adopted handshake: %v", err)
	}
}

// deadReader is the host end of a session whose process was killed with a request on the
// wire: writes reach the guest, reads never return. Close is a no-op -- the CHANNEL
// outlives the process, which is the premise of the whole resync.
type deadReader struct {
	io.Writer
	blocked chan struct{}
}

func (d deadReader) Read([]byte) (int, error) { <-d.blocked; return 0, io.EOF }
func (d deadReader) Close() error             { return nil }

// socketPair connects two ends over a real unix socket. BUFFERED, unlike net.Pipe: a
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

// The version gate refuses a guest newer than the host knows or older than it supports --
// a safe deferral rather than driving a skewed guest.
func TestCompatibleGuest(t *testing.T) {
	if !compatibleGuest(api.GuestProtocol) {
		t.Error("the current protocol must be compatible")
	}
	if compatibleGuest(api.GuestProtocol + 1) {
		t.Error("a guest newer than the host must be refused")
	}
	if compatibleGuest(api.MinGuestProtocol - 1) {
		t.Error("a guest older than the host's minimum must be refused")
	}
}

// The pin is ref-agnostic: an immutable content digest works exactly like a
// name:tag -- the verb passes it through to podman verbatim, so the registry-pull future
// (pin by @sha256:digest) drops in with no agent change. v0's warm-standby uses local
// image IDs; both are content-addressed identities.
func TestPayloadPinAcceptsDigest(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	digest := "briard-dummy@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := g.PayloadPin(context.Background(), digest); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"podman", "image", "exists", digest},
		{"podman", "tag", digest, "briard-payload:serve"},
		{"sync", "-f", "/var/lib/briard/.payload-image"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want the digest passed through verbatim %v", f.runs, want)
	}
	if got := f.files["/var/lib/briard/.payload-image"]; got != digest+"\n" {
		t.Errorf("pin file = %q, want the digest %q", got, digest)
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
	// AND IT PERSISTS NOTHING, which is the same rule read the other way round.
	//
	// It used to write /etc/briard/node-id, because syscall.Sethostname does not survive a guest
	// reboot while the `.res` naming this node did -- so a rebooted guest ran as "guest" against
	// its own config saying `on n1`, drbd-reactor promoted into the mismatch at boot, and the node
	// parked quorate-but-never-Primary with no VIP and no address (V3.20).
	//
	// Two facts that must agree need one LIFETIME. V3.20 gave them one by making the NAME durable;
	// [V3b.16b] gives them one by making the `.res` EPHEMERAL, so both are re-derived from the host
	// at every bring-up and nothing can promote before that bring-up has happened ([V3b.16a]).
	// Asserting the ABSENCE is what stops the deleted file returning as a well-meant restore-at-
	// boot: a third copy on disk is a third thing that can be wrong.
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
	// SYSTEM_DEV rides along, and the VIP unit's STOP path is why ([V3b.26d]): a standby must
	// bring a DEDICATED service NIC down, because the flock MAC on it is shared with the peer and
	// a Secondary emitting from it teaches the switch the wrong port ([B.100]/[B.101]) -- while a
	// link-down on the DRBD NIC would take replication with it. The guest cannot tell those apart
	// from VIP_DEV alone, and it may not guess ([V3b.16a]).
	if got := f.files[vipEnvPath]; got != "VIP_DEV=eth2\nSYSTEM_DEV=eth1\n" {
		t.Errorf("%s = %q, want VIP_DEV=eth2 + SYSTEM_DEV=eth1", vipEnvPath, got)
	}
}

// A WITNESS claims no VIP, so it gets no file at all -- SYSTEM_DEV must not create one on its own.
// briard-vip.service takes vip.env as a REQUIRED EnvironmentFile, so a file written for a node
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

// The VIP ADDRESS is the LAN's, not ours: baking it made the product work only on the subnet our
// lab happens to use and fail GREEN everywhere else (V3.19). ConfigureNet must record it beside
// the device -- and RECORD rather than APPLY it, since the promoter chain claims the address on
// promotion and a guest that configured it here would hold the VIP while Secondary.
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

// An unset address must omit the line rather than write an empty one: the guest's baked fallback
// is what the agent-less harnesses run on, and a bare `VIP_ADDR=` would blank it and take
// `ip addr add` down with it.
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

// The flock's visible name goes to its own file and republishes the unit -- and touches NOTHING
// about addressing. That separation is the whole point of the three-way identifier split: a name
// is a label, an address is an identity, so a rename must never be an addressing call.
func TestSetMDNSNameWritesNameAndRepublishes(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.SetMDNSName(context.Background(), "brave-elf"); err != nil {
		t.Fatal(err)
	}
	if got := f.files[mdnsEnvPath]; got != "FLOCK_NAME=brave-elf\n" {
		t.Errorf("%s = %q, want FLOCK_NAME=brave-elf", mdnsEnvPath, got)
	}
	// try-restart, not restart: a Secondary publishes no name, and starting the unit there would
	// announce a name for an address this node does not hold.
	want := []string{"systemctl", "try-restart", mdnsUnit}
	if len(f.runs) != 1 || !reflect.DeepEqual(f.runs[0], want) {
		t.Errorf("runs = %v, want exactly one %v", f.runs, want)
	}
	// The VIP's own file must be untouched -- a rename that rewrote it would put every rename
	// through the addressing path this design exists to keep it out of.
	if _, touched := f.files[vipEnvPath]; touched {
		t.Errorf("a rename wrote %s; renaming must not touch addressing", vipEnvPath)
	}
}

// An empty name publishes NOTHING rather than `briard-.local`. Every agent-less harness and every
// node installed before the name existed sends "", and a guess is worse than silence.
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

// THE ASSERTION THIS FEATURE EXISTS FOR. avahi conflict-renames on a collision and tells nobody,
// so the name we asked for is not evidence of the name in force. MDNSPublished must report what
// was ESTABLISHED -- here `brave-elf-2`, after avahi bumped it -- and never echo the request.
// Reporting the request would rebuild V3.19 exactly: a name present, plausible, and not what
// anyone thinks it is.
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
// host asks every cycle, and the common case must not read as a fault.
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

// A diskless witness provisions the config but writes no reactor file and, having
// no metadata, runs no create-md.
func TestProvisionWitnessSkipsReactorAndCreateMD(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	req := ProvisionRequest{Resource: "r0", ResConfig: "RES", Diskless: true}
	res, err := g.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.CreatedMetadata {
		t.Error("a diskless witness has no metadata to create")
	}
	if _, ok := f.files["/run/briard/drbd-reactor.d/briard.toml"]; ok {
		t.Error("witness should not write a reactor config")
	}
	if len(f.runs) != 0 {
		t.Errorf("witness should run no dump-md/create-md, got %v", f.runs)
	}
}

func TestUpAndReactorStartCommands(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.Up(context.Background(), "r0"); err != nil {
		t.Fatal(err)
	}
	if err := g.ReactorStart(context.Background(), "r0"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"systemctl", "start", "drbd@r0.target"},
		{"systemctl", "start", "drbd-reactor.service"},
	}
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
	err := g.Up(context.Background(), "r0")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"systemctl start drbd@r0.target", "exit status 1", "no such target"} {
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

// A data node's bring-up: config + reactor written, then create-md -> target -> reactor.
func TestBringUpDataNode(t *testing.T) {
	f := &fakeExec{} // fresh disk: create-md succeeds
	g := dial(t, f)
	spec := BringUpSpec{
		Resource: demoResource(),
		Promoter: []string{"briard-data.service", "podman-briard-payload.service", "briard-vip.service"},
	}
	if err := g.BringUp(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if f.files["/run/briard/drbd.d/r0.res"] == "" {
		t.Error("BringUp wrote no .res")
	}
	if f.files["/run/briard/drbd-reactor.d/briard.toml"] == "" {
		t.Error("data node got no reactor config")
	}
	want := [][]string{
		{"drbdadm", "create-md", "r0"},
		{"systemctl", "start", "drbd@r0.target"},
		{"systemctl", "start", "drbd-reactor.service"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("bring-up sequence = %v, want %v", f.runs, want)
	}
}

// A revived data node (metadata already on disk): create-md refuses (no --force, so no wipe),
// so bring-up ATTACHES -- and, even though it's the seed, runs NO new-current-uuid (which would
// split-brain against the peer that kept serving) -- then resyncs from peers. This is what
// makes a node survive a reboot without wiping/re-seeding its replica.
func TestBringUpRestartAttachesWithoutWipe(t *testing.T) {
	f := &fakeExec{runFn: existingMetadata} // create-md refuses -> metadata present
	g := dial(t, f)
	spec := BringUpSpec{Resource: demoResource(), FreshInit: true, Promoter: []string{"briard-vip.service"}}
	if err := g.BringUp(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"drbdadm", "create-md", "r0"}, // attempted, refused (no --force) -> attach
		{"systemctl", "start", "drbd@r0.target"},
		{"systemctl", "start", "drbd-reactor.service"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("restart bring-up = %v, want %v (create-md refused, no new-current-uuid)", f.runs, want)
	}
}

// A witness's bring-up: config written, comes up, but no create-md and no reactor.
func TestBringUpWitness(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.BringUp(context.Background(), BringUpSpec{Resource: demoResource(), Diskless: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.files["/run/briard/drbd-reactor.d/briard.toml"]; ok {
		t.Error("witness should not get a reactor config")
	}
	want := [][]string{{"systemctl", "start", "drbd@r0.target"}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("witness bring-up = %v, want %v (no create-md, no reactor)", f.runs, want)
	}
}

// A fresh resource needs new-current-uuid (skip initial sync) between up and reactor.
func TestBringUpFreshInit(t *testing.T) {
	f := &fakeExec{} // fresh disk: create-md succeeds -> seed declares UpToDate
	g := dial(t, f)
	spec := BringUpSpec{Resource: demoResource(), FreshInit: true, Promoter: []string{"briard-vip.service"}}
	if err := g.BringUp(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"drbdadm", "create-md", "r0"},
		{"systemctl", "start", "drbd@r0.target"},
		{"drbdadm", "new-current-uuid", "--clear-bitmap", "r0/0"},
		{"systemctl", "start", "drbd-reactor.service"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("fresh-init bring-up = %v, want %v", f.runs, want)
	}
}

func TestPayloadStartStopCommands(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.PayloadStart(context.Background(), "podman-briard-payload.service"); err != nil {
		t.Fatal(err)
	}
	if err := g.PayloadStop(context.Background(), "podman-briard-payload.service"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"systemctl", "start", "podman-briard-payload.service"},
		{"systemctl", "--job-mode=ignore-dependencies", "stop", "podman-briard-payload.service"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

// Is-active prints the state word and exits non-zero when inactive; PayloadActive
// trusts the word, not the exit code.
func TestPayloadActiveReadsState(t *testing.T) {
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
		active, err := g.PayloadActive(context.Background(), "podman-briard-payload.service")
		if err != nil {
			t.Fatalf("PayloadActive(%q): %v", tc.out, err)
		}
		if active != tc.want {
			t.Errorf("PayloadActive(%q) = %v, want %v", tc.out, active, tc.want)
		}
	}
}

func TestDataSnapshotCommand(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.Snapshot(context.Background(), "/data/ha", "/data/ha/.snapshots/ha-1"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"btrfs", "subvolume", "snapshot", "-r", "/data/ha", "/data/ha/.snapshots/ha-1"}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

// Restore swaps the live subvolume for a fresh rw snapshot of the restore point.
func TestDataRestoreCommands(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.Restore(context.Background(), "/data/ha", "/data/ha/.snapshots/ha-1"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"btrfs", "subvolume", "delete", "/data/ha"},
		{"btrfs", "subvolume", "snapshot", "/data/ha/.snapshots/ha-1", "/data/ha"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

// A failed delete aborts restore before the (destructive) re-snapshot.
func TestDataRestoreStopsOnDeleteError(t *testing.T) {
	f := &fakeExec{err: errors.New("delete boom")}
	g := dial(t, f)
	if err := g.Restore(context.Background(), "/data/ha", "/snap"); err == nil {
		t.Fatal("expected error")
	}
	if len(f.runs) != 1 {
		t.Errorf("restore should stop after a failed delete, ran %v", f.runs)
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

// Os.stage realises the closure into the store. In production it passes no
// substituter options at all -- the guest uses the caches baked into its image
// (cache.nixos.org + cache.briard.io), so an --option here would mean the host had
// silently taken over a decision that belongs to the image.
func TestStageRealisesClosureFromBakedCaches(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	closure := "/nix/store/abc123-nixos-system-guest-26.05"
	if err := g.Stage(context.Background(), closure, StageSource{}); err != nil {
		t.Fatal(err)
	}
	// The trailing sync is load-bearing, not hygiene: nix registers the staged paths durably
	// but not their DATA, so without it a crash-consistent capture taken moments later (the
	// switch path's live snapshot, a power cut) restores a registered-but-torn closure that
	// nix will never re-fetch ([B.65]).
	want := [][]string{{"nix-store", "--realise", closure}, {"sync"}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v (no --option: the image's own substituters)", f.runs, want)
	}
}

// A StageSource overrides both the cache and the key it must be signed by, for that one
// call. The key travels with the URL deliberately: pointing a guest at a cache without
// saying whose signature to accept would either fail (the baked keys don't cover it) or
// tempt someone to relax require-sigs, which is the one thing this must never do.
func TestStageFromOverridesCacheAndKey(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	closure := "/nix/store/abc123-nixos-system-guest-26.05"
	src := StageSource{URL: "http://192.168.1.1:8080", Key: "briard-test-1:AAAA="}
	if err := g.Stage(context.Background(), closure, src); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{
		"nix-store", "--realise", closure,
		"--option", "substituters", src.URL,
		"--option", "trusted-public-keys", src.Key,
	}, {"sync"}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

// A failed realise must not sync: the sync exists to make a SUCCESSFUL stage durable, and
// running it after a failure would only blur whose error the caller sees.
func TestStageSkipsSyncWhenRealiseFails(t *testing.T) {
	f := &fakeExec{runFn: func(name string, _ []string) ([]byte, error) {
		if name == "nix-store" {
			return nil, errors.New("substituter unreachable")
		}
		return nil, nil
	}}
	g := dial(t, f)
	if err := g.Stage(context.Background(), "/nix/store/abc123-nixos-system-guest-26.05", StageSource{}); err == nil {
		t.Fatal("a failed realise must fail the stage")
	}
	for _, r := range f.runs {
		if r[0] == "sync" {
			t.Errorf("sync ran after a failed realise: %v", f.runs)
		}
	}
}

// An empty closure is refused before anything runs: `nix-store --realise` with no path
// exits 0 having done nothing, so without this guard a caller that lost its target would
// see a successful stage and go on to switch to "".
func TestStageRefusesEmptyClosure(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.Stage(context.Background(), "", StageSource{}); err == nil {
		t.Fatal("staging an empty closure must fail")
	}
	if len(f.runs) != 0 {
		t.Errorf("ran %v, want nothing", f.runs)
	}
}

// Os.stageboot makes a closure BOOTABLE without making it the default. The exact
// commands are the assertion, because two of them are easy to write in a way that looks
// identical and is wrong: the profile must be `staging` (not the system profile, which
// would hand over the default), and switch-to-configuration must be run from
// /run/current-system (not from the staged closure, whose copy passes ITSELF as
// install-grub.pl's default entry -- the `nixos-rebuild boot --profile-name` trap).
func TestStageBootArmsStagingWithoutMovingTheDefault(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	closure := "/nix/store/abc123-nixos-system-guest-26.05"
	if err := g.StageBoot(context.Background(), closure); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"test", "-e", closure},
		{"mkdir", "-p", "/nix/var/nix/profiles/system-profiles"},
		{"nix-env", "-p", "/nix/var/nix/profiles/system-profiles/staging", "--set", closure},
		{"/run/current-system/bin/switch-to-configuration", "boot"},
		{"sync"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v,\nwant %v", f.runs, want)
	}
}

// Os.poweroff must return BEFORE the machine goes down, or the reply races the
// shutdown and the host sees a dead channel -- indistinguishable from a guest that crashed,
// which is the one thing a clean-shutdown path must never look like. --no-block is what
// buys that, so it is asserted rather than assumed.
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

// Arming never fetches: a closure that is not already in the store is refused
// before the bootloader is touched, so a half-armed grub cannot outlive the mistake.
func TestStageBootRefusesUnstagedClosure(t *testing.T) {
	f := &fakeExec{runFn: func(name string, _ []string) ([]byte, error) {
		if name == "test" {
			return nil, errors.New("no such path")
		}
		return nil, nil
	}}
	g := dial(t, f)
	if err := g.StageBoot(context.Background(), "/nix/store/missing"); err == nil {
		t.Fatal("stageboot on an absent closure must fail")
	}
	if len(f.runs) != 1 {
		t.Errorf("ran %v after the presence check failed; want nothing further", f.runs[1:])
	}
}

// Os.components resolves the four boot-critical symlinks and reads kernel-params
// as a FILE. An empty closure means the BOOTED generation — not /run/current-system, which
// a switch-only update moves out from under the running kernel.
func TestComponentsReadsBootedByDefault(t *testing.T) {
	f := &fakeExec{output: []byte("/nix/store/xxx\n")}
	g := dial(t, f)
	if _, err := g.Components(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"readlink", "-f", "/run/booted-system/kernel"},
		{"readlink", "-f", "/run/booted-system/initrd"},
		{"readlink", "-f", "/run/booted-system/kernel-modules"},
		{"readlink", "-f", "/run/booted-system/systemd"},
		{"cat", "/run/booted-system/kernel-params"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v,\nwant %v", f.runs, want)
	}
}

// A named closure is read from that closure, and the values come back in the right fields —
// a transposition here would silently compare a kernel against an initrd and read as
// "changed", turning every update into a reboot.
func TestComponentsReadsNamedClosureIntoFields(t *testing.T) {
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if name == "cat" {
			return []byte("console=ttyS0 quiet\n"), nil
		}
		return []byte("/resolved" + args[len(args)-1] + "\n"), nil
	}}
	g := dial(t, f)
	c, err := g.Components(context.Background(), "/nix/store/tgt")
	if err != nil {
		t.Fatal(err)
	}
	want := SystemComponents{
		Kernel:        "/resolved/nix/store/tgt/kernel",
		Initrd:        "/resolved/nix/store/tgt/initrd",
		KernelModules: "/resolved/nix/store/tgt/kernel-modules",
		Systemd:       "/resolved/nix/store/tgt/systemd",
		KernelParams:  "console=ttyS0 quiet",
	}
	if c != want {
		t.Errorf("components = %+v,\nwant %+v", c, want)
	}
}

// An EMPTY kernel-params is damage, never a value: no bootable generation has an empty
// command line, but a closure whose data pages were lost to a crash-consistent capture
// reads exactly this way — registered, symlinks intact, file content gone ([B.65]: the
// restored rollback snapshot). Handing "" back as a diffable value routed a switch-only
// change down the reboot path, into the very generation whose files are torn.
func TestComponentsRefusesEmptyKernelParams(t *testing.T) {
	f := &fakeExec{runFn: func(name string, args []string) ([]byte, error) {
		if name == "cat" {
			return []byte("\n"), nil // torn file: exists, content gone
		}
		return []byte("/resolved" + args[len(args)-1] + "\n"), nil
	}}
	g := dial(t, f)
	if _, err := g.Components(context.Background(), "/nix/store/tgt"); err == nil {
		t.Fatal("an empty kernel-params read must fail, not diff")
	}
}

func TestSwitchSetsProfileAndActivates(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	closure := "/nix/store/def456-nixos-system-guest-25.05"
	if err := g.Switch(context.Background(), closure); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"test", "-e", closure},
		{"nix-env", "-p", "/nix/var/nix/profiles/system", "--set", closure},
		{closure + "/bin/switch-to-configuration", "switch"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v", f.runs, want)
	}
}

// A failed profile --set aborts before activation.
func TestSwitchStopsOnProfileError(t *testing.T) {
	f := &fakeExec{runFn: func(name string, _ []string) ([]byte, error) {
		if name == "nix-env" {
			return nil, errors.New("nix-env boom")
		}
		return nil, nil // the staged check passes; the profile set is what fails
	}}
	g := dial(t, f)
	if err := g.Switch(context.Background(), "/nix/store/x"); err == nil {
		t.Fatal("expected error")
	}
	if len(f.runs) != 2 { // the staged check, then the failed --set, then stop
		t.Errorf("switch should stop after a failed --set, ran %v", f.runs)
	}
}

// An unstaged closure is refused before anything is touched, and this guard is load-bearing
// rather than defensive: with substituters configured `nix-env --set` would happily
// SUBSTITUTE a missing closure, so a switch on the failover path could go to the network --
// breaking's "converge is select, never build" exactly where it cannot afford to wait.
// (It moved here from os.pin when the code pin was removed; the rule always belonged to
// switching rather than to recording.)
func TestSwitchRefusesUnstagedClosure(t *testing.T) {
	f := &fakeExec{runFn: func(name string, _ []string) ([]byte, error) {
		if name == "test" {
			return nil, errors.New("not found")
		}
		return nil, nil
	}}
	if err := dial(t, f).Switch(context.Background(), "/nix/store/missing"); err == nil {
		t.Fatal("expected a refusal for an unstaged closure")
	}
	for _, r := range f.runs {
		if len(r) > 0 && r[0] == "nix-env" {
			t.Errorf("must not touch the system profile for an unstaged closure, ran %v", f.runs)
		}
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
	// No maintenance marker: briard-converge is a switch-free gate, so there is nothing
	// autonomous to hold off during a managed op. Pause stops the daemon (services + Primary stay
	// up); resume restarts it (re-adopts, no demote).
	//
	// TWO COMMANDS, NOT FOUR, AND THE ABSENCE IS THE ASSERTION. Pause used to `rm` drbd-reactor's
	// `Before=` drop-in and daemon-reload first, to defuse the promote-vs-stop deadlock. That
	// defusal moved onto drbd-reactor.service's ExecStop ([B.85]), which also covers the stops
	// this verb never sees -- a shutdown, the deadman's reboot, a host reboot. The two ship in one
	// closure (the guest agent is built INTO the guest image), so a copy here would only be a
	// second place to keep right. If these commands come back, they came back for a reason that
	// needs writing down.
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

// BringUpGuest end-to-end over a REAL unix socket (the prod transport shape): a
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
	spec := BringUpSpec{Resource: demoResource(), Promoter: []string{"briard-vip.service"}}
	if err := BringUpGuest(context.Background(), sock, spec); err != nil {
		t.Fatal(err)
	}
	if f.files["/run/briard/drbd.d/r0.res"] == "" {
		t.Error("bring-up wrote no .res over the socket")
	}
}

// TestBackupSaveRestore drives the backup.save + backup.restore verbs end-to-end over
// the control channel: seal a .storage tree to an encrypted blob, wipe it, restore, and
// assert the config returns (the guest does the tar/age work locally; only the small
// verb crosses the pipe).
func TestBackupSaveRestore(t *testing.T) {
	g := dial(t, &fakeExec{}) // fake Run for the `sync` flush; backup does real file I/O

	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ".storage"), 0o755); err != nil {
		t.Fatal(err)
	}
	const sentinel = `{"entries":["briard_canary"]}`
	if err := os.WriteFile(filepath.Join(base, ".storage/core.config_entries"), []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := backup.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "home.age")

	ctx := context.Background()
	if err := g.BackupSave(ctx, base, []string{".storage"}, key.Recipient, dest); err != nil {
		t.Fatalf("BackupSave: %v", err)
	}
	blob, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("backup blob not written: %v", err)
	}
	if strings.Contains(string(blob), "briard_canary") {
		t.Fatal("plaintext leaked into the backup blob")
	}

	// Wipe the config, then restore from the blob.
	if err := os.RemoveAll(filepath.Join(base, ".storage")); err != nil {
		t.Fatal(err)
	}
	if err := g.BackupRestore(ctx, base, dest, key.Identity); err != nil {
		t.Fatalf("BackupRestore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(base, ".storage/core.config_entries"))
	if err != nil {
		t.Fatalf("config not restored: %v", err)
	}
	if string(got) != sentinel {
		t.Errorf("restored config = %q, want %q", got, sentinel)
	}
}

// Bring-up must put a runtime-installed service's units back BEFORE it starts the
// promoter. The units live on the guest's tmpfs, so a reboot erases them while the host's
// manifest cache survives — and drbd-reactor, started at the end of BringUp, would otherwise
// promote into a chain naming units that do not exist.
//
// The ordering is the assertion, not merely that both happened: a render that lands after
// ReactorStart is the bug with extra steps.
func TestBringUpRendersServiceUnitsBeforeStartingThePromoter(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)

	err := g.BringUp(context.Background(), BringUpSpec{
		Resource:      drbd.Resource{Name: "r0", Device: "/dev/drbd0", Peers: []drbd.Peer{{Name: "n1", Address: "10.0.0.1", NodeID: 0}}},
		Promoter:      []string{"briard-app.service"},
		ServiceUnits:  map[string]string{"briard-app.container": "[Container]\nImage=x\n"},
		ServiceImages: []string{"briard-app-img.service"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := f.files[quadletDir+"/briard-app.container"]; got == "" {
		t.Fatalf("the service unit was never written under %s: %v", quadletDir, f.files)
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
	warm := idx("systemctl", "start", "briard-app-img.service")
	reactor := idx("systemctl", "start", "drbd-reactor.service")
	if reload < 0 || warm < 0 || reactor < 0 {
		t.Fatalf("missing a step: daemon-reload=%d warm=%d reactor=%d (%v)", reload, warm, reactor, f.runs)
	}
	if !(reload < warm && warm < reactor) {
		t.Errorf("wrong order: daemon-reload=%d, warm=%d, reactor=%d — units and images must both precede the promoter", reload, warm, reactor)
	}
}

// ...and a node with no installed service renders nothing, so the shipped zero-service node is
// untouched by the above. Without this, "renders before the promoter" could be satisfied by
// rendering unconditionally, which would write an empty unit set on every ordinary bring-up.
func TestBringUpRendersNothingWithNoInstalledService(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)

	err := g.BringUp(context.Background(), BringUpSpec{
		Resource: drbd.Resource{Name: "r0", Device: "/dev/drbd0", Peers: []drbd.Peer{{Name: "n1", Address: "10.0.0.1", NodeID: 0}}},
		Promoter: []string{"briard-app.service"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for path := range f.files {
		if strings.HasPrefix(path, quadletDir+"/") {
			t.Errorf("a node with no installed service wrote a quadlet unit: %s", path)
		}
	}
}

// Os.gc must delete old profile GENERATIONS, not merely collect. Each
// system-N-link is itself a gcroot pinning a whole closure, so a bare nix-collect-garbage
// frees nothing however many have accumulated -- -d is what does the work, and asserting
// the exact argv is what keeps that from being silently dropped.
func TestCollectGarbageDeletesOldGenerations(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f)
	if err := g.CollectGarbage(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"nix-collect-garbage", "-d"}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("runs = %v, want %v -- without -d this collects nothing", f.runs, want)
	}
}

// Under DHCP the host stops choosing the VIP, so it has to be able to ask what the address turned
// out to be. Ground truth is the interface, and the answer for an unaddressed device is "" -- NOT
// an error, because the host asks every cycle and a Secondary holding no VIP is the normal case.
// An error there would read as a dead channel and trigger a reconnect every poll (V3.19c).
func TestVIPReadsTheLiveAddress(t *testing.T) {
	f := &fakeExec{output: []byte(
		"3: eth2    inet 192.168.9.50/24 brd 192.168.9.255 scope global eth2\\       valid_lft forever\n")}
	g := dial(t, f)
	got, err := g.VIP(context.Background(), "eth2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.9.50/24" {
		t.Errorf("VIP = %q, want 192.168.9.50/24", got)
	}
}

// A device with no global address (Secondary) answers "" and no error.
func TestVIPUnaddressedIsEmptyNotAnError(t *testing.T) {
	f := &fakeExec{}
	g := dial(t, f) // fakeExec returns nothing for the ip call
	got, err := g.VIP(context.Background(), "eth2")
	if err != nil {
		t.Fatalf("an unaddressed device must not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("VIP = %q, want empty", got)
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

// firstCIDR must take the field after "inet" and nothing else -- never a brd address, never a
// partially-parsed string. A wrong answer here is an address the host would probe and report.
func TestFirstCIDRParsesOnlyInet(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"3: eth2    inet 192.168.9.50/24 brd 192.168.9.255 scope global eth2", "192.168.9.50/24"},
		{"", ""},
		{"3: eth2    inet6 fe80::1/64 scope link", ""},
		{"garbage with no address at all", ""},

		// IPv4LL is NOT an address this node holds on anyone's behalf -- it is the one the
		// machine gave itself when nobody answered. Reporting it made the node probe its own
		// link-local address, pass, and publish HEALTHY while the LAN could not reach it
		// (measured: 169.254.57.250, with the DHCP server stopped). "No address" is the honest
		// answer, and it is the one that makes a data node read not-ready.
		{"3: eth2    inet 169.254.57.250/16 brd 169.254.255.255 scope global eth2", ""},
		// ...and it must not shadow a real address that follows it.
		{"3: eth2    inet 169.254.57.250/16 scope global eth2\n3: eth2    inet 192.168.9.50/24 scope global eth2", "192.168.9.50/24"},
	} {
		if got := firstCIDR(tc.in); got != tc.want {
			t.Errorf("firstCIDR(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The boot id is what lets the host tell a bounced in-guest agent from a rebooted guest, so the
// handshake has to carry it end to end -- read in the guest, over the wire, onto the Client
// ([B.102]).
func TestHandshakeReportsBootID(t *testing.T) {
	f := &fakeExec{files: map[string]string{bootIDPath: "0f9c2b1e-3d4a-4c5b-8e7f-1a2b3c4d5e6f\n"}}
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

// A guest that cannot produce a boot id must still HANDSHAKE. The handshake is what proves the
// channel is live and what the host refuses an incompatible guest on; failing it over a field
// no host has ever needed to drive a guest would turn a missing diagnostic into an unusable
// node. Empty then reads as "no evidence", which is what the host's reboot check requires.
func TestHandshakeWithoutBootIDStillSucceeds(t *testing.T) {
	g := dial(t, &fakeExec{}) // ReadFile of an unseeded path is os.ErrNotExist
	h, err := g.Handshake(context.Background())
	if err != nil {
		t.Fatalf("a guest with no readable boot id must still handshake: %v", err)
	}
	if h.BootID != "" || g.BootID() != "" {
		t.Errorf("boot id = %q/%q, want empty", h.BootID, g.BootID())
	}
	if h.Version != api.GuestProtocol {
		t.Errorf("version = %d, want %d", h.Version, api.GuestProtocol)
	}
}

// THE BRIDGE SUBSTRATE'S SERVICE IDENTITY: the host hands the guest one NIC, so the guest builds
// the second MAC itself ([V3b.26c]). Three properties, and each one is a defect if it goes.
func TestConfigureNetMakesTheServiceNIC(t *testing.T) {
	// `ip link show dev eth2` must FAIL for the create branch to be reached -- that is the
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
		// (1) NEVER brought up here. `ip link add` leaves the device down and that IS the standby
		// discipline: a Secondary holding the flock MAC up teaches the switch the wrong port for
		// the VIP ([B.100]/[B.101]). briard-vip.service owns the up, on promotion.
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
	// (3) The MAC is set AT CREATION. A macvlan comes up holding a kernel-random MAC, and a frame
	// emitted before a follow-up `link set address` teaches the switch a port for an address
	// nobody owns -- so an argv that sets it afterwards is a real, narrow defect.
	for i, a := range add {
		if a == "address" && i < len(add)-1 && add[i+1] == "52:54:00:ab:cd:ef" {
			return
		}
	}
	t.Errorf("the flock MAC is not set in the create call: %v", add)
}

// Under macvtap the host built the device, so the guest must create NOTHING -- the failable half
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

// An EXISTING service NIC has its MAC re-asserted rather than assumed. The flock MAC is
// flock-scoped, so an adoption changes it under a device that outlives the change (DESIGN §1.2);
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
