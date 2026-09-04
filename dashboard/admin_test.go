package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/routes"
)

// fakePort is the host's admin port as the dashboard sees it: it records what was asked and
// answers what the test told it to, after the test says so.
type fakePort struct {
	mu      sync.Mutex
	asked   []api.Directive
	answer  chan api.DirectiveOutcome
	fail    error
	noPort  bool
	waiting chan struct{}
}

func (p *fakePort) Submit(ctx context.Context, d api.Directive) (api.DirectiveOutcome, error) {
	p.mu.Lock()
	p.asked = append(p.asked, d)
	noPort, fail := p.noPort, p.fail
	p.mu.Unlock()
	if noPort {
		return api.DirectiveOutcome{}, errNoAdminPort
	}
	if fail != nil {
		return api.DirectiveOutcome{}, fail
	}
	select {
	case p.waiting <- struct{}{}:
	default:
	}
	select {
	case o := <-p.answer:
		return o, nil
	case <-ctx.Done():
		return api.DirectiveOutcome{}, ctx.Err()
	}
}

func newFakePort() *fakePort {
	return &fakePort{answer: make(chan api.DirectiveOutcome, 1), waiting: make(chan struct{}, 1)}
}

// "SET UP HOME ASSISTANT" ([V3b.31i]): the button relays exactly one service-install directive
// to the host and nothing else; while the host works the page says so and polls itself; a second
// click while it runs asks nothing more; a refusal from the host is shown with its reason and the
// CLI; success is forgotten the moment the routes table lists the service, and the ordinary card
// takes over.
func TestSetUpHomeAssistantRelaysOneInstallToTheHost(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	must(t, os.Remove(filepath.Join(r.dir, "routes.json"))) // nothing installed yet
	port := newFakePort()
	r.app.port = port
	body := r.page(c)
	if !strings.Contains(body, `action="/install/home-assistant"`) || !strings.Contains(body, "Set up Home Assistant") || strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("the not-installed card offers no button, or the idle page polls: %s", body)
	}
	// Untrusted cannot press it, and pressing for anything but HA is refused.
	if resp := r.do("POST", "/install/home-assistant", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("install with no session = %d, want 401", resp.StatusCode)
	}
	if resp := r.do("POST", "/install/mosquitto", c, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("install of another service = %d, want 404", resp.StatusCode)
	}
	if resp := r.do("POST", "/install/home-assistant", c, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("press = %d, want 303", resp.StatusCode)
	}
	select {
	case <-port.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the directive never reached the port")
	}
	body = r.page(c)
	if !strings.Contains(body, "Installing") || !strings.Contains(body, `http-equiv="refresh"`) || strings.Contains(body, "Set up Home Assistant") {
		t.Errorf("the page while installing does not say so and poll: %s", body)
	}
	// A second press while it runs relays nothing more.
	if resp := r.do("POST", "/install/home-assistant", c, nil); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("second press = %d", resp.StatusCode)
	}
	port.mu.Lock()
	asked := append([]api.Directive(nil), port.asked...)
	port.mu.Unlock()
	if len(asked) != 1 || asked[0].Kind != api.DirectiveServiceInstall || asked[0].Payload != "home-assistant" {
		t.Fatalf("the host was asked %+v; want exactly one service-install of home-assistant", asked)
	}
	// The host says no: the reason and the CLI are on the page, and the button is back.
	port.answer <- api.DirectiveOutcome{ID: asked[0].ID, State: api.OutcomeFailed, Detail: "catalog: no such service"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		body = r.page(c)
		if strings.Contains(body, "Could not install") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, "catalog: no such service") || !strings.Contains(body, "sudo briard service install home-assistant") || !strings.Contains(body, "Try again") || strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("a refused install is not surfaced with its reason, the CLI and a retry, or the page still polls: %s", body)
	}
	// Try again, and this time the host does it: the routes table lists HA, the ordinary card
	// (Starting… / the button gated on RUNNING) takes over and the install is forgotten.
	if resp := r.do("POST", "/install/home-assistant", c, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("retry = %d", resp.StatusCode)
	}
	<-port.waiting
	tbl := routes.Table{Services: []routes.Service{{
		Name: "home-assistant", Hosts: []string{"briard-brave-elf-home-assistant.local"},
		Address: r.haURL.Hostname(), Health: "http://:" + r.haURL.Port() + "/manifest.json",
		Routes: []routes.Route{{Listen: routes.ListenName, To: "http://:" + r.haURL.Port()}},
	}}}
	raw, _ := json.Marshal(tbl)
	must(t, os.WriteFile(filepath.Join(r.dir, "routes.json"), raw, 0o644))
	port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: "installed"}
	deadline = time.Now().Add(5 * time.Second)
	for {
		body = r.page(c)
		if strings.Contains(body, "Open Home Assistant") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, "Open Home Assistant") || strings.Contains(body, "Installing") || strings.Contains(body, "Could not install") {
		t.Errorf("after a successful install the ordinary card did not take over: %s", body)
	}
	r.app.mu.Lock()
	left := len(r.app.installs)
	r.app.mu.Unlock()
	if left != 0 {
		t.Errorf("%d install records kept after the service was routed", left)
	}
	// A host with no admin port (an older host, a rig that is not briard's qemu) is surfaced as
	// such, with the CLI.
	must(t, os.Remove(filepath.Join(r.dir, "routes.json")))
	port.mu.Lock()
	port.noPort = true
	port.mu.Unlock()
	r.do("POST", "/install/home-assistant", c, nil)
	deadline = time.Now().Add(5 * time.Second)
	for {
		body = r.page(c)
		if strings.Contains(body, "no admin port") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, "no admin port") || !strings.Contains(body, "sudo briard service install home-assistant") {
		t.Errorf("a host without the port is not surfaced: %s", body)
	}
}

