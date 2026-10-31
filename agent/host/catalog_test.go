package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
	"briard.io/shared/manifest"
	"briard.io/shared/notify"
)

// indexRing verifies with one throwaway key, the way the release keyring does.
type indexRing struct{ pub ed25519.PublicKey }

func (r indexRing) Verify(artifact, sig []byte) error {
	if !ed25519.Verify(r.pub, artifact, sig) {
		return errors.New("bad signature")
	}
	return nil
}

// fakeCatalog is a published catalog: a signed index of two apps -- Home Assistant installable
// from the start, Paperless-ngx when a test publishes it, which re-signs the index with the
// mark. Nothing but the index is served: a refresh that asks for anything else is the shape
// this node no longer has, and the test says so.
type fakeCatalog struct {
	srv  *httptest.Server
	priv ed25519.PrivateKey
	mu   sync.Mutex
	idx  string
}

const testIndex = `{"apps":[` +
	`{"name":"home-assistant","available":true,"title":"Home Assistant","functionality":"Smart home control","description":"Runs your home.","homepage":"https://www.home-assistant.io"},` + "\n" +
	`{"name":"paperless-ngx","title":"Paperless-ngx","functionality":"Document management system","description":"Scan it, find it.","homepage":"https://docs.paperless-ngx.com"}` +
	`]}`

func newFakeCatalog(t *testing.T) (*fakeCatalog, *manifest.Catalog) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCatalog{priv: priv, idx: testIndex}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Write([]byte(f.idx))
	})
	mux.HandleFunc("/index.json.sig", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Write(ed25519.Sign(f.priv, []byte(f.idx)))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the node asked the catalog for %s; the index is the whole read", r.URL.Path)
		http.NotFound(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f, &manifest.Catalog{BaseURL: f.srv.URL, Verifier: indexRing{pub}, Client: f.srv.Client()}
}

// publish marks name available in the index, as the catalog's publish does beside the manifest.
func (f *fakeCatalog) publish(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idx = strings.Replace(f.idx, `{"name":"`+name+`",`, `{"name":"`+name+`","available":true,`, 1)
}

// newTestCatalog is a catalog state over a temp directory on a fixed clock.
func newTestCatalog(t *testing.T, cat *manifest.Catalog) (*catalogState, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	s := newCatalogState(t.TempDir(), cat, func(f string, v ...any) { t.Logf(f, v...) })
	s.now = func() time.Time { return now }
	return s, &now
}

func appOf(t *testing.T, v dashboard.Catalog, name string) dashboard.CatalogApp {
	t.Helper()
	for _, a := range v.Apps {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("%s is not in the view: %+v", name, v.Apps)
	return dashboard.CatalogApp{}
}

// A refresh reads the index, and nothing else, and keeps it on disk: a fresh state over the
// same directory knows it with no network, and is not due again until the cache is a day old.
func TestCatalogRefreshReadsTheIndexAndWhatIsInstallable(t *testing.T) {
	_, cat := newFakeCatalog(t)
	s, now := newTestCatalog(t, cat)
	ctx := context.Background()
	if v := s.view(); len(v.Apps) != 0 || !v.Fetched.IsZero() {
		t.Fatalf("before any read the view must be empty, got %+v", v)
	}
	s.refresh(ctx)
	v := s.view()
	if len(v.Apps) != 2 || !v.Fetched.Equal(*now) {
		t.Fatalf("view = %+v", v)
	}
	if !appOf(t, v, "home-assistant").Available || appOf(t, v, "paperless-ngx").Available {
		t.Errorf("availability is the index's word: %+v", v.Apps)
	}
	if appOf(t, v, "paperless-ngx").Functionality != "Document management system" {
		t.Errorf("the index's text must reach the view: %+v", appOf(t, v, "paperless-ngx"))
	}

	// The same directory, a catalog that cannot be reached: the cache answers, and nothing is
	// asked for until the cache is a day old.
	off := newCatalogState(s.dir, &manifest.Catalog{BaseURL: "http://127.0.0.1:1", Verifier: indexRing{}}, s.logf)
	off.now = s.now
	off.refresh(ctx)
	if v2 := off.view(); len(v2.Apps) != 2 || !appOf(t, v2, "home-assistant").Available {
		t.Errorf("the cache must answer with the catalog unreachable: %+v", v2)
	}
	*now = now.Add(catalogRefreshEvery)
	off.refresh(ctx) // due, fails, keeps what it had
	if v2 := off.view(); len(v2.Apps) != 2 {
		t.Errorf("a failed read must keep the last index: %+v", v2)
	}
}

// The wish toggles: recorded, removed, recorded again, and kept across a restart. It is refused
// before the index is read, for a name the index does not list, and for an app that is
// installable (the page offers Install there).
func TestCatalogWishTogglesAndIsRefusedWhereItMakesNoSense(t *testing.T) {
	_, cat := newFakeCatalog(t)
	s, _ := newTestCatalog(t, cat)
	wish := func(name string) api.DirectiveOutcome {
		return s.toggle(api.Directive{ID: "w", Kind: api.DirectiveWish, Payload: name})
	}
	if o := wish("paperless-ngx"); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "not read the catalog") {
		t.Errorf("before the index: %+v", o)
	}
	s.refresh(context.Background())
	if o := wish("immich"); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "does not list") {
		t.Errorf("an unlisted name: %+v", o)
	}
	if o := wish("home-assistant"); o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "install it instead") {
		t.Errorf("an installable app: %+v", o)
	}
	if o := wish("paperless-ngx"); o.State != api.OutcomeDone || !strings.Contains(o.Detail, "recorded") {
		t.Errorf("the first press: %+v", o)
	}
	if !appOf(t, s.view(), "paperless-ngx").Wished {
		t.Error("the view does not show the wish")
	}
	again := newCatalogState(s.dir, cat, s.logf)
	if !appOf(t, again.view(), "paperless-ngx").Wished {
		t.Error("the wish did not survive a restart")
	}
	if o := wish("paperless-ngx"); o.State != api.OutcomeDone || !strings.Contains(o.Detail, "removed") {
		t.Errorf("the second press: %+v", o)
	}
	if appOf(t, s.view(), "paperless-ngx").Wished {
		t.Error("the second press did not remove the wish")
	}
	if raw, err := os.ReadFile(filepath.Join(s.dir, wishesName)); err != nil || string(raw) != "[]" {
		t.Errorf("wishes on disk = %q, %v; want an empty list", raw, err)
	}
}

