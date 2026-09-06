package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/agent/install"
	"briard.io/shared/api"
)

func guestMan(version, system, minHost string) install.Manifest {
	return install.Manifest{Chain: install.ChainGuest, Version: version, System: system, MinHost: minHost,
		Artifacts: []install.Entry{{Name: "nixos.qcow2.zst"}}}
}

// The guest chain's comparison ([B.86d]), one row each: the closure is the truth, min_host is
// the one direction that can go wrong, and the host chain's ordering rules apply after both.
func TestDecideGuest(t *testing.T) {
	const host = "v3.20260906.aaaaaaa"
	old, cur, next := "guest.20260901.o", "guest.20260906.c", "guest.20260910.n"
	sysOld, sysCur, sysNext := "/nix/store/o-nixos-system", "/nix/store/c-nixos-system", "/nix/store/n-nixos-system"
	stable := guestMan(cur, sysCur, "")
	for _, tc := range []struct {
		name         string
		target       string
		want         install.Manifest
		have         *install.Manifest
		stable       *install.Manifest
		running      string
		install      bool
		err          error
		reasonSubstr string
	}{
		{"already running the release's closure is a no-op whatever the record says", install.TargetStable, guestMan(next, sysNext, ""), nil, nil, sysNext, false, nil, "already running"},
		{"stable newer than the record installs", install.TargetStable, guestMan(next, sysNext, ""), ptr(guestMan(cur, sysCur, "")), nil, sysCur, true, nil, "stable moved"},
		{"stable older than the record is a no-op (a pin survives the timer)", install.TargetStable, guestMan(old, sysOld, ""), ptr(guestMan(cur, sysCur, "")), nil, sysCur, false, nil, "nothing to do"},
		{"no record installs", install.TargetStable, guestMan(cur, sysCur, ""), nil, nil, sysOld, true, nil, "no installed manifest"},
		{"a record naming this release while the guest runs another closure is disbelieved", install.TargetLatest, guestMan(cur, sysCur, ""), ptr(guestMan(cur, sysCur, "")), nil, sysOld, true, nil, "no installed manifest"},
		{"min_host at the host's date is satisfied", install.TargetLatest, guestMan(next, sysNext, host), nil, nil, sysCur, true, nil, ""},
		{"min_host past the host refuses", install.TargetLatest, guestMan(next, sysNext, "v3.20260907.bbbbbbb"), nil, nil, sysCur, false, install.ErrHostTooOld, ""},
		{"min_host refuses even when already running (the host is the problem)", install.TargetLatest, guestMan(next, sysNext, "v3.20270101.bbbbbbb"), nil, nil, sysNext, false, install.ErrHostTooOld, ""},
		{"no closure cannot be applied", install.TargetLatest, guestMan(next, "", ""), nil, nil, sysCur, false, install.ErrManifest, ""},
		{"an exact pin below stable is refused", old, guestMan(old, sysOld, ""), ptr(guestMan(cur, sysCur, "")), &stable, sysCur, false, install.ErrBelowFloor, ""},
		{"an exact pin at stable's date installs", cur, guestMan(cur, sysCur, ""), ptr(guestMan(next, sysNext, "")), &stable, sysNext, true, nil, ""},
		{"a host id with no date cannot satisfy a min_host", install.TargetLatest, guestMan(next, sysNext, host), nil, nil, sysCur, false, install.ErrHostTooOld, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hv := host
			if strings.HasPrefix(tc.name, "a host id with no date") {
				hv = "dev"
			}
			d, err := decideGuest(tc.target, tc.want, tc.have, tc.stable, tc.running, hv)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Install != tc.install {
				t.Errorf("install = %v (%s), want %v", d.Install, d.Reason, tc.install)
			}
			if tc.reasonSubstr != "" && !strings.Contains(d.Reason, tc.reasonSubstr) {
				t.Errorf("reason = %q, want it to mention %q", d.Reason, tc.reasonSubstr)
			}
		})
	}
}

func ptr(m install.Manifest) *install.Manifest { return &m }

// guestChannel is a signed guest chain served over HTTP: one release under `latest` and
// `stable` (the same release, unless stableVersion differs), keyed by the returned PEM.
type guestChannel struct {
	priv   ed25519.PrivateKey
	bodies map[string][]byte
	srv    *httptest.Server
}

