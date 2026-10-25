package host

import (
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"briard.io/agent/install"
	"briard.io/internal/testsock"
	"briard.io/shared/api"
	"briard.io/shared/notify"
)

func quiet(string, ...any) {}

// dialAdmin submits one directive over the admin socket and returns the outcome, the way the
// CLI does. Retries the dial briefly: serveLocal binds asynchronously.
func dialAdmin(t *testing.T, sock string, req any) api.DirectiveOutcome {
	t.Helper()
	var conn net.Conn
	var err error
	for i := 0; i < 100; i++ {
		if conn, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var o api.DirectiveOutcome
	if err := json.NewDecoder(conn).Decode(&o); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	return o
}

// TestServeLocalRoundTrip is the contract in one shot: a directive submitted on the socket
// reaches the observe loop verbatim, and the loop's outcome comes back to the caller.
func TestServeLocalRoundTrip(t *testing.T) {
	sock := filepath.Join(testsock.Dir(t), "admin.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := make(chan localRequest)
	go serveLocal(ctx, sock, "", reqs, quiet)

	// Stand in for the observe loop: apply one request, echo back a recognisable outcome.
	var got api.Directive
	go func() {
		rq := <-reqs
		got = rq.d
		rq.resp <- api.DirectiveOutcome{ID: rq.d.ID, State: api.OutcomeDone, Detail: "applied"}
	}()

	o := dialAdmin(t, sock, api.Directive{ID: "i1", Kind: api.DirectiveLog, Payload: "hello"})
	if o.State != api.OutcomeDone || o.Detail != "applied" || o.ID != "i1" {
		t.Fatalf("outcome = %+v, want done/applied/i1", o)
	}
	if got.Kind != api.DirectiveLog || got.Payload != "hello" || got.ID != "i1" {
		t.Fatalf("the loop saw %+v, want the directive as submitted", got)
	}
}

// TestServeLocalRejectsBeforeTheLoop pins the two refusals the door makes on its OWN, without
// bothering the observe loop. The assertion that matters is the negative one: a malformed or
// kindless request must never reach dispatch, because dispatch's unknown-kind path is a log line
// and an admin who typo'd a verb deserves an error, not a silent success.
func TestServeLocalRejectsBeforeTheLoop(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  any
		want string
	}{
		{"not json", "{{{not json", "bad request"},
		{"no kind", api.Directive{ID: "i1", Payload: "x"}, "directive has no kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := filepath.Join(testsock.Dir(t), "admin.sock")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reqs := make(chan localRequest, 1)
			go serveLocal(ctx, sock, "", reqs, quiet)

			// A stand-in observe loop that ALWAYS answers, so a regression that lets a bad
			// request through fails on the assertion below rather than hanging the suite on a
			// reply that never comes. A test whose failure mode is a stall is one CI reports
			// as a timeout ten minutes later, with no clue which invariant broke.
			reached := make(chan api.Directive, 1)
			go func() {
				select {
				case rq := <-reqs:
					reached <- rq.d
					rq.resp <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: "reached the loop"}
				case <-ctx.Done():
				}
			}()

			o := dialAdmin(t, sock, tc.req)
			if o.State != api.OutcomeFailed {
				t.Fatalf("state = %q, want %q", o.State, api.OutcomeFailed)
			}
			if !strings.Contains(o.Detail, tc.want) {
				t.Fatalf("detail = %q, want it to mention %q", o.Detail, tc.want)
			}
			select {
			case d := <-reached:
				t.Fatalf("a rejected request reached the observe loop: %+v", d)
			default:
			}
		})
	}
}