// The notice: nothing while the wished app has no manifest; one open alert when a refresh finds
// one; silence while that holds; resolved once the app is installed -- or, in the other
// scenario, once the wish is taken back.
func TestCatalogWishNoticeIsAnEdge(t *testing.T) {
	for _, settle := range []string{"installed", "taken back"} {
		t.Run(settle, func(t *testing.T) {
			f, cat := newFakeCatalog(t)
			s, now := newTestCatalog(t, cat)
			fn := &fakeNotifier{}
			st := newAlertStore(filepath.Join(t.TempDir(), "alerts.json"), fn, s.logf)
			ctx := context.Background()
			none := map[string]bool{}
			s.refresh(ctx)
			s.toggle(api.Directive{Kind: api.DirectiveWish, Payload: "paperless-ngx"})
			s.notices(ctx, st, none)
			if len(fn.alerts) != 0 {
				t.Fatalf("a wish with no manifest alerted: %+v", fn.alerts)
			}
			f.publish("paperless-ngx")
			*now = now.Add(catalogRefreshEvery)
			s.refresh(ctx)
			s.notices(ctx, st, none)
			s.notices(ctx, st, none)
			if got := levels(fn.alerts); got != "open" {
				t.Fatalf("after the manifest appeared: %q (%+v); want one open", got, fn.alerts)
			}
			if a := fn.alerts[0]; a.Key != "catalog:paperless-ngx" || !strings.Contains(a.Title, "Paperless-ngx is now available") || a.Severity != notify.Info {
				t.Errorf("the notice: %+v", a)
			}
			if settle == "installed" {
				none = map[string]bool{"paperless-ngx": true}
			} else {
				s.toggle(api.Directive{Kind: api.DirectiveWish, Payload: "paperless-ngx"})
			}
			s.notices(ctx, st, none)
			s.notices(ctx, st, none)
			if got := levels(fn.alerts); got != "open,resolved" {
				t.Fatalf("settled by being %s: %q; want open,resolved", settle, got)
			}
		})
	}
}

type fakeCatalogGuest struct {
	views    []dashboard.Catalog
	supports bool
}

func (g *fakeCatalogGuest) DashboardCatalog(_ context.Context, c dashboard.Catalog) error {
	g.views = append(g.views, c)
	return nil
}
func (g *fakeCatalogGuest) SupportsDashboardCatalog() bool { return g.supports }

// The page's copy is pushed once per connection and again only when the view moved; a guest
// that does not take it is left alone.
func TestCatalogViewIsPushedWhenItMoves(t *testing.T) {
	_, cat := newFakeCatalog(t)
	s, _ := newTestCatalog(t, cat)
	s.refresh(context.Background())
	cfg := Config{catalog: s}
	g := &fakeCatalogGuest{supports: true}
	pushed := ""
	ctx := context.Background()
	cfg.pushCatalogView(ctx, g, &fakeNotifier{}, &pushed, s.logf)
	cfg.pushCatalogView(ctx, g, &fakeNotifier{}, &pushed, s.logf)
	if len(g.views) != 1 || len(g.views[0].Apps) != 2 {
		t.Fatalf("pushed %d views (%+v); want the one", len(g.views), g.views)
	}
	s.toggle(api.Directive{Kind: api.DirectiveWish, Payload: "paperless-ngx"})
	cfg.pushCatalogView(ctx, g, &fakeNotifier{}, &pushed, s.logf)
	if len(g.views) != 2 || !appOf(t, g.views[1], "paperless-ngx").Wished {
		t.Fatalf("the wish did not move the view: %+v", g.views)
	}
	old := &fakeCatalogGuest{}
	pushed = ""
	cfg.pushCatalogView(ctx, old, &fakeNotifier{}, &pushed, s.logf)
	if len(old.views) != 0 {
		t.Error("a guest without the verb was pushed to")
	}
	none := ""
	Config{}.pushCatalogView(ctx, g, &fakeNotifier{}, &none, s.logf) // no catalog: nothing, no panic
}
