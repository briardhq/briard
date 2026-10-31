package host

// THE CATALOG'S INDEX, AS THIS NODE HOLDS IT. The published index lists every app a household
// can see and which of them it can install; this node fetches it, whole, on a daily read,
// and keeps it node-locally for its dashboard to render. The page never reaches the
// network and no copy of the list is baked into any binary: the list has the catalog's clock,
// not the release's, and a node that has never been online has no list to show.
//
// A WISH IS THE HOUSEHOLD'S, AND IS KEPT HERE. "I want this" on a listed-but-not-installable
// app records the name on this node; when a later read of the index finds a manifest for a
// wished app, the node tells the household itself -- the card on its page and an alert through
// the ordinary surface -- so the promise the button makes is kept by the node alone, with no
// account and no address. A household's preference is by nature shared by every node of a
// flock, but a wish is not worth a file on the replicated volume; node-local is the trade, and
// it is a trade.
//
// A NEW WISH IS ALSO COUNTED, ONCE (api.Wish): the app's name and a voter value that is a
// one-way hash of the flock's id, sent off the loop with two retries and then dropped. The
// count is how the next app is chosen; the wish on this node never depends on it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"briard.io/agent/cloud"
	"briard.io/agent/selfupdate"
	"briard.io/shared/api"
	"briard.io/shared/atomicfile"
	"briard.io/shared/dashboard"
	"briard.io/shared/manifest"
	"briard.io/shared/model"
	"briard.io/shared/notify"
)

const (
	// catalogCacheName is the index as last read, beside the node's other caches; wishesName
	// the household's wishes, by catalog name. Two files because they have two writers with
	// two clocks: the refresh rewrites the first, a button press the second.
	catalogCacheName = "catalog.json"
	wishesName       = "wishes.json"
	// catalogRefreshEvery is how old the cache may be before the next watcher tick re-reads
	// the index: daily, the update check's own cadence. A failed read leaves the cache as it
	// was, so it is retried at the next tick.
	catalogRefreshEvery = 24 * time.Hour
	// catalogReadBudget bounds one refresh: the one read of the index.
	catalogReadBudget = 3 * time.Minute
)

// wishRetry is the count's patience: the waits between three tries, after which the count is
// dropped and the wish stands on this node all the same.
var wishRetry = []time.Duration{30 * time.Second, 5 * time.Minute}

// catalogCache is the file's shape: the verified bytes kept whole (parsed again on load, so a
// corrupt cache is an empty one rather than a half-list), and which listed apps had a manifest
// when they were read.
type catalogCache struct {
	Fetched time.Time       `json:"fetched"`
	Index   json.RawMessage `json:"index"`
}

// catalogState is what this node knows of the catalog's index and of the household's wishes.
// One per agent, shared by the watcher that refreshes it, the dispatch that toggles a wish and
// the loop that pushes the page's view; the mutex is the whole of their ordering.
type catalogState struct {
	dir  string
	cat  *manifest.Catalog
	logf func(string, ...any)
	now  func() time.Time
	// tell counts a new wish with us, off the caller's goroutine; nil counts nothing.
	tell func(app string)

	mu     sync.Mutex
	loaded bool
	c      catalogCache
	idx    manifest.Index
	wishes []string
	// settled is the wishes the last notices() asserted, so a wish taken back since gets its
	// resolved asserted once more (the store drops it when nothing was open).
	settled []string
}

func newCatalogState(dir string, cat *manifest.Catalog, logf func(string, ...any)) *catalogState {
	return &catalogState{dir: dir, cat: cat, logf: logf, now: time.Now}
}

// load reads both files once. A file that is missing or does not parse is empty: the index is
// re-read at the next tick, and a wish list that cannot be read is a list of none, logged.
func (s *catalogState) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	if raw, err := os.ReadFile(filepath.Join(s.dir, catalogCacheName)); err == nil {
		var c catalogCache
		if err := json.Unmarshal(raw, &c); err != nil {
			s.logf("catalog: cache does not parse (%v); reading the index again", err)
		} else if idx, err := manifest.ParseIndex(c.Index); err != nil {
			s.logf("catalog: cached index does not parse (%v); reading it again", err)
		} else {
			s.c, s.idx = c, idx
		}
	}
	if raw, err := os.ReadFile(filepath.Join(s.dir, wishesName)); err == nil {
		if err := json.Unmarshal(raw, &s.wishes); err != nil {
			s.logf("catalog: wishes do not parse (%v); starting from none", err)
			s.wishes = nil
		}
	}
}

