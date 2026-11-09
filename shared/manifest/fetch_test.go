package manifest

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// ring is a minimal Verifier over one Ed25519 key — the same primitive selfupdate.Keyring
// implements, kept local so this package's tests don't import the agent.
type ring struct{ pub ed25519.PublicKey }

func (r ring) Verify(artifact, sig []byte) error {
	if r.pub == nil {
		return errors.New("no keys")
	}
	if !ed25519.Verify(r.pub, artifact, sig) {
		return errors.New("bad signature")
	}
	return nil
}

// served is one published catalog: a list naming one app's version, that version's file beside
// it, and the list's signature, made with a throwaway key over the list as listed.
type served struct {
	line App    // the list's line for the app
	file []byte // the bytes served at <name>/<version>.json
	sign bool   // serve the list's signature
	// tamper mutates the served LIST after signing, to model a compromised mirror.
	tamper func([]byte) []byte
}

// listing is the line a list carries for exactly these manifest bytes.
func listing(t *testing.T, body []byte) App {
	t.Helper()
	m, id, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	a := goodApp()
	a.Name, a.Available, a.Version, a.Identity = m.Name, true, m.Version, id
	return a
}

func catalogServer(t *testing.T, s served) (*Catalog, *[]string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	list := rawIndex(t, Index{Apps: []App{s.line}})
	sig := ed25519.Sign(priv, list)
	if s.tamper != nil {
		list = s.tamper(list)
	}
	var asked []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case "/catalog/latest.json":
			w.Write(list)
		case "/catalog/latest.json.sig":
			if !s.sign {
				http.NotFound(w, r)
				return
			}
			w.Write(sig)
		case "/catalog/" + s.line.Name + "/" + s.line.Version + ".json":
			w.Write(s.file)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Catalog{URL: srv.URL + "/catalog/latest.json", Verifier: ring{pub}, Client: srv.Client()}, &asked
}

func TestFetchVerified(t *testing.T) {
	body := raw(t, good())
	c, asked := catalogServer(t, served{line: listing(t, body), file: body, sign: true})

	m, id, got, err := c.Fetch(context.Background(), "home-assistant")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m.Name != "home-assistant" || string(got) != string(body) {
		t.Fatalf("got %q and %d bytes", m.Name, len(got))
	}
	_, want, _ := Parse(body)
	if id != want {
		t.Fatalf("identity = %q, want the hash of the served bytes %q", id, want)
	}
	// The version file is read from beside the list, by the version the list names.
	if w := "/catalog/home-assistant/2026.7.1.json"; !slices.Contains(*asked, w) {
		t.Fatalf("asked %v; want %s", *asked, w)
	}
}

// TestFetchRefusesAFileTheListDoesNotName: the list is signed and genuine, and the version file
// beside it is a valid manifest -- just not the one the list names (a mirror swapped the pinned
// image). Only the hash in the signed line catches it, and it must, before the file is parsed.
func TestFetchRefusesAFileTheListDoesNotName(t *testing.T) {
	body := raw(t, good())
	swapped := []byte(strings.Replace(string(body), hex64, hex64b, 1))
	c, _ := catalogServer(t, served{line: listing(t, body), file: swapped, sign: true})
	_, _, _, err := c.Fetch(context.Background(), "home-assistant")
	if err == nil || !strings.Contains(err.Error(), "the signed list names") {
		t.Fatalf("err = %v; want the file refused against the list's identity", err)
	}
}

// TestFetchRefusesTamperedList: the mirror served a valid signature over the ORIGINAL list and a
// modified one. Verification is over the raw bytes, so this must fail.
func TestFetchRefusesTamperedList(t *testing.T) {
	body := raw(t, good())
	c, _ := catalogServer(t, served{line: listing(t, body), file: body, sign: true, tamper: func(b []byte) []byte {
		return []byte(strings.Replace(string(b), "Paperless-ngx", "Paperless-nix", 1))
	}})
	if _, _, _, err := c.Fetch(context.Background(), "home-assistant"); err == nil {
		t.Fatal("Fetch accepted a list that did not match its signature")
	}
}

// TestFetchRefusesUnsigned: no signature served at all.
func TestFetchRefusesUnsigned(t *testing.T) {
	body := raw(t, good())
	c, _ := catalogServer(t, served{line: listing(t, body), file: body})
	if _, _, _, err := c.Fetch(context.Background(), "home-assistant"); err == nil {
		t.Fatal("Fetch accepted an unsigned list")
	}
}

// TestFetchRefusesAnUnlistedApp: an app with no line, or a line with no version, has nothing to
// install, whatever files sit beside the list.
func TestFetchRefusesAnUnlistedApp(t *testing.T) {
	body := raw(t, good())
	c, _ := catalogServer(t, served{line: listing(t, body), file: body, sign: true})
	if _, _, _, err := c.Fetch(context.Background(), "mosquitto"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	asked := goodApp()
	asked.Name = "home-assistant"
	c, _ = catalogServer(t, served{line: asked, file: body, sign: true})
	if _, _, _, err := c.Fetch(context.Background(), "home-assistant"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for an app listed but not available", err)
	}
}

// TestFetchFailsClosedWithoutAKeyring: no trust root means every manifest is refused, and the
// network is never touched. Fail-open here would make the catalog's signature decorative.
func TestFetchFailsClosedWithoutAKeyring(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer srv.Close()
	c := &Catalog{URL: srv.URL + "/catalog/latest.json", Client: srv.Client()}
	if _, _, _, err := c.Fetch(context.Background(), "home-assistant"); !errors.Is(err, ErrNoVerifier) {
		t.Fatalf("err = %v, want ErrNoVerifier", err)
	}
	if reached {
		t.Fatal("Fetch hit the network with no keyring configured")
	}
}

// TestFetchRefusesNameMismatch: a file listed under one name that calls itself another would
// install under a handle nobody asked for — and, since the name is the subvolume, would aim it
// at a different app's data. The list names it, and the hash matches: the name is what's wrong.
func TestFetchRefusesNameMismatch(t *testing.T) {
	m := good()
	m.Name = "something-else"
	body := raw(t, m)
	line := listing(t, body)
	line.Name = "home-assistant"
	c, _ := catalogServer(t, served{line: line, file: body, sign: true})
	_, _, _, err := c.Fetch(context.Background(), "home-assistant")
	if err == nil || !strings.Contains(err.Error(), "declares name") {
		t.Fatalf("err = %v, want a name-mismatch refusal", err)
	}
}

// TestFetchRejectsUnsafeName keeps "../" out of the URL we build.
func TestFetchRejectsUnsafeName(t *testing.T) {
	c := &Catalog{URL: "https://briard.io/catalog/latest.json", Verifier: ring{}}
	for _, name := range []string{"../secrets", "a/b", "", "UPPER"} {
		if _, _, _, err := c.Fetch(context.Background(), name); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Fetch(%q) err = %v, want ErrInvalid", name, err)
		}
	}
}
