package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fetcherFixture wires a Fetcher over a temp layout + a keyring holding one signer, and serves
// `artifact` at /agent. Returns the fetcher, the signer, and the artifact bytes.
func fetcherFixture(t *testing.T, artifact []byte) (*Fetcher, ed25519.PrivateKey, *httptest.Server) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := NewKeyring(pubPEM(t, pub))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	l := New(filepath.Join(root, "state"), filepath.Join(root, "run"))
	if err := os.MkdirAll(l.Base, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed a committed binary so we can prove refuse-and-stay leaves it untouched.
	if err := os.WriteFile(l.AgentPath(), []byte("COMMITTED"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(artifact)
	}))
	t.Cleanup(srv.Close)
	return &Fetcher{Layout: l, Keyring: kr}, priv, srv
}

func TestFetchAndStageStagesAndArmsAVerifiedArtifact(t *testing.T) {
	artifact := []byte("a fresh, correctly-signed briard-agent")
	f, priv, srv := fetcherFixture(t, artifact)
	sig := ed25519.Sign(priv, artifact)

	if err := f.FetchAndStage(context.Background(), srv.URL+"/agent", sig); err != nil {
		t.Fatalf("FetchAndStage of a good artifact: %v", err)
	}
	if !f.Layout.NextStaged() {
		t.Error("verified artifact was not staged to agent.next")
	}
	if !f.Layout.Armed() {
		t.Error("verified artifact was staged but the trial was not armed")
	}
	got, _ := os.ReadFile(f.Layout.NextPath())
	if string(got) != string(artifact) {
		t.Errorf("staged bytes = %q, want the artifact", got)
	}
}

// The load-bearing negative: a tampered artifact must be refused AND leave the layout pristine —
// nothing staged, nothing armed, committed binary intact. [[verification-assertions-must-fail]]
func TestFetchAndStageRefusesTamperedArtifactAndKeepsCurrent(t *testing.T) {
	artifact := []byte("the served bytes")
	f, priv, srv := fetcherFixture(t, artifact)
	// Sign DIFFERENT bytes, so the signature won't verify the served artifact.
	sig := ed25519.Sign(priv, []byte("what the cloud thought it was signing"))

	err := f.FetchAndStage(context.Background(), srv.URL+"/agent", sig)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered artifact: got %v, want ErrBadSignature", err)
	}
	if f.Layout.NextStaged() {
		t.Error("a refused artifact was staged — refuse-and-stay violated")
	}
	if f.Layout.Armed() {
		t.Error("a refused artifact armed a trial — refuse-and-stay violated")
	}
	if got, _ := os.ReadFile(f.Layout.AgentPath()); string(got) != "COMMITTED" {
		t.Errorf("committed binary changed on a refused update: %q", got)
	}
}

func TestFetchAndStageRefusesUnsignedArtifact(t *testing.T) {
	f, _, srv := fetcherFixture(t, []byte("x"))
	if err := f.FetchAndStage(context.Background(), srv.URL+"/agent", nil); !errors.Is(err, ErrUnsigned) {
		t.Errorf("empty sig: got %v, want ErrUnsigned", err)
	}
	if f.Layout.NextStaged() || f.Layout.Armed() {
		t.Error("an unsigned artifact was staged/armed")
	}
}

func TestFetchAndStageRefusesNon200(t *testing.T) {
	f, priv, _ := fetcherFixture(t, nil)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer down.Close()
	err := f.FetchAndStage(context.Background(), down.URL+"/agent", ed25519.Sign(priv, []byte("x")))
	if err == nil {
		t.Fatal("a 404 artifact fetch should error")
	}
	if f.Layout.NextStaged() || f.Layout.Armed() {
		t.Error("a failed fetch staged/armed something")
	}
}

// No keyring wired == fail closed (never fetch-and-trust without a ring).
func TestFetchAndStageNoKeyringFailsClosed(t *testing.T) {
	root := t.TempDir()
	l := New(filepath.Join(root, "state"), filepath.Join(root, "run"))
	os.MkdirAll(l.Base, 0o755)
	f := &Fetcher{Layout: l} // Keyring nil
	if err := f.FetchAndStage(context.Background(), "http://unused", []byte("sig")); !errors.Is(err, ErrNoKeys) {
		t.Errorf("nil keyring: got %v, want ErrNoKeys", err)
	}
}