// refresh re-reads the index when the cache is due. Nil-safe, for a node built without a
// catalog. The network is touched off the loop, on the watcher's goroutine, and the lock is
// held only around the state.
func (s *catalogState) refresh(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.load()
	due := s.c.Fetched.IsZero() || s.now().Sub(s.c.Fetched) >= catalogRefreshEvery
	s.mu.Unlock()
	if !due {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, catalogReadBudget)
	defer cancel()
	idx, raw, err := s.cat.FetchIndex(rctx)
	if err != nil {
		s.logf("catalog: index: %v (keeping what was read before)", err)
		return
	}
	c := catalogCache{Fetched: s.now(), Index: raw}
	out, err := json.Marshal(c)
	if err != nil {
		s.logf("catalog: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := atomicfile.Write(filepath.Join(s.dir, catalogCacheName), out, 0o600, 0o700); err != nil {
		s.logf("catalog: cache: %v", err)
		return
	}
	s.c, s.idx = c, idx
	installable := 0
	for _, a := range idx.Apps {
		if a.Available {
			installable++
		}
	}
	s.logf("catalog: index read: %d apps listed, %d installable", len(idx.Apps), installable)
}

// toggle is the household's "I want this", and its second press: the wish recorded when
// absent, removed when present. Refused for an app the index does not list -- before the first
// read of the index there is nothing to wish for -- and for an installable one that is not
// already wished: the page offers Install there, not a wish.
func (s *catalogState) toggle(d api.Directive) api.DirectiveOutcome {
	failed := func(detail string) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: detail}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	name := d.Payload
	i := slices.IndexFunc(s.idx.Apps, func(a manifest.App) bool { return a.Name == name })
	if i < 0 {
		if len(s.idx.Apps) == 0 {
			return failed("this machine has not read the catalog yet")
		}
		return failed(fmt.Sprintf("the catalog does not list %q", name))
	}
	wished := slices.Contains(s.wishes, name)
	if !wished && s.idx.Apps[i].Available {
		return failed(s.idx.Apps[i].Title + " is available; install it instead")
	}
	wishes := slices.Clone(s.wishes)
	if wished {
		wishes = slices.DeleteFunc(wishes, func(n string) bool { return n == name })
	} else {
		wishes = append(wishes, name)
	}
	if wishes == nil {
		wishes = []string{} // "none", not "never written"
	}
	raw, err := json.Marshal(wishes)
	if err != nil {
		return failed(err.Error())
	}
	if err := atomicfile.Write(filepath.Join(s.dir, wishesName), raw, 0o600, 0o700); err != nil {
		return failed(fmt.Sprintf("record the wish: %v", err))
	}
	s.wishes = wishes
	if wished {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "the wish for " + s.idx.Apps[i].Title + " is removed"}
	}
	if s.tell != nil {
		s.tell(name)
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "the wish for " + s.idx.Apps[i].Title + " is recorded"}
}

