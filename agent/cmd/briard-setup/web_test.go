package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/agent/install"
)

const webVersion = "v3.20261027.abc1234"

// webChannel serves a signed windows arm: stable's manifest (signed by signer, naming arts)
// under the pointer and the artifacts under the version it names. It returns the channel's URL
// and the PEM of the key the program would carry.
func webChannel(t *testing.T, platform string, arts map[string][]byte, served map[string][]byte, signer ed25519.PrivateKey) string {
	t.Helper()
	var es []install.Entry
	for name, b := range arts {
		s := sha256.Sum256(b)
		es = append(es, install.Entry{Name: name, SHA256: hex.EncodeToString(s[:]), Size: int64(len(b))})
	}
	mb, err := json.Marshal(install.Manifest{Chain: install.ChainBriard, Platform: platform, Version: webVersion, Artifacts: es})
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string][]byte{
		"briard/stable/windows-amd64/manifest.json":     mb,
		"briard/stable/windows-amd64/manifest.json.sig": ed25519.Sign(signer, mb),
	}
	for name, b := range served {
		bodies["briard/"+webVersion+"/windows-amd64/"+name] = b
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := bodies[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func key(t *testing.T) (ed25519.PrivateKey, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// The web installer runs stable's install.ps1 only once the manifest verified against the key it
// carries and the script matched the manifest; every other answer leaves no script to run.
func TestPrepareRunsOnlyAVerifiedStableScript(t *testing.T) {
	priv, pemKey := key(t)
	other, _ := key(t)
	script := []byte("# stable's install.ps1")
	ok := map[string][]byte{"install.ps1": script, "briard-agent.exe": []byte("agent")}
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"verified", webChannel(t, "windows-amd64", ok, ok, priv), true},
		{"signed by another key", webChannel(t, "windows-amd64", ok, ok, other), false},
		{"script not the manifest's", webChannel(t, "windows-amd64", ok, map[string][]byte{"install.ps1": []byte("# tampered")}, priv), false},
		{"script missing", webChannel(t, "windows-amd64", ok, nil, priv), false},
		{"manifest names no script", webChannel(t, "windows-amd64", map[string][]byte{"briard-agent.exe": []byte("agent")}, ok, priv), false},
		{"another arm's manifest", webChannel(t, "linux-amd64", ok, ok, priv), false},
	}
	for _, c := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, keyringFile), pemKey, 0o644); err != nil {
			t.Fatal(err)
		}
		env, err := prepare(context.Background(), dir, c.url, t.Logf)
		got, rerr := os.ReadFile(filepath.Join(dir, installScript))
		if c.want {
			if err != nil || string(got) != string(script) || env != nil {
				t.Errorf("%s: err %v, script %q (%v), env %q; want stable's script and no BRIARD_ARTIFACTS", c.name, err, got, rerr, env)
			}
			continue
		}
		if err == nil || rerr == nil {
			t.Errorf("%s: err %v, script on disk %v; want a refusal and no script", c.name, err, rerr == nil)
		} else if strings.Contains(err.Error(), "could not reach") {
			t.Errorf("%s: %v; the channel answered, so it is no network problem", c.name, err)
		}
	}
}

// The Store's payload is installed from, with nothing fetched; a payload with neither is refused.
func TestPrepareInstallsFromTheCarriedArm(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, installScript), []byte("#"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := prepare(context.Background(), dir, "http://127.0.0.1:1", t.Logf)
	if err != nil || len(env) != 1 || env[0] != "BRIARD_ARTIFACTS="+dir {
		t.Errorf("carried arm: env %q, err %v; want BRIARD_ARTIFACTS=%s", env, err, dir)
	}
	if _, err := prepare(context.Background(), t.TempDir(), "http://127.0.0.1:1", t.Logf); err == nil {
		t.Error("a payload with neither an install nor the key was accepted")
	}
}

// A channel that never answers is said as a network problem the household can act on; the
// transport's own words go to the log.
func TestPrepareSaysAnUnreachableChannelPlainly(t *testing.T) {
	_, pemKey := key(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, keyringFile), pemKey, 0o644); err != nil {
		t.Fatal(err)
	}
	var said []string
	_, err := prepare(context.Background(), dir, "http://127.0.0.1:1", func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) })
	want := "could not reach 127.0.0.1:1. Check this computer's internet connection, then run Briard Setup again"
	if err == nil || err.Error() != want {
		t.Errorf("err %v; want %q", err, want)
	}
	if last := said[len(said)-1]; !strings.Contains(last, "stable's manifest") || !strings.Contains(last, "127.0.0.1:1") {
		t.Errorf("the log's last line %q; want the transport's own words", last)
	}
	if _, err := os.Stat(filepath.Join(dir, installScript)); err == nil {
		t.Error("a script was left to run")
	}
}