// THE SERIAL PORT ITSELF, with a unix socket standing in for the character device: one JSON
// line out, the answer with the matching ID back, and lines that are not the answer (an ID-less
// failure for someone else's half-line, another request's outcome) skipped.
func TestSerialPortSubmitMatchesTheOutcomeByID(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "port")
	ln, err := net.Listen("unix", sock)
	must(t, err)
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		rd := bufio.NewReader(conn)
		for {
			line, err := rd.ReadBytes('\n')
			if err != nil {
				return
			}
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			var d api.Directive
			if json.Unmarshal(line, &d) != nil {
				continue
			}
			// Noise first, then the answer.
			json.NewEncoder(conn).Encode(api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "bad request: half a line"})
			json.NewEncoder(conn).Encode(api.DirectiveOutcome{ID: "someone-else", State: api.OutcomeDone})
			json.NewEncoder(conn).Encode(api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "did " + d.Payload})
		}
	}()
	// The device is a character special file the test cannot fake; the wire it carries is
	// what the claim is about, so the port is opened onto the socket instead.
	p := &serialPort{open: func() (io.ReadWriteCloser, error) { return net.Dial("unix", sock) }}
	got, err := p.Submit(context.Background(), api.Directive{Kind: api.DirectiveServiceInstall, Payload: "home-assistant"})
	must(t, err)
	if got.State != api.OutcomeDone || got.Detail != "did home-assistant" || got.ID == "" {
		t.Errorf("outcome = %+v; want the matching answer, with the ID Submit minted", got)
	}
	// The context cuts a read short: a host that never answers does not hold the caller forever.
	silent := filepath.Join(t.TempDir(), "silent")
	sl, err := net.Listen("unix", silent)
	must(t, err)
	defer sl.Close()
	go func() {
		c, _ := sl.Accept()
		if c != nil {
			<-time.After(10 * time.Second)
			c.Close()
		}
	}()
	quiet := &serialPort{open: func() (io.ReadWriteCloser, error) { return net.Dial("unix", silent) }}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := quiet.Submit(ctx, api.Directive{Kind: api.DirectiveServiceInstall}); err == nil || ctx.Err() == nil {
		t.Errorf("Submit against a silent host returned %v before the context ended", err)
	}
	// And the real device path when it does not exist: the no-port error, by name.
	absent := &serialPort{path: filepath.Join(t.TempDir(), "absent")}
	if _, err := absent.Submit(context.Background(), api.Directive{Kind: api.DirectiveServiceInstall}); err != errNoAdminPort {
		t.Errorf("Submit on an absent port = %v; want errNoAdminPort", err)
	}
}
