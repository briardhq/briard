package host

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
)

type fakeHandoff struct {
	got dashboard.Handoff
	err error
}

func (f *fakeHandoff) DashboardHandoff(_ context.Context, h dashboard.Handoff) error {
	f.got = h
	return f.err
}

func TestDashboardMintsACodeAndReportsTheURL(t *testing.T) {
	g := &fakeHandoff{}
	cfg := Config{FlockName: "brave-elf"}
	o := cfg.applyDashboard(context.Background(), g, api.Directive{ID: "d1", Kind: api.DirectiveDashboard, Payload: `{"name":"Kostas","username":"kostas","language":"el"}`}, nil, t.Logf)
	if o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v", o)
	}
	if len(g.got.Code) != 64 || g.got.Issued.IsZero() {
		t.Errorf("handoff = %+v; want a 32-byte hex code with an issue time", g.got)
	}
	if g.got.Name != "Kostas" || g.got.Username != "kostas" || g.got.Language != "el" {
		t.Errorf("account not carried: %+v", g.got)
	}
	want := "http://briard.local/?code=" + g.got.Code
	if o.Detail != want {
		t.Errorf("Detail = %q, want %q", o.Detail, want)
	}
	// Twice is two codes: the directive is the reset.
	o2 := cfg.applyDashboard(context.Background(), g, api.Directive{Kind: api.DirectiveDashboard}, nil, t.Logf)
	if o2.Detail == o.Detail {
		t.Error("a second directive reused the code")
	}
}

func TestDashboardNeedsAFlockNameAndAGuest(t *testing.T) {
	g := &fakeHandoff{}
	if o := (Config{}).applyDashboard(context.Background(), g, api.Directive{Kind: api.DirectiveDashboard}, nil, t.Logf); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "has no name yet") {
		t.Errorf("unnamed flock = %+v; want failed, saying the machine is unnamed", o)
	}
	g.err = errors.New("channel down")
	if o := (Config{FlockName: "x"}).applyDashboard(context.Background(), g, api.Directive{Kind: api.DirectiveDashboard}, nil, t.Logf); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "channel down") {
		t.Errorf("guest refusing = %+v; want failed with the cause", o)
	}
	if o := (Config{FlockName: "x"}).applyDashboard(context.Background(), g, api.Directive{Kind: api.DirectiveDashboard, Payload: "{"}, nil, t.Logf); o.State != api.OutcomeFailed {
		t.Errorf("bad payload = %+v; want failed", o)
	}
}

// THE NAME FIRST, WHEN A BROWSER CAN OPEN IT: registered, found to resolve on this network, with
// its certificate; the `.local` alias under it with the same code. Short of any of the three,
// the alias alone -- an https link to a name without its certificate is a warning, not a page.
func TestDashboardOffersTheNameOnceItIsUsable(t *testing.T) {
	g := &fakeHandoff{}
	cfg := Config{FlockName: "brave-elf"}
	cs := &casaRunner{cfg: cfg, st: casaState{Registered: true}, resolves: dashboard.CasaResolves, certUntil: time.Now().Add(time.Hour)}
	o := cfg.applyDashboard(context.Background(), g, api.Directive{Kind: api.DirectiveDashboard}, cs, t.Logf)
	if want := "https://brave-elf.briard.casa/?code=" + g.got.Code + "\nhttp://briard.local/?code=" + g.got.Code; o.Detail != want {
		t.Errorf("Detail = %q, want %q", o.Detail, want)
	}
	for _, short := range []func(){
		func() { cs.resolves = "" },
		func() { cs.resolves, cs.certUntil = dashboard.CasaResolves, time.Time{} },
		func() { cs.certUntil, cs.st.Registered = time.Now().Add(time.Hour), false },
	} {
		short()
		o := cfg.applyDashboard(context.Background(), g, api.Directive{Kind: api.DirectiveDashboard}, cs, t.Logf)
		if want := "http://briard.local/?code=" + g.got.Code; o.Detail != want {
			t.Errorf("Detail = %q, want the alias alone", o.Detail)
		}
	}
}
