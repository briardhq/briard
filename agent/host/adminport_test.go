package host

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/shared/api"
)

// THE GUEST'S ADMIN PORT ([V3b.31i]), with a unix socket standing in for qemu's host end and a
// test standing in for the dashboard: an install rides the same channel as the local door and
// comes back with the loop's outcome; anything else the guest asks for is refused by name
// without reaching the loop; a half-line from a dead writer is answered with an ID-less failure
// and does not poison the next request.
func TestAdminPortRelaysOnlyInGuestDirectives(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := make(chan localRequest)
	var logs []string
	go serveAdminPort(ctx, sock, reqs, func(f string, a ...any) { logs = append(logs, strings.TrimSpace(f)) })
	// The observe loop, as far as this test is concerned: answer every install with Done.
	go func() {
		for rq := range reqs {
			rq.resp <- api.DirectiveOutcome{ID: rq.d.ID, State: api.OutcomeDone, Detail: "installed " + rq.d.Payload}
		}
	}()
	guest, err := ln.Accept() // qemu's guest end, as the dashboard would write to it
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	rd := bufio.NewReader(guest)
	ask := func(line string) api.DirectiveOutcome {
		t.Helper()
		if _, err := guest.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		_ = guest.SetReadDeadline(time.Now().Add(5 * time.Second))
		raw, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatalf("no outcome for %q: %v", line, err)
		}
		var o api.DirectiveOutcome
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatalf("outcome %q does not parse: %v", raw, err)
		}
		return o
	}
	if o := ask(`{"id":"a1","kind":"service-install","payload":"home-assistant"}` + "\n"); o.ID != "a1" || o.State != api.OutcomeDone || o.Detail != "installed home-assistant" {
		t.Errorf("install outcome = %+v", o)
	}
	for _, kind := range []string{api.DirectiveHandover, "upgrade", api.DirectiveSync, ""} {
		o := ask(`{"id":"x","kind":"` + kind + `"}` + "\n")
		if o.ID != "x" || o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "may not ask") {
			t.Errorf("%q from the guest = %+v; want refused by name", kind, o)
		}
	}
	// A dead writer's half-line, then a real request on the same wire: the half-line gets an
	// ID-less failure, the request its own outcome.
	if o := ask(`{"id":"half","kind":"serv` + "\n"); o.ID != "" || o.State != api.OutcomeFailed {
		t.Errorf("half-line = %+v; want an ID-less failure", o)
	}
	if o := ask(`{"id":"a2","kind":"service-prewarm","payload":"mosquitto"}` + "\n"); o.ID != "a2" || o.State != api.OutcomeDone {
		t.Errorf("prewarm after the half-line = %+v", o)
	}
	// The wire drops (the guest rebooted): the agent reconnects to the next qemu end.
	guest.Close()
	guest2, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer guest2.Close()
	rd = bufio.NewReader(guest2)
	guest = guest2
	if o := ask(`{"id":"a3","kind":"service-install","payload":"home-assistant"}` + "\n"); o.ID != "a3" || o.State != api.OutcomeDone {
		t.Errorf("install after a reconnect = %+v", o)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "refused a") || !strings.Contains(joined, "submitted by the guest's dashboard") {
		t.Errorf("the port did not log what it refused and what it relayed:\n%s", joined)
	}
}

// No port configured is a launch without a dashboard: nothing is dialled and nothing blocks.
func TestAdminPortDisabledWhenUnset(t *testing.T) {
	done := make(chan struct{})
	go func() {
		serveAdminPort(context.Background(), "", make(chan localRequest), func(string, ...any) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serveAdminPort with no socket did not return")
	}
}
