package manifest

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><circle cx="8" cy="8" r="7" fill="#c33"/></svg>`

func iconURI(svg string) string {
	return iconSVG + base64.StdEncoding.EncodeToString([]byte(svg))
}

func goodApp() App {
	return App{
		Name:          "paperless-ngx",
		Title:         "Paperless-ngx",
		Functionality: "document management system",
		Description:   "Scan or drop in letters and bills; it reads them and makes every word searchable.",
		Icon:          iconURI(testSVG),
		Homepage:      "https://docs.paperless-ngx.com",
	}
}

func rawIndex(t *testing.T, idx Index) []byte {
	t.Helper()
	b, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestParseIndexGood(t *testing.T) {
	second := goodApp()
	second.Name, second.Title, second.Icon = "mealie", "Mealie", ""
	idx, err := ParseIndex(rawIndex(t, Index{Apps: []App{goodApp(), second}}))
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	if len(idx.Apps) != 2 || idx.Apps[0].Name != "paperless-ngx" || idx.Apps[1].Name != "mealie" {
		t.Fatalf("parsed %+v; want both apps in file order", idx.Apps)
	}
}

func TestParseIndexRejectsUnknownFields(t *testing.T) {
	raw := []byte(`{"apps":[{"name":"a","title":"A","functionality":"two words","description":"d","homepage":"https://a.example","status":"available"}]}`)
	if _, err := ParseIndex(raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a status field parsed: %v; availability is a manifest beside the index, never a field", err)
	}
}

func TestParseIndexRejectsOversize(t *testing.T) {
	a := goodApp()
	a.Description = strings.Repeat("x", maxIndexSize)
	if _, err := ParseIndex(rawIndex(t, Index{Apps: []App{a}})); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an index over %d bytes parsed: %v", maxIndexSize, err)
	}
}

func TestIndexValidateRejects(t *testing.T) {
	cases := map[string]func(*App){
		"name not a slug":        func(a *App) { a.Name = "Paperless NGX" },
		"no title":               func(a *App) { a.Title = " " },
		"no functionality":       func(a *App) { a.Functionality = "" },
		"one-word functionality": func(a *App) { a.Functionality = "documents" },
		"six-word functionality": func(a *App) { a.Functionality = "a system for managing all documents" },
		"no description":         func(a *App) { a.Description = "" },
		"line break in title":    func(a *App) { a.Title = "Paper\nless" },
		"http homepage":          func(a *App) { a.Homepage = "http://docs.paperless-ngx.com" },
		"no homepage":            func(a *App) { a.Homepage = "" },
		"homepage not a URL":     func(a *App) { a.Homepage = "docs.paperless-ngx.com" },
		"icon not a data URI":    func(a *App) { a.Icon = "<svg/>" },
		"icon a PNG":             func(a *App) { a.Icon = "data:image/png;base64,iVBORw0KGgo=" },
		"icon not base64":        func(a *App) { a.Icon = iconSVG + "!!!" },
		"icon not SVG":           func(a *App) { a.Icon = iconURI("<html></html>") },
		"icon too large":         func(a *App) { a.Icon = iconURI(testSVG + strings.Repeat(" ", maxIconSize)) },
		"icon with script":       func(a *App) { a.Icon = iconURI(`<svg><script>alert(1)</script></svg>`) },
		"icon with handler":      func(a *App) { a.Icon = iconURI(`<svg onload="alert(1)"></svg>`) },
		"icon with javascript:":  func(a *App) { a.Icon = iconURI(`<svg><a href="javascript:alert(1)"/></svg>`) },
		"icon with external ref": func(a *App) { a.Icon = iconURI(`<svg><image href="https://x.example/a.png"/></svg>`) },
		"icon with foreign HTML": func(a *App) { a.Icon = iconURI(`<svg><foreignObject><div/></foreignObject></svg>`) },
		"icon with @import":      func(a *App) { a.Icon = iconURI(`<svg><style>@import url(x.css)</style></svg>`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := goodApp()
			mutate(&a)
			err := Index{Apps: []App{a}}.Validate()
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted; want ErrInvalid, got %v", err)
			}
		})
	}
	t.Run("duplicate name", func(t *testing.T) {
		err := Index{Apps: []App{goodApp(), goodApp()}}.Validate()
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("two entries for one slug accepted: %v", err)
		}
	})
	t.Run("empty index", func(t *testing.T) {
		if err := (Index{}).Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("an index of nothing accepted: %v", err)
		}
	})
}

func TestIndexAcceptsRealIconShapes(t *testing.T) {
	// The shapes the curated collection actually ships: an XML declaration, a style block with
	// classes, a gradient referenced by fragment, and xmlns URLs (which are not hrefs).
	for name, svg := range map[string]string{
		"xml declaration": `<?xml version="1.0" encoding="UTF-8"?>` + testSVG,
		"style classes":   `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{fill:#fff}</style><path class="a" d="M0 0h1v1z"/></svg>`,
		"gradient by fragment": `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><defs><linearGradient id="g"><stop offset="0"/></linearGradient></defs>` +
			`<circle r="1" style="fill:url(#g)"/></svg>`,
		"media query in style": `<svg xmlns="http://www.w3.org/2000/svg"><style>:root{color:#000}@media(prefers-color-scheme:dark){:root{color:#fff}}</style><path d="M0 0h1v1z" fill="currentColor"/></svg>`,
	} {
		t.Run(name, func(t *testing.T) {
			a := goodApp()
			a.Icon = iconURI(svg)
			if err := (Index{Apps: []App{a}}).Validate(); err != nil {
				t.Fatalf("a shape the collection ships was refused: %v", err)
			}
		})
	}
}