// view is what the page is told: every listed app in the index's order, with this node's facts
// about each -- wished, installed, and whether the index offers an update to what it runs.
// Nil-safe; a node without a catalog tells the page nothing.
//
// THE OFFER IS DERIVED, NEVER STORED: the index's identity against the installed one, at every
// look, so an update that commits or an undo that moves the app back changes the offer at the
// next push with nothing to clear. An index that names no identity (one published before the
// field) offers nothing.
func (s *catalogState) view(installed map[string]model.ServiceSpec) dashboard.Catalog {
	if s == nil {
		return dashboard.Catalog{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	v := dashboard.Catalog{Fetched: s.c.Fetched, Apps: []dashboard.CatalogApp{}}
	for _, a := range s.idx.Apps {
		ca := dashboard.CatalogApp{App: a, Wished: slices.Contains(s.wishes, a.Name)}
		if spec, ok := installed[a.Name]; ok {
			ca.Installed, ca.Running = true, spec.Version
			ca.Update = a.Available && a.Identity != "" && string(a.Identity) != spec.Manifest
		}
		v.Apps = append(v.Apps, ca)
	}
	return v
}

// notices asserts, for every wished app, whether its wish has been met: open while the app
// has gained a manifest and is not installed, resolved once it is installed or the wish is taken
// back. Asserted at every look, like every other alerter here; the store makes it an edge.
func (s *catalogState) notices(ctx context.Context, n notify.Notifier, installed map[string]model.ServiceSpec) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.load()
	type wish struct {
		app manifest.App
		met bool
	}
	var ws []wish
	for _, a := range s.idx.Apps {
		if slices.Contains(s.wishes, a.Name) {
			_, have := installed[a.Name]
			ws = append(ws, wish{app: a, met: a.Available && !have})
		}
	}
	// A wish taken back has no entry above, so its resolved is asserted here, by name.
	var gone []string
	for _, name := range s.settled {
		if !slices.ContainsFunc(ws, func(w wish) bool { return w.app.Name == name }) {
			gone = append(gone, name)
		}
	}
	s.settled = s.settled[:0]
	for _, w := range ws {
		s.settled = append(s.settled, w.app.Name)
	}
	s.mu.Unlock()
	for _, w := range ws {
		fireAlert(ctx, n, s.logf, wishAlert(w.app, w.met))
	}
	for _, name := range gone {
		fireAlert(ctx, n, s.logf, wishAlert(manifest.App{Name: name, Title: name}, false))
	}
}

// wishAlert is what the household is told: the app by its title, and that it can be installed
// from the Apps page -- the one thing a person can do about it.
func wishAlert(a manifest.App, met bool) notify.Alert {
	key := "catalog:" + a.Name
	if !met {
		return notify.Alert{Key: key, Kind: notify.Resolved, Title: a.Title + ": the wish is settled",
			Body: "It is installed now, or the wish was taken back."}
	}
	return notify.Alert{Key: key, Kind: notify.Open, Severity: notify.Info,
		Title: a.Title + " is now available on your Briard",
		Body:  fmt.Sprintf("You asked for %s (%s). Briard can install it now: open the Apps page.", a.Title, a.Functionality)}
}

// catalogGuest is the slice of the guest binding the page's copy needs. *guestagent.Client
// satisfies it.
type catalogGuest interface {
	DashboardCatalog(ctx context.Context, c dashboard.Catalog) error
	SupportsDashboardCatalog() bool
}

// pushCatalogView hands the guest the page's view when it differs from what this connection
// last pushed, and asserts the wishes' notices against what is installed now. pushed starts
// empty with every connection, so a fresh guest is always told.
func (cfg Config) pushCatalogView(ctx context.Context, r any, n notify.Notifier, pushed *string, logf func(string, ...any)) {
	if cfg.catalog == nil {
		return
	}
	installed := cfg.installedByName(logf)
	cfg.catalog.notices(ctx, n, installed)
	g, ok := r.(catalogGuest)
	if !ok || !g.SupportsDashboardCatalog() {
		return
	}
	v := cfg.catalog.view(installed)
	raw, err := json.Marshal(v)
	if err != nil || string(raw) == *pushed {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := g.DashboardCatalog(pctx, v); err != nil {
		logf("catalog: dashboard copy: %v", err)
		return
	}
	*pushed = string(raw)
}

// applyWish is the directive's door (dispatch): a node without a catalog has nothing to wish from.
func (cfg Config) applyWish(d api.Directive) api.DirectiveOutcome {
	if cfg.catalog == nil {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: "this node reads no catalog"}
	}
	return cfg.catalog.toggle(d)
}

// newCatalog builds the node's catalog state where it can exist: a state directory to cache in
// and the release keyring to verify with. Elsewhere nil, and every caller is nil-safe: the page
// then shows what is installed and nothing it could add.
func (cfg Config) newCatalog(ctx context.Context, logf func(string, ...any)) *catalogState {
	if cfg.stateDir() == "" || cfg.CatalogURL == "" {
		return nil
	}
	kr, err := selfupdate.NewKeyring(cfg.UpdateKeyring)
	if err != nil {
		logf("catalog: release keyring: %v (the page lists only what is installed)", err)
		return nil
	}
	s := newCatalogState(cfg.stateDir(), &manifest.Catalog{BaseURL: cfg.CatalogURL, Verifier: kr}, logf)
	if cfg.CasaURL != "" {
		voter := cfg.wishVoter()
		s.tell = func(app string) {
			w := api.Wish{App: app, Voter: voter}
			go tellWish(ctx, func(ctx context.Context) error { return cloud.SendWish(ctx, cfg.CasaURL, w) }, app, wishRetry, logf)
		}
	}
	return s
}

// tellWish sends one count, retrying after each of waits, and gives up quietly: a count that
// never arrives costs one vote in a ranking, and the wish itself is already on disk.
func tellWish(ctx context.Context, send func(context.Context) error, app string, waits []time.Duration, logf func(string, ...any)) {
	for i := 0; ; i++ {
		err := send(ctx)
		if err == nil {
			logf("catalog: the wish for %s is counted", app)
			return
		}
		if i == len(waits) || ctx.Err() != nil {
			logf("catalog: the wish for %s was not counted: %v (it stands on this machine)", app, err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(waits[i]):
		}
	}
}

// wishVoter is the household's name in the wish count: a one-way hash of the flock's id made
// for this alone, so the count sees a household once and the value matches nothing else the id
// feeds. Where there is no flock id it falls back to the node's name, as the service address does.
func (cfg Config) wishVoter() string {
	h := sha256.Sum256([]byte("briard-wish-voter:" + orNode(cfg.FlockID, cfg.Node)))
	return hex.EncodeToString(h[:16])
}

// installedByName is what this node runs, by service name, from the node-local cache: the
// installed side of every catalog question (notices, view).
func (cfg Config) installedByName(logf func(string, ...any)) map[string]model.ServiceSpec {
	out := map[string]model.ServiceSpec{}
	if specs, _, ok := cfg.installedServices(logf); ok {
		for _, s := range specs {
			out[s.Name] = s
		}
	}
	return out
}
