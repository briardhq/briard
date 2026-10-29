package main

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"briard.io/agent/host"
)

// leaseRefused is the agent's refusal as the status carries it: the lease's reason, then its fix.
const leaseRefused = "your router leases Briard nothing of its own: it offered 192.168.7.144, the address of the machine Briard runs on; " +
	"pick a free address outside your router's DHCP range and run `briard config set vip <address>/24` in a terminal opened as administrator"

// Every reading the poll can make, and what the icon shows for it.
func TestShow(t *testing.T) {
	refused := fmt.Errorf("%w: %w", host.ErrNoAgent, fs.ErrPermission)
	absent := fmt.Errorf("%w: no such file", host.ErrNoAgent)
	for _, tc := range []struct {
		name string
		r    reading
		want view
	}{
		{"serving", reading{status: host.LocalStatus{Serving: true}},
			view{state: serving, tip: "Briard is running", action: "stop"}},
		{"an open alert", reading{status: host.LocalStatus{Serving: true, Open: 1}},
			view{state: degraded, tip: "1 alert needs your attention", action: "stop", open: 1}},
		{"two", reading{status: host.LocalStatus{Open: 2}},
			view{state: degraded, tip: "2 alerts need your attention", action: "stop", open: 2}},
		{"not serving", reading{status: host.LocalStatus{}},
			view{state: degraded, tip: "Briard is not serving right now", action: "stop"}},
		{"updating", reading{status: host.LocalStatus{Serving: true, Updating: true, Open: 1}},
			view{state: working, tip: "Briard is updating", action: "stop", open: 1}},
		{"no address", reading{status: host.LocalStatus{Open: 1, Refused: leaseRefused}},
			view{state: degraded, tip: "Briard cannot take an address on your network", action: "stop", open: 1}},
		{"refused", reading{refusal: "this node has no usable network device: no wired or wireless adapter is up"},
			view{state: degraded, tip: "this node has no usable network device: no wired or wireless adapter is up", action: "stop"}},
		{"stopped", reading{err: absent, down: true},
			view{state: stopped, tip: "Briard is stopped", action: "start"}},
		{"stopped to be updated", reading{err: absent, down: true, update: true},
			view{state: working, tip: "Briard is updating"}},
		{"starting, no door yet", reading{err: absent},
			view{state: working, tip: "Briard is starting"}},
		{"bringing its VM up", reading{err: context.DeadlineExceeded},
			view{state: working, tip: "Briard is starting"}},
		{"another account's", reading{err: refused, down: true},
			view{state: stopped, tip: "Briard was installed by another account on this computer"}},
		{"stop asked, still up", reading{status: host.LocalStatus{Serving: true}, pending: "stop"},
			view{state: working, tip: "Briard is stopping"}},
		{"stop asked, door gone", reading{err: absent, pending: "stop"},
			view{state: working, tip: "Briard is stopping"}},
		{"stop landed", reading{err: absent, down: true, pending: "stop"},
			view{state: stopped, tip: "Briard is stopped", action: "start"}},
		{"start asked", reading{err: absent, down: true, pending: "start"},
			view{state: working, tip: "Briard is starting"}},
		{"start landed", reading{status: host.LocalStatus{Serving: true}, pending: "start"},
			view{state: serving, tip: "Briard is running", action: "stop"}},
	} {
		if got := show(tc.r); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A toast is more alerts open than the last answer said -- never the first answer, never a poll
// that got none, never fewer.
func TestToast(t *testing.T) {
	one := &host.LocalStatus{Open: 1}
	for _, tc := range []struct {
		name string
		prev *host.LocalStatus
		r    reading
		want bool
	}{
		{"first answer", nil, reading{status: host.LocalStatus{Open: 3}}, false},
		{"a new one", one, reading{status: host.LocalStatus{Open: 2}}, true},
		{"same", one, reading{status: host.LocalStatus{Open: 1}}, false},
		{"resolved", one, reading{status: host.LocalStatus{}}, false},
		{"no answer", one, reading{err: host.ErrNoAgent, status: host.LocalStatus{Open: 5}}, false},
	} {
		if got := toast(tc.prev, tc.r); got != tc.want {
			t.Errorf("%s: toast = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// A Start menu click, against every reading the poll can make: the page once Briard serves,
// whatever else is open; one start of a cleanly stopped Briard, and the restart note when that
// start ends in a clean stop; the other account's refusal at once; and patience for everything
// in between, up to the bound.
func TestLaunch(t *testing.T) {
	refused := fmt.Errorf("%w: %w", host.ErrNoAgent, fs.ErrPermission)
	absent := fmt.Errorf("%w: no such file", host.ErrNoAgent)
	stopped := reading{err: absent, down: true}
	for _, tc := range []struct {
		name    string
		r       reading
		started bool
		waited  time.Duration
		want    step
		says    string
	}{
		{"serving", reading{status: host.LocalStatus{Serving: true}}, false, 0, openPage, ""},
		{"serving with an alert open", reading{status: host.LocalStatus{Serving: true, Open: 2}}, true, time.Minute, openPage, ""},
		{"no address, at once", reading{status: host.LocalStatus{Open: 1, Refused: leaseRefused}}, true, 30 * time.Second, failWith, "cannot serve your home: your router leases Briard nothing"},
		{"no address, with the fix", reading{status: host.LocalStatus{Open: 1, Refused: leaseRefused}}, false, 0, failWith, "config set vip"},
		{"stopped", stopped, false, 0, startIt, ""},
		{"stopped again after this click's start", stopped, true, 5 * time.Second, failWith, "Restart Windows"},
		{"another account's", reading{err: refused}, false, 0, failWith, "another account"},
		{"starting, not answering yet", reading{err: absent}, true, time.Minute, waitFor, ""},
		{"the VM coming up", reading{status: host.LocalStatus{}}, true, 2 * time.Minute, waitFor, ""},
		{"finishing its install", reading{refusal: "briard is finishing its install: fetching the guest image"}, true, 5 * time.Minute, waitFor, ""},
		{"an update swapping it", reading{err: absent, down: true, update: true}, false, time.Minute, waitFor, ""},
		{"still finishing at the bound", reading{refusal: "briard is finishing its install: fetching the guest image"}, true, launchBound + time.Second, failWith, "finishing its install"},
	} {
		got, says := launch(tc.r, tc.started, tc.waited)
		if got != tc.want || !strings.Contains(says, tc.says) || (tc.says == "") != (says == "") {
			t.Errorf("%s: launch = %v %q; want %v saying %q", tc.name, got, says, tc.want, tc.says)
		}
	}
}
