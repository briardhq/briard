package manifest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// The catalog's LIST is what a household can see: the apps it can install today and the ones it
// may ask for. A node reads one, `latest.json` (each app's newest version), at its CATALOG_URL,
// with a detached signature over the same bytes made by the release key; rigs read `dev.json`,
// the same shape. It is the only signed document in the catalog: a line names an app's version
// file beside it, `<name>/<version>.json`, by the sha256 of its exact bytes, so a mirror that
// cannot forge the list can forge neither the list nor any manifest it names.
//
// THE LIST SAYS WHICH APPS ARE INSTALLABLE. An entry marked available names its version and
// identity; the rest are listed so a household can ask for them. Omission is the LESS capable
// shape, the same discipline the manifest schema keeps: an entry that says nothing is one a
// node cannot install. A node learns the whole catalog from this one document, read whole and
// cached, the way every add-on store and package index is read; asking the catalog per app is
// what those moved away from.
//
// IT IS PROSE AND PICTURES, NOT CAPABILITY, which is why its rules are about size and safety
// rather than about what a node may do: nothing here is rendered into a unit file. The icon is
// the one field with teeth. It is carried as a data URI so a page can show it through an <img>,
// where an SVG's styles and scripts cannot reach the page, and it is checked here as well so a
// catalog cannot ship markup that a less careful renderer would inline.
const (
	// maxIndexSize bounds the whole document. Forty apps with inline vector icons weigh well
	// under 100 KB; the cap leaves room without letting a broken publish hand a node a
	// megabyte of prose.
	maxIndexSize = 300 << 10
	// maxIconSize bounds one decoded icon. Vector marks from a curated collection run 1 to 12
	// KB; a bitmap smuggled in as base64 does not fit.
	maxIconSize = 16 << 10
	iconSVG     = "data:image/svg+xml;base64,"
)

// Index is the catalog's listing, in the order a page shows it: the file's order IS the order,
// so a renderer needs no rule of its own and two renderers agree.
type Index struct {
	Apps []App `json:"apps"`
}

// App is one entry: what a household sees of an app before and after it is installable.
type App struct {
	// Name is the catalog slug, the key everything else hangs on: an installable app's version
	// files are under `<Name>/`, and the product's own knowledge of an app is keyed on it.
	Name string `json:"name"`
	// Available: the list names a version of the app a node can install. Absent for an app a
	// household can only ask for.
	Available bool `json:"available,omitempty"`
	// Version and Identity name that version, for an available app and only for one: its
	// upstream version, which is also its file's name (`<Name>/<Version>.json`), and the
	// Identity of the file's exact bytes, which is the app a node would install now and what it
	// checks the file against. A node that runs the app under another identity has an update to
	// offer, learned from this one document and with no read that depends on what the node runs.
	Version  string   `json:"version,omitempty"`
	Identity Identity `json:"identity,omitempty"`
	// Title is the app's name for a person ("Paperless-ngx").
	Title string `json:"title"`
	// Functionality says in two to five brand-less words what the app does ("document
	// management system"), shown beside the title: the title is known to few, the function
	// to most.
	Functionality string `json:"functionality"`
	// Description is a few lines on what the app does for a household. It promises nothing
	// and names no version; Version does, for an available app.
	Description string `json:"description"`
	// Icon is the app's mark as an SVG data URI, or empty, in which case a page shows the
	// title's initial instead. Bounded in size and checked for scripts and external
	// references, so it is safe to serve from a page that trusts it.
	Icon string `json:"icon,omitempty"`
	// Homepage is the upstream project's site: the one link a listed app can offer before it
	// is installable.
	Homepage string `json:"homepage"`
}

// ParseIndex decodes and validates an index. Unknown fields are refused, as the manifest's
// are: a field this build does not understand is one it cannot show correctly, and a catalog
// should not be able to widen what an older node renders without the node noticing.
func ParseIndex(raw []byte) (Index, error) {
	if len(raw) > maxIndexSize {
		return Index{}, fmt.Errorf("%w: index is %d bytes, larger than %d", ErrInvalid, len(raw), maxIndexSize)
	}
	var idx Index
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&idx); err != nil {
		return Index{}, fmt.Errorf("%w: index: %v", ErrInvalid, err)
	}
	if err := idx.Validate(); err != nil {
		return Index{}, err
	}
	return idx, nil
}

