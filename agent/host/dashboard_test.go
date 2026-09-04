package host

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	o := cfg.applyDashboard(context.Background(), g, api.Directive{ID: "d1", Kind: api.DirectiveDashboard, Payload: `{"name":"Kostas","username":"kostas","language":"el"}`}, t.Logf)
	if o.State != api.OutcomeDone {
		t.Fatalf("outcome = %+v", o)
	}
	if len(g.got.Code) != 64 || g.got.Issued.IsZero() {
		t.Errorf("handoff = %+v; want a 32-byte hex code with an issue time", g.got)
	}
	if g.got.Name != "Kostas" || g.got.Username != "kostas" || g.got.Language != "el" {
		t.Errorf("account not carried: %+v", g.got)
	}
	want := "http://briard-brave-elf.local/?code=" + g.got.Code
	if o.Detail != want {
		t.Errorf("Detail = %q, want %q", o.Detail, want)
	}
	// Twice is two codes: the directive is the reset.
	o2 := cfg.applyDashboard(context.Background(), g, api.Directive{Kind: api.DirectiveDashboard}, t.Logf)
	if o2.Detail == o.Detail {
		t.Error("a second directive reused the code")
	}
}

func TestDashboardNeedsAFlockNameAndAGuest(t *testing.T) {
	g := &fakeHandoff{}
	if o := (Config{}).applyDashboard(context.Background(), g, api.Directive{Kind: api.DirectiveDashboard}, t.Logf); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "flock name") {
		t.Errorf("unnamed flock = %+v; want failed, naming the flock", o)
	}
	g.err = errors.New("channel down")
	if o := (Config{FlockName: "x"}).applyDashboard(context.Background(), g, api.Directive{Kind: api.DirectiveDashboard}, t.Logf); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "channel down") {
		t.Errorf("guest refusing = %+v; want failed with the cause", o)
	}
	if o := (Config{FlockName: "x"}).applyDashboard(context.Background(), g, api.Directive{Kind: api.DirectiveDashboard, Payload: "{"}, t.Logf); o.State != api.OutcomeFailed {
		t.Errorf("bad payload = %+v; want failed", o)
	}
}