// indexServer serves one index and its detached signature, signed with a throwaway key.
func indexServer(t *testing.T, body []byte, sign bool, tamper func([]byte) []byte) (*httptest.Server, Verifier) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	sig := ed25519.Sign(priv, body)
	served := body
	if tamper != nil {
		served = tamper(append([]byte(nil), body...))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+IndexName, func(w http.ResponseWriter, r *http.Request) { w.Write(served) })
	mux.HandleFunc("/"+IndexName+sigSuffix, func(w http.ResponseWriter, r *http.Request) {
		if !sign {
			http.NotFound(w, r)
			return
		}
		w.Write(sig)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, ring{pub}
}

func TestFetchIndexVerified(t *testing.T) {
	body := rawIndex(t, Index{Apps: []App{goodApp()}})
	srv, v := indexServer(t, body, true, nil)
	c := &Catalog{BaseURL: srv.URL, Verifier: v, Client: srv.Client()}
	idx, raw, err := c.FetchIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}
	if len(idx.Apps) != 1 || idx.Apps[0].Name != "paperless-ngx" {
		t.Fatalf("parsed %+v", idx.Apps)
	}
	if string(raw) != string(body) {
		t.Fatal("the raw bytes returned are not the bytes served; a node caches what was signed")
	}
}

func TestFetchIndexRefusesTamperedDocument(t *testing.T) {
	body := rawIndex(t, Index{Apps: []App{goodApp()}})
	srv, v := indexServer(t, body, true, func(b []byte) []byte {
		return []byte(strings.Replace(string(b), "Paperless-ngx", "Paperless-nix", 1))
	})
	c := &Catalog{BaseURL: srv.URL, Verifier: v, Client: srv.Client()}
	if _, _, err := c.FetchIndex(context.Background()); err == nil {
		t.Fatal("a document altered after signing was accepted")
	}
}

func TestFetchIndexRefusesUnsigned(t *testing.T) {
	body := rawIndex(t, Index{Apps: []App{goodApp()}})
	srv, v := indexServer(t, body, false, nil)
	c := &Catalog{BaseURL: srv.URL, Verifier: v, Client: srv.Client()}
	if _, _, err := c.FetchIndex(context.Background()); err == nil {
		t.Fatal("an index with no signature was accepted")
	}
}

func TestFetchIndexFailsClosedWithoutAKeyring(t *testing.T) {
	body := rawIndex(t, Index{Apps: []App{goodApp()}})
	srv, _ := indexServer(t, body, true, nil)
	c := &Catalog{BaseURL: srv.URL, Client: srv.Client()}
	if _, _, err := c.FetchIndex(context.Background()); !errors.Is(err, ErrNoVerifier) {
		t.Fatalf("with no trust root, got %v; want ErrNoVerifier before any byte is fetched", err)
	}
}
