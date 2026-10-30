package manifest

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
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

// catalogServer serves one manifest and its detached signature, signed with a throwaway key.
// tamper mutates the served document AFTER signing, to model a compromised mirror.
func catalogServer(t *testing.T, name string, body []byte, sign bool, tamper func([]byte) []byte) (*httptest.Server, Verifier) {
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
	mux.HandleFunc("/"+name+".json", func(w http.ResponseWriter, r *http.Request) { w.Write(served) })
	mux.HandleFunc("/"+name+".json.sig", func(w http.ResponseWriter, r *http.Request) {
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

func TestFetchVerified(t *testing.T) {
	body := raw(t, good())
	srv, v := catalogServer(t, "home-assistant", body, true, nil)
	c := &Catalog{BaseURL: srv.URL, Verifier: v, Client: srv.Client()}

	m, id, _, err := c.Fetch(context.Background(), "home-assistant")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m.Name != "home-assistant" {
		t.Fatalf("got %q", m.Name)
	}
	_, want, _ := Parse(body)
	if id != want {
		t.Fatalf("identity = %q, want the hash of the served bytes %q", id, want)
	}
}

// TestFetchRefusesTamperedDocument: the mirror served a valid signature over the ORIGINAL bytes
// and a modified document. Verification is over the raw bytes, so this must fail — and it must
// fail before Parse, because parsing untrusted input is the larger attack surface.
func TestFetchRefusesTamperedDocument(t *testing.T) {
	body := raw(t, good())
	srv, v := catalogServer(t, "home-assistant", body, true, func(b []byte) []byte {
		return []byte(strings.Replace(string(b), hex64, hex64b, 1)) // swap the pinned image
	})
	c := &Catalog{BaseURL: srv.URL, Verifier: v, Client: srv.Client()}
	if _, _, _, err := c.Fetch(context.Background(), "home-assistant"); err == nil {
		t.Fatal("Fetch accepted a document that did not match its signature")
	}
}

// TestFetchRefusesUnsigned: no signature served at all.
func TestFetchRefusesUnsigned(t *testing.T) {
	srv, v := catalogServer(t, "home-assistant", raw(t, good()), false, nil)
	c := &Catalog{BaseURL: srv.URL, Verifier: v, Client: srv.Client()}
	if _, _, _, err := c.Fetch(context.Background(), "home-assistant"); err == nil {
		t.Fatal("Fetch accepted an unsigned manifest")
	}
}

// TestFetchFailsClosedWithoutAKeyring: no trust root means every manifest is refused, and the
// network is never touched. Fail-open here would make the catalog's signature decorative.
func TestFetchFailsClosedWithoutAKeyring(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer srv.Close()
	c := &Catalog{BaseURL: srv.URL, Client: srv.Client()}
	if _, _, _, err := c.Fetch(context.Background(), "home-assistant"); !errors.Is(err, ErrNoVerifier) {
		t.Fatalf("err = %v, want ErrNoVerifier", err)
	}
	if reached {
		t.Fatal("Fetch hit the network with no keyring configured")
	}
}

// TestFetchRefusesNameMismatch: a manifest served at one name that calls itself another would
// install under a handle nobody asked for — and, since the name is the subvolume, would aim it
// at a different service's data.
func TestFetchRefusesNameMismatch(t *testing.T) {
	m := good()
	m.Name = "something-else"
	srv, v := catalogServer(t, "home-assistant", raw(t, m), true, nil)
	c := &Catalog{BaseURL: srv.URL, Verifier: v, Client: srv.Client()}
	_, _, _, err := c.Fetch(context.Background(), "home-assistant")
	if err == nil || !strings.Contains(err.Error(), "declares name") {
		t.Fatalf("err = %v, want a name-mismatch refusal", err)
	}
}

// TestFetchRejectsUnsafeName keeps "../" out of the URL we build.
func TestFetchRejectsUnsafeName(t *testing.T) {
	c := &Catalog{BaseURL: "https://briard.io/catalog", Verifier: ring{}}
	for _, name := range []string{"../secrets", "a/b", "", "UPPER"} {
		if _, _, _, err := c.Fetch(context.Background(), name); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Fetch(%q) err = %v, want ErrInvalid", name, err)
		}
	}
}

// Has is existence by HEAD: 200 is published, 404 is not, and anything else is an error the
// caller must not read as "not published" -- a mirror that is down knows nothing.
func TestHasIsExistenceAndNothingElse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/home-assistant.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("asked with %s; want HEAD", r.Method)
		}
	})
	mux.HandleFunc("/broken.json", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Catalog{BaseURL: srv.URL, Client: srv.Client()}
	ctx := context.Background()
	if ok, err := c.Has(ctx, "home-assistant"); err != nil || !ok {
		t.Errorf("a published manifest: ok=%v err=%v", ok, err)
	}
	if ok, err := c.Has(ctx, "paperless-ngx"); err != nil || ok {
		t.Errorf("an unpublished manifest: ok=%v err=%v; want false, nil", ok, err)
	}
	if _, err := c.Has(ctx, "broken"); err == nil {
		t.Error("a 502 read as an answer; it is an error")
	}
	if _, err := c.Has(ctx, "../index"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a non-slug name: %v; want ErrInvalid", err)
	}
}
