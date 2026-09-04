package main

// The dashboard's end of the host's admin port ([V3b.31i]), and the one thing it asks for.
//
// "Set up Home Assistant" is a host directive pressed from inside the guest. The dashboard opens
// the second virtio-serial port (shared/dashboard.AdminPortDev), writes the directive as one JSON
// line -- the protocol `briard` speaks to the host's admin socket -- and reads the outcome back.
// It relays; it decides nothing. The host answers through the same dispatch the CLI and the cloud
// go through, and accepts from this port only what stays inside the guest.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"briard.io/agent/hass"
	"briard.io/shared/api"
)

// adminPort relays one directive to the host and returns its outcome. The real one is the
// serial port; tests hand in a fake.
type adminPort interface {
	Submit(ctx context.Context, d api.Directive) (api.DirectiveOutcome, error)
}

// serialPort is the real port: opened per request, one request at a time (a serial line has no
// framing beyond the newline, and the host answers in order). Each request carries a fresh ID
// and the reader skips any line that is not its answer -- the host answers a stale half-line
// from a dashboard that died mid-write with an ID-less failure, on purpose.
type serialPort struct {
	path string
	// open is how the wire is reached: the character device by default; a test dials a socket.
	open func() (io.ReadWriteCloser, error)
	mu   sync.Mutex
}

// errNoAdminPort is a host that offers no port: an older host agent, or a guest not launched by
// briard's own qemu (the hermetic rigs). Surfaced as such; the CLI is the same request typed.
var errNoAdminPort = errors.New("this node's host offers no admin port")

func (p *serialPort) openDevice() (io.ReadWriteCloser, error) {
	f, err := os.OpenFile(p.path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNoAdminPort
		}
		return nil, err
	}
	return f, nil
}

func (p *serialPort) Submit(ctx context.Context, d api.Directive) (api.DirectiveOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	open := p.open
	if open == nil {
		open = p.openDevice
	}
	f, err := open()
	if err != nil {
		return api.DirectiveOutcome{}, err
	}
	defer f.Close()
	if d.ID == "" {
		id, err := newSecret()
		if err != nil {
			return api.DirectiveOutcome{}, err
		}
		d.ID = id[:16]
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return api.DirectiveOutcome{}, err
	}
	// A leading newline ends whatever a previous writer left unfinished on the line.
	if _, err := f.Write(append(append([]byte{'\n'}, raw...), '\n')); err != nil {
		return api.DirectiveOutcome{}, fmt.Errorf("write to the admin port: %w", err)
	}
	// The port is pollable, so the context can cut a read short; where it is not, the deadline
	// is refused and the read simply waits, as the CLI does.
	if dl, ok := f.(interface{ SetReadDeadline(time.Time) error }); ok {
		stop := context.AfterFunc(ctx, func() { _ = dl.SetReadDeadline(time.Now()) })
		defer stop()
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var o api.DirectiveOutcome
		if err := json.Unmarshal(sc.Bytes(), &o); err != nil || o.ID != d.ID {
			continue
		}
		return o, nil
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return api.DirectiveOutcome{}, ctx.Err()
		}
		return api.DirectiveOutcome{}, fmt.Errorf("read from the admin port: %w", err)
	}
	return api.DirectiveOutcome{}, errors.New("the admin port closed before the host answered")
}

// install is one service install the dashboard asked the host for, from the click until the
// routes table lists the service (or the host said no).
type install struct {
	Started time.Time
	Done    bool
	Failed  bool
	Detail  string
}

// installView is the card's view of it.
type installView struct {
	Running bool
	Failed  bool
	Detail  string
	Since   string
	// Progress is the pull so far ([V3b.31j]); nil when the host left no total to measure against.
	Progress *progressView
}

// requestInstall is the button: one install in flight per service, the directive relayed in
// the background because a pull runs minutes and a browser should not hold a request that
// long. The page shows the state and refreshes itself.
func (a *app) requestInstall(w http.ResponseWriter, r *http.Request, name string) {
	if name != hass.Name {
		http.Error(w, "the dashboard only sets up Home Assistant\n", http.StatusNotFound)
		return
	}
	a.mu.Lock()
	if cur, ok := a.installs[name]; ok && !cur.Done {
		a.mu.Unlock()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.installs[name] = &install{Started: a.now()}
	a.mu.Unlock()
	go func() {
		// The pull is bounded on the host ([B.56]); this is the ceiling on waiting for its answer.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
		defer cancel()
		o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveServiceInstall, Payload: name})
		a.mu.Lock()
		defer a.mu.Unlock()
		st := a.installs[name]
		st.Done = true
		switch {
		case err != nil:
			st.Failed, st.Detail = true, err.Error()
		case o.State != api.OutcomeDone:
			st.Failed, st.Detail = true, o.Detail
		}
		log.Printf("dashboard: install %s: outcome=%+v err=%v", name, o, err)
	}()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// installState is what the card shows for a service, nil when nothing was asked. A finished,
// successful install is forgotten once the routes table lists the service -- the card's
// ordinary states take over from there.
func (a *app) installState(name string, routed bool) *installView {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.installs[name]
	if !ok {
		return nil
	}
	if st.Done && !st.Failed && routed {
		delete(a.installs, name)
		return nil
	}
	v := &installView{Running: !st.Done, Failed: st.Failed, Detail: st.Detail, Since: st.Started.Format("15:04")}
	if v.Running {
		v.Progress = a.pullProgress(name)
	}
	return v
}