func newGuestChannel(t *testing.T) (*guestChannel, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	c := &guestChannel{priv: priv, bodies: map[string][]byte{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := c.bodies[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(c.srv.Close)
	return c, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func (c *guestChannel) publish(t *testing.T, m install.Manifest, pointers ...string) []byte {
	t.Helper()
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append([]string{m.Version}, pointers...) {
		c.bodies["guest/"+p+"/manifest.json"] = mb
		c.bodies["guest/"+p+"/manifest.json.sig"] = ed25519.Sign(c.priv, mb)
	}
	return mb
}

type stubSystem struct{ system string }

func (s stubSystem) SystemPath(context.Context) (string, error) { return s.system, nil }

func guestCfg(t *testing.T, c *guestChannel, key []byte) Config {
	t.Helper()
	return Config{
		Version:           "v3.20260906.aaaaaaa",
		ChannelURL:        c.srv.URL,
		UpdateKeyring:     key,
		GuestReleaseCache: filepath.Join(t.TempDir(), "guest-release.json"),
		UpgradeBudget:     testUpgradeBudget,
	}
}

// The directive end to end: a signed guest release resolved on the channel, its closure staged
// and switched through the existing upgrade path, the node-local record written with the exact
// signed bytes -- and, once there, a no-op that says so. [[verification-assertions-must-fail]]:
// the closure the upgrader saw is the manifest's, not something the test handed it.
func TestUpdateGuestResolvesAndUpgrades(t *testing.T) {
	c, key := newGuestChannel(t)
	want := guestMan("guest.20260910.nnnnnnn", "/nix/store/n-nixos-system", "v3.20260906.aaaaaaa")
	raw := c.publish(t, want, install.TargetLatest, install.TargetStable)
	cfg := guestCfg(t, c, key)
	up := &fakeUpgrader{}
	o := cfg.applyGuestUpdate(context.Background(), api.Directive{ID: "d1", Kind: install.DirectiveUpdateGuest}, stubSystem{"/nix/store/o-nixos-system"}, up, nil, t.Logf)
	if o.State != api.OutcomeDone || o.ID != "d1" {
		t.Fatalf("outcome = %+v, want done", o)
	}
	if up.staged != want.System || up.target != want.System {
		t.Errorf("staged %q, switched %q; want the manifest's closure %q", up.staged, up.target, want.System)
	}
	if !strings.Contains(o.Detail, "now running "+want.Version) {
		t.Errorf("detail = %q", o.Detail)
	}
	if b, err := os.ReadFile(cfg.GuestReleaseCache); err != nil || string(b) != string(raw) {
		t.Errorf("record = %q, %v; want the exact signed manifest", b, err)
	}

	// Now running it: a no-op that names the release, and nothing is staged again.
	up2 := &fakeUpgrader{}
	o = cfg.applyGuestUpdate(context.Background(), api.Directive{Kind: install.DirectiveUpdateGuest, Payload: install.TargetStable}, stubSystem{want.System}, up2, nil, t.Logf)
	if o.State != api.OutcomeDone || !strings.Contains(o.Detail, "already running "+want.Version) || up2.staged != "" {
		t.Errorf("second run: %+v (staged %q)", o, up2.staged)
	}
}

// Refusals that must leave the node untouched: a host below min_host (escalated -- it is the
// support window closing), a release the channel does not carry, a tampered manifest, no
// keyring, and an exact pin below stable.
func TestUpdateGuestRefusesWithoutTouchingTheNode(t *testing.T) {
	c, key := newGuestChannel(t)
	stable := guestMan("guest.20260906.ccccccc", "/nix/store/c-nixos-system", "")
	c.publish(t, stable, install.TargetStable)
	tooNew := guestMan("guest.20260910.nnnnnnn", "/nix/store/n-nixos-system", "v3.20270101.zzzzzzz")
	c.publish(t, tooNew, install.TargetLatest)
	old := guestMan("guest.20260101.ooooooo", "/nix/store/old-nixos-system", "")
	c.publish(t, old)
	cfg := guestCfg(t, c, key)
	running := stubSystem{"/nix/store/x-nixos-system"}

	for _, tc := range []struct {
		name, target, want string
		cfg                Config
	}{
		{"host too old", install.TargetLatest, "older than the guest release requires", cfg},
		{"unknown pin", "guest.20990101.nothere", "404", cfg},
		{"pin below stable", old.Version, "older than stable", cfg},
		{"no keyring", install.TargetStable, "no release keyring", func() Config { c := cfg; c.UpdateKeyring = nil; return c }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeUpgrader{}
			o := tc.cfg.applyGuestUpdate(context.Background(), api.Directive{Kind: install.DirectiveUpdateGuest, Payload: tc.target}, running, up, nil, t.Logf)
			if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, tc.want) {
				t.Errorf("outcome = %+v, want failed mentioning %q", o, tc.want)
			}
			if up.staged != "" || up.target != "" || up.rebootTarget != "" {
				t.Errorf("a refused update touched the node: %+v", up)
			}
			if _, err := os.Stat(tc.cfg.GuestReleaseCache); tc.cfg.GuestReleaseCache != "" && !os.IsNotExist(err) {
				t.Error("a refused update wrote the release record")
			}
		})
	}

	// Tampered after signing: refused by the signature, before any decision.
	c.bodies["guest/latest/manifest.json"] = []byte(`{"chain":"guest","version":"guest.20260910.nnnnnnn","system":"/nix/store/evil","artifacts":[{"name":"x"}]}`)
	up := &fakeUpgrader{}
	o := cfg.applyGuestUpdate(context.Background(), api.Directive{Kind: install.DirectiveUpdateGuest}, running, up, nil, t.Logf)
	if o.State != api.OutcomeFailed || up.staged != "" {
		t.Errorf("a tampered manifest was acted on: %+v (staged %q)", o, up.staged)
	}
}

// A guest that runs the record's closure but was moved by the cloud since is upgraded rather
// than reported as already there; a guest already on the release's closure with a stale record
// gets the record corrected without an upgrade.
func TestUpdateGuestTrustsTheClosureOverTheRecord(t *testing.T) {
	c, key := newGuestChannel(t)
	want := guestMan("guest.20260910.nnnnnnn", "/nix/store/n-nixos-system", "")
	raw := c.publish(t, want, install.TargetLatest)
	cfg := guestCfg(t, c, key)
	os.WriteFile(cfg.GuestReleaseCache, raw, 0o644) // the record says: at want
	up := &fakeUpgrader{}
	o := cfg.applyGuestUpdate(context.Background(), api.Directive{Kind: install.DirectiveUpdateGuest}, stubSystem{"/nix/store/moved-by-cloud"}, up, nil, t.Logf)
	if o.State != api.OutcomeDone || up.target != want.System {
		t.Errorf("a stale record suppressed the upgrade: %+v (switched %q)", o, up.target)
	}

	cfg2 := guestCfg(t, c, key) // no record, guest already on the closure
	up2 := &fakeUpgrader{}
	o = cfg2.applyGuestUpdate(context.Background(), api.Directive{Kind: install.DirectiveUpdateGuest}, stubSystem{want.System}, up2, nil, t.Logf)
	if o.State != api.OutcomeDone || up2.staged != "" {
		t.Fatalf("outcome = %+v, staged %q", o, up2.staged)
	}
	if b, _ := os.ReadFile(cfg2.GuestReleaseCache); string(b) != string(raw) {
		t.Error("the record was not corrected to the release the guest runs")
	}
}

// The nightly tick lands in the small hours, tomorrow if today's have passed, never more than
// two hours past three.
func TestNextGuestUpdateTick(t *testing.T) {
	loc := time.FixedZone("test", 2*3600)
	for _, now := range []time.Time{
		time.Date(2026, 9, 6, 1, 0, 0, 0, loc),  // before the window: today
		time.Date(2026, 9, 6, 3, 0, 0, 0, loc),  // at it: tomorrow
		time.Date(2026, 9, 6, 15, 0, 0, 0, loc), // after: tomorrow
	} {
		got := nextGuestUpdateTick(now, loc)
		if !got.After(now) {
			t.Errorf("tick %s is not after %s", got, now)
		}
		if h := got.In(loc).Hour(); h < 3 || h >= 5 {
			t.Errorf("tick %s is outside 03:00-05:00", got.In(loc))
		}
		if got.Sub(now) > 27*time.Hour {
			t.Errorf("tick %s is more than a day past %s", got, now)
		}
	}
}