// TestServeLocalIsRootOnly: the door is 0600, so it is the agent's own uid (root on a real node)
// or nobody. An admin socket that any local user can drive is a privilege escalation wearing a
// convenience costume.
func TestServeLocalIsRootOnly(t *testing.T) {
	sock := filepath.Join(testsock.Dir(t), "admin.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveLocal(ctx, sock, "", make(chan localRequest), quiet)

	var st os.FileInfo
	var err error
	for i := 0; i < 100; i++ {
		if st, err = os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("stat %s: %v", sock, err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %04o, want 0600", perm)
	}
	if st.Mode()&fs.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (mode %v)", sock, st.Mode())
	}
}

// TestServeLocalReplacesAStaleSocket: an unclean stop leaves the socket file behind, and a bind
// onto an existing path fails. Without the unlink, a node would come back from a crash with no
// admin door — precisely the state in which someone wants one.
func TestServeLocalReplacesAStaleSocket(t *testing.T) {
	dir := testsock.Dir(t)
	sock := filepath.Join(dir, "admin.sock")
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("seed a stale socket: %v", err)
	}
	stale.Close()                       // leaves the file behind (Close unlinks only its own)
	if err := touch(sock); err != nil { // ensure the path exists either way
		t.Fatalf("touch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := make(chan localRequest)
	go serveLocal(ctx, sock, "", reqs, quiet)
	go func() {
		rq := <-reqs
		rq.resp <- api.DirectiveOutcome{State: api.OutcomeDone}
	}()
	if o := dialAdmin(t, sock, api.Directive{Kind: api.DirectiveNoop}); o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v, want the door to have rebound over the stale path", o)
	}
}

// TestServeLocalDisabled: "" is off, and off means nothing is created.
func TestServeLocalDisabled(t *testing.T) {
	dir := testsock.Dir(t)
	done := make(chan struct{})
	go func() { serveLocal(context.Background(), "", "", nil, quiet); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveLocal(\"\") did not return")
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 0 {
		t.Fatalf("a disabled door created something: %v %v", ents, err)
	}
}

func touch(p string) error {
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	return f.Close()
}

// The status directive answers from what the loop already holds, on each state the tray shows:
// serving, degraded by an open alert (a resolved one does not count), updating, and a node whose
// last tick was not healthy. A Config built by hand -- no lane, no store, no tick -- answers too.
func TestStatusAnswersFromTheLastTick(t *testing.T) {
	status := func(cfg Config, n notify.Notifier) LocalStatus {
		t.Helper()
		o := cfg.dispatch(context.Background(), api.Directive{ID: "s", Kind: api.DirectiveStatus}, originLocal, nil, nil, n, nil, nil, nil, quiet)
		if o.State != api.OutcomeDone || o.ID != "s" {
			t.Fatalf("status = %+v", o)
		}
		var st LocalStatus
		if err := json.Unmarshal([]byte(o.Detail), &st); err != nil {
			t.Fatalf("status detail %q: %v", o.Detail, err)
		}
		return st
	}
	if got := status(Config{}, nil); got != (LocalStatus{}) {
		t.Errorf("a bare Config = %+v, want the zero answer", got)
	}
	cfg := Config{healthy: new(atomic.Bool), acts: newActLane()}
	cfg.healthy.Store(true)
	s := testStore(t, nil)
	if got := status(cfg, s); got != (LocalStatus{Serving: true}) {
		t.Errorf("serving = %+v", got)
	}
	for _, a := range []notify.Alert{open("disk", "low"), open("clock", "off"), resolved("clock"), event("slept")} {
		if err := s.Notify(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	if got := status(cfg, s); got != (LocalStatus{Serving: true, Open: 1}) {
		t.Errorf("degraded = %+v, want one open", got)
	}
	for kind, updating := range map[string]bool{api.DirectiveAgentUpdate: true, api.DirectiveUpgradeSystem: true,
		install.DirectiveUpdateVM: true, api.DirectiveServiceInstall: false} {
		if _, ok := cfg.acts.claim(kind); !ok {
			t.Fatal("the lane is held")
		}
		if got := status(cfg, s); got.Updating != updating {
			t.Errorf("with %s in flight, updating = %t, want %t", kind, got.Updating, updating)
		}
		cfg.acts.release()
	}
	cfg.healthy.Store(false)
	if got := status(cfg, s); got.Serving {
		t.Errorf("an unhealthy tick reads as serving: %+v", got)
	}
}