// Validate enforces the index's rules: every app named once by a slug, every text field
// present and on one line, the homepage an https URL, the icon a bounded SVG data URI with
// nothing in it that could run or fetch.
func (idx Index) Validate() error {
	if len(idx.Apps) == 0 {
		return fmt.Errorf("%w: index lists no apps", ErrInvalid)
	}
	seen := make(map[string]bool, len(idx.Apps))
	for _, a := range idx.Apps {
		if !slug.MatchString(a.Name) {
			return fmt.Errorf("%w: index app name %q is not a slug", ErrInvalid, a.Name)
		}
		if seen[a.Name] {
			return fmt.Errorf("%w: index lists %q twice", ErrInvalid, a.Name)
		}
		seen[a.Name] = true
		for what, v := range map[string]string{"title": a.Title, "functionality": a.Functionality, "description": a.Description} {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("%w: index app %q has no %s", ErrInvalid, a.Name, what)
			}
			if err := safeValue(a.Name+" "+what, v); err != nil {
				return err
			}
		}
		if n := len(strings.Fields(a.Functionality)); n < 2 || n > 5 {
			// Two to five words: one word is a category, six is a sentence. The field sits
			// beside the title and has to read at a glance.
			return fmt.Errorf("%w: index app %q functionality %q is %d words, want 2 to 5", ErrInvalid, a.Name, a.Functionality, n)
		}
		if err := validHomepage(a.Name, a.Homepage); err != nil {
			return err
		}
		if a.Icon != "" {
			if err := validIcon(a.Name, a.Icon); err != nil {
				return err
			}
		}
		if err := validRelease(a); err != nil {
			return err
		}
	}
	return nil
}

// identityRe is an Identity as Parse spells one.
var identityRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// versionRe is a version a list may name: it is also the version file's name, so it is one path
// element -- no separator, and no leading dot to make it `.` or `..`.
var versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// validRelease holds Version and Identity to their rule: an available app names both and an
// unavailable one neither, the version one path element and the identity a hash as Parse
// spells it.
func validRelease(a App) error {
	switch {
	case !a.Available && a.Version == "" && a.Identity == "":
		return nil
	case !a.Available:
		return fmt.Errorf("%w: index app %q names a release but is not available", ErrInvalid, a.Name)
	case a.Version == "" || a.Identity == "":
		return fmt.Errorf("%w: index app %q is available without both a version and an identity", ErrInvalid, a.Name)
	case !identityRe.MatchString(string(a.Identity)):
		return fmt.Errorf("%w: index app %q identity %q is not sha256:<64 hex>", ErrInvalid, a.Name, a.Identity)
	case !versionRe.MatchString(a.Version):
		return fmt.Errorf("%w: index app %q version %q cannot name a file", ErrInvalid, a.Name, a.Version)
	}
	return nil
}

func validHomepage(name, s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%w: index app %q homepage %q is not an https URL", ErrInvalid, name, s)
	}
	return nil
}

// validIcon accepts a base64 SVG data URI of bounded size whose markup carries no script, no
// event handler, no external reference and no foreign content. The list is of what an SVG can
// do beyond draw; a mark from a curated collection needs none of it.
func validIcon(name, s string) error {
	if !strings.HasPrefix(s, iconSVG) {
		return fmt.Errorf("%w: index app %q icon is not an SVG data URI", ErrInvalid, name)
	}
	raw, err := base64.StdEncoding.DecodeString(s[len(iconSVG):])
	if err != nil {
		return fmt.Errorf("%w: index app %q icon is not base64: %v", ErrInvalid, name, err)
	}
	if len(raw) > maxIconSize {
		return fmt.Errorf("%w: index app %q icon is %d bytes, larger than %d", ErrInvalid, name, len(raw), maxIconSize)
	}
	body := strings.ToLower(string(raw))
	if !strings.Contains(body, "<svg") {
		return fmt.Errorf("%w: index app %q icon is not SVG", ErrInvalid, name)
	}
	for _, bad := range []string{"<script", "javascript:", "<foreignobject", "<iframe", "<embed", "<object", "href=\"http", "href='http", "url(http", "@import"} {
		if strings.Contains(body, bad) {
			return fmt.Errorf("%w: index app %q icon contains %q", ErrInvalid, name, bad)
		}
	}
	if m := eventHandler.FindString(body); m != "" {
		return fmt.Errorf("%w: index app %q icon carries an event handler %q", ErrInvalid, name, strings.TrimSpace(m))
	}
	return nil
}

// eventHandler matches an SVG attribute that runs script on an event (onload=, onclick=, ...).
var eventHandler = regexp.MustCompile(`\son[a-z]+\s*=`)

// FetchIndex downloads the list at the catalog's URL and its detached signature beside it
// (`<URL>.sig`), verifies the signature over the exact bytes received, and returns the parsed
// list with those bytes, which are what a node caches. Verify-then-parse, as Fetch does and for
// the same reason.
func (c *Catalog) FetchIndex(ctx context.Context) (Index, []byte, error) {
	if c.Verifier == nil {
		return Index{}, nil, ErrNoVerifier
	}
	if c.URL == "" {
		return Index{}, nil, errors.New("manifest: no catalog URL configured")
	}
	raw, err := c.get(ctx, c.URL)
	if err != nil {
		return Index{}, nil, fmt.Errorf("manifest: fetch the catalog: %w", err)
	}
	sig, err := c.get(ctx, c.URL+sigSuffix)
	if err != nil {
		return Index{}, nil, fmt.Errorf("manifest: fetch the catalog's signature: %w", err)
	}
	if err := c.Verifier.Verify(raw, sig); err != nil {
		return Index{}, nil, fmt.Errorf("manifest: %s: %w", c.URL, err)
	}
	idx, err := ParseIndex(raw)
	if err != nil {
		return Index{}, nil, err
	}
	return idx, raw, nil
}
