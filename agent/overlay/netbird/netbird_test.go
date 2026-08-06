package netbird

import (
	"context"
	"errors"
	"strings"
	"testing"

	"briard.io/shared/api"
)

// fakeRunner records the args of each call and returns a canned status JSON for
// `status`, an optional error, and nil otherwise.
type fakeRunner struct {
	calls  [][]string
	status string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return nil, f.err
	}
	if len(args) > 0 && args[0] == "status" {
		return []byte(f.status), nil
	}
	return nil, nil
}

func (f *fakeRunner) lastMatching(sub string) []string {
	for i := len(f.calls) - 1; i >= 0; i-- {
		if len(f.calls[i]) > 0 && f.calls[i][0] == sub {
			return f.calls[i]
		}
	}
	return nil
}

func hasFlag(args []string, flag, val string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == val {
			return true
		}
	}
	return false
}

const statusTwoRelayed = `{
  "management": {"connected": true},
  "signal": {"connected": true},
  "fqdn": "briard-home.netbird.cloud",
  "netbirdIp": "100.116.35.175/16",
  "peers": {"connected": 2, "details": [
    {"status": "Connected", "connectionType": "Relayed"},
    {"status": "Connected", "connectionType": "Relayed"},
    {"status": "Idle", "connectionType": ""}
  ]}
}`

const statusOneDirect = `{
  "management": {"connected": true},
  "signal": {"connected": true},
  "fqdn": "briard-home.netbird.cloud",
  "netbirdIp": "100.116.35.175/16",
  "peers": {"connected": 2, "details": [
    {"status": "Connected", "connectionType": "P2P"},
    {"status": "Connected", "connectionType": "Relayed"}
  ]}
}`

func TestEnrollNode_flagsAndIdentity(t *testing.T) {
	f := &fakeRunner{status: statusTwoRelayed}
	p := New(Config{SetupKey: "sk-123", DaemonAddr: "unix:///run/nb.sock", DisableDNS: true}, f)

	id, err := p.EnrollNode(context.Background(), api.EnrollRequest{NodeName: "n1"})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	up := f.lastMatching("up")
	if !hasFlag(up, "--setup-key", "sk-123") {
		t.Errorf("up missing setup key: %v", up)
	}
	if !hasFlag(up, "--hostname", "n1") {
		t.Errorf("up missing hostname n1: %v", up)
	}
	if !hasFlag(up, "--daemon-addr", "unix:///run/nb.sock") {
		t.Errorf("up missing daemon-addr: %v", up)
	}
	if !contains(up, "--disable-dns") {
		t.Errorf("up missing --disable-dns: %v", up)
	}
	// Identity parsed from status, address stripped of the /16 mask
	if id.Name != "briard-home.netbird.cloud" || id.Address != "100.116.35.175" {
		t.Errorf("identity = %+v, want name+bare ip", id)
	}
}

func TestEnrollNode_noSetupKey(t *testing.T) {
	f := &fakeRunner{}
	p := New(Config{}, f)
	if _, err := p.EnrollNode(context.Background(), api.EnrollRequest{NodeName: "n1"}); err == nil {
		t.Fatal("want error when no setup key")
	}
	if len(f.calls) != 0 {
		t.Errorf("should not have run netbird without a key: %v", f.calls)
	}
}

func TestHealth_allRelayed(t *testing.T) {
	f := &fakeRunner{status: statusTwoRelayed}
	p := New(Config{}, f)
	h, err := p.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !h.Up || h.PeersUp != 2 || !h.Relayed {
		t.Errorf("health = %+v, want Up, PeersUp=2, Relayed=true", h)
	}
}

func TestHealth_directFlipsRelayedFalse(t *testing.T) {
	f := &fakeRunner{status: statusOneDirect}
	p := New(Config{}, f)
	h, err := p.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !h.Up || h.PeersUp != 2 || h.Relayed {
		t.Errorf("health = %+v, want Up, PeersUp=2, Relayed=false (one direct peer)", h)
	}
}

func TestHealth_downWhenSignalOffline(t *testing.T) {
	f := &fakeRunner{status: `{"management":{"connected":true},"signal":{"connected":false},"peers":{"details":[]}}`}
	p := New(Config{}, f)
	h, err := p.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.Up {
		t.Errorf("want Up=false when signal disconnected, got %+v", h)
	}
}

func TestUp_noSetupKeyFlag(t *testing.T) {
	f := &fakeRunner{status: statusTwoRelayed}
	p := New(Config{SetupKey: "sk-123"}, f)
	if err := p.Up(context.Background()); err != nil {
		t.Fatalf("up: %v", err)
	}
	up := f.lastMatching("up")
	if contains(up, "--setup-key") {
		t.Errorf("Up (reconnect) must not carry the setup key: %v", up)
	}
}

func TestTeardown_callsDown(t *testing.T) {
	f := &fakeRunner{}
	p := New(Config{DaemonAddr: "unix:///run/nb.sock"}, f)
	if err := p.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	down := f.lastMatching("down")
	if down == nil || !hasFlag(down, "--daemon-addr", "unix:///run/nb.sock") {
		t.Errorf("down not issued with daemon-addr: %v", f.calls)
	}
}

func TestHealth_runnerError(t *testing.T) {
	f := &fakeRunner{err: errors.New("boom")}
	p := New(Config{}, f)
	if _, err := p.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "netbird") {
		t.Errorf("want wrapped netbird error, got %v", err)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
