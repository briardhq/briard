package main

// The dashboard's end of the host's admin port, and the one thing it asks for.
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
	"slices"
	"strings"
	"sync"
	"time"

	"briard.io/agent/files"
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

// install is one app install the dashboard asked the host for, from the click until the
// routes table lists the app (or the host said no). Update: it was the Update button on an
// installed app -- the same directive, since an update is an install over what runs, and the
// card says "updating" instead of turning back into a chooser.
type install struct {
	Started time.Time
	Update  bool
	Done    bool
	Failed  bool
	Detail  string
	// Set is the act this install is part of, in the host's order (requestInstallSet); nil for
	// one app alone.
	Set []string
}

// installView is the card's view of it. Updated: an update finished well while the host's view
// still offers it -- the offer is hidden until the host's next push takes it away. Waiting: it is
// in a set, behind an app still on its way.
type installView struct {
	Running bool
	Update  bool
	Updated bool
	Failed  bool
	Waiting bool
	Detail  string
	Since   string
	// Progress is the pull so far; nil when the host left no total to measure against.
	Progress *progressView
	// Next names the apps waiting behind this one, for the card of the one on its way.
	Next string
}

// requestInstall is the Install button, and with update the Update button: one install in
// flight per app, the directive relayed in the background because a pull runs minutes and
// a browser should not hold a request that long. The page shows the state and refreshes itself.
// An update is asked only of an app the host's view offers one for; the host decides again.
func (a *app) requestInstall(w http.ResponseWriter, r *http.Request, name string, update bool) {
	switch {
	case update && !a.offered(name):
		http.Error(w, "the catalog offers no update for that app\n", http.StatusNotFound)
		return
	case !update && !a.installable(name):
		http.Error(w, "the catalog has no manifest for that app\n", http.StatusNotFound)
		return
	}
	a.mu.Lock()
	if cur, ok := a.installs[name]; ok && !cur.Done {
		a.mu.Unlock()
		http.Redirect(w, r, back(r), http.StatusSeeOther)
		return
	}
	a.installs[name] = &install{Started: a.now(), Update: update}
	a.mu.Unlock()
	go func() {
		// The pull is bounded on the host; this is the ceiling on waiting for its answer.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
		defer cancel()
		o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveAppInstall, Payload: name})
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
		log.Printf("dashboard: install %s (update=%v): outcome=%+v err=%v", name, update, o, err)
	}()
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

// requestInstallSet is the chooser's one Install button: every app it ticked, as ONE directive the
// host installs in order -- one act, because a second act while one runs is refused "busy", and a
// queue here would be a second place holding the household's intent that dies with this process.
// Files goes first when it is ticked: it is small, so something works within seconds.
func (a *app) requestInstallSet(w http.ResponseWriter, r *http.Request, picked []string) {
	var names []string
	for _, n := range picked {
		if a.installable(n) && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		http.Error(w, "pick at least one app the catalog has a manifest for\n", http.StatusBadRequest)
		return
	}
	slices.SortStableFunc(names, func(x, y string) int {
		switch {
		case x == files.Name && y != files.Name:
			return -1
		case y == files.Name && x != files.Name:
			return 1
		}
		return 0
	})
	a.mu.Lock()
	for _, n := range names {
		if cur, ok := a.installs[n]; ok && !cur.Done {
			a.mu.Unlock()
			http.Redirect(w, r, back(r), http.StatusSeeOther)
			return
		}
	}
	for _, n := range names {
		a.installs[n] = &install{Started: a.now(), Set: names}
	}
	a.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
		defer cancel()
		o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveAppInstall, Payload: strings.Join(names, " ")})
		a.mu.Lock()
		defer a.mu.Unlock()
		// The host names the one that failed after those it installed ("installed a, b; c: why"):
		// that one says why, the ones before it are done, and the ones after were never tried.
		failed, why := "", ""
		switch {
		case err != nil:
			failed, why = names[0], err.Error()
		case o.State != api.OutcomeDone:
			rest := o.Detail
			if i := strings.Index(rest, "; "); strings.HasPrefix(rest, "installed ") && i >= 0 {
				rest = rest[i+2:]
			}
			failed, why = names[0], o.Detail
			for _, n := range names {
				if strings.HasPrefix(rest, n+": ") {
					failed, why = n, strings.TrimPrefix(rest, n+": ")
				}
			}
		}
		tried := true
		for _, n := range names {
			st := a.installs[n]
			switch {
			case !tried:
				delete(a.installs, n)
			case n == failed:
				st.Done, st.Failed, st.Detail = true, true, why
				tried = false
			default:
				st.Done = true
			}
		}
		log.Printf("dashboard: install %v: outcome=%+v err=%v", names, o, err)
	}()
	http.Redirect(w, r, back(r), http.StatusSeeOther)
}

// installState is what the card shows for an app, nil when nothing was asked. A finished,
// successful install is forgotten once the routes table lists the app, and a successful
// update once the host's view no longer offers it -- the card's ordinary states take over from
// there.
func (a *app) installState(name string, routed, offered bool) *installView {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.installs[name]
	if !ok {
		return nil
	}
	if st.Done && !st.Failed && routed && !(st.Update && offered) {
		delete(a.installs, name)
		return nil
	}
	v := &installView{Running: !st.Done, Update: st.Update, Updated: st.Done && !st.Failed && st.Update,
		Failed: st.Failed, Detail: st.Detail, Since: st.Started.In(zone()).Format("15:04")}
	if v.Running && st.Set != nil {
		// THE HOST INSTALLS A SET IN ITS ORDER, so the table says where it is: a member it lists
		// is installed (its own card shows it), and of the rest the first is on its way and the
		// others wait their turn. The end of the act settles each as done or failed.
		if routed {
			return nil
		}
		routedNow := routedNames(a.routesPath)
		for _, n := range st.Set {
			if n == name {
				break
			}
			if !slices.Contains(routedNow, n) {
				v.Waiting = true
			}
		}
	}
	if v.Running && !v.Waiting {
		v.Progress = a.pullProgress(name)
	}
	return v
}
