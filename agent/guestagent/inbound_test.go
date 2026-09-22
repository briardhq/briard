package guestagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"briard.io/agent/quadlet"
)

// ringExec is a guest with a .snapshots directory: `ls` answers with `members`, `btrfs subvolume
// show` fails for anything absent (nothing is at a fresh member's path), and everything else
// succeeds. It is the minimum a ring's rate limit and its take both read.
func ringExec(members ...string) *fakeExec {
	f := &fakeExec{files: map[string]string{
		manifestPath("home-assistant"): `{"name":"home-assistant","version":"2026.7.1"}`,
	}}
	f.runFn = func(name string, args []string) ([]byte, error) {
		if name == "ls" {
			return []byte(strings.Join(members, "\n")), nil
		}
		if len(args) > 1 && args[1] == "show" {
			return nil, errors.New("ERROR: not a subvolume")
		}
		return nil, nil
	}
	return f
}

func serve(t *testing.T, f *fakeExec, service, body string) inboundResponse {
	t.Helper()
	var out bytes.Buffer
	_ = ServeInbound(context.Background(), f, service, strings.NewReader(body), &out)
	var resp inboundResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("response %q does not parse: %v", out.String(), err)
	}
	return resp
}

// TestInboundStartingTakesAMember is the channel's whole purpose: a service about to start says
// so, and the member exists before it is answered — which is what makes the caller's wait the
// ordering guarantee ([B.143]).
func TestInboundStartingTakesAMember(t *testing.T) {
	f := ringExec()
	resp := serve(t, f, "home-assistant", `{"verb":"service.starting"}`)
	if resp.Error != "" {
		t.Fatalf("error = %q, want a member taken", resp.Error)
	}
	var took string
	for _, r := range f.runs {
		if len(r) > 4 && r[1] == "subvolume" && r[2] == "snapshot" && r[3] == "-r" {
			took = r[5]
		}
	}
	if took == "" {
		t.Fatalf("nothing was snapshotted: %v", f.runs)
	}
	svc, ok := quadlet.SnapshotMemberService(took)
	if !ok || svc != "home-assistant" {
		t.Errorf("member %q is not home-assistant's", took)
	}
	// The sidecar carries the manifest the service is running, read from the volume by the guest
	// itself — the host is not in the start path on a promotion or a crash restart.
	var meta quadlet.SnapshotMeta
	if err := json.Unmarshal([]byte(f.files[quadlet.SnapshotSidecar(took)]), &meta); err != nil {
		t.Fatalf("the member has no readable sidecar: %v", err)
	}
	if meta.Trigger != quadlet.TriggerStart {
		t.Errorf("trigger = %q, want %q", meta.Trigger, quadlet.TriggerStart)
	}
	if !strings.Contains(meta.Manifest, "2026.7.1") {
		t.Errorf("sidecar manifest = %q, want the version on the volume", meta.Manifest)
	}
}

// TestInboundStartingRateLimitsACrashLoop: a crash loop restarts every few seconds and would fill
// the picker with hundreds of identical entries. The member that matters is the FIRST — taken
// before the bad change — so the rest are skipped and that one survives.
//
// It is also the abuse bound. The caller is a container, so this is what stops anything running
// as the service from filling the replicated volume on purpose ([B.155] by a new road).
func TestInboundStartingRateLimitsACrashLoop(t *testing.T) {
	recent := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Now().Add(-5*time.Second))
	f := ringExec(strings.TrimPrefix(recent, quadlet.SnapshotsDir))
	resp := serve(t, f, "home-assistant", `{"verb":"service.starting"}`)
	if resp.Error != "" {
		t.Fatalf("error = %q, want a quiet skip", resp.Error)
	}
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "subvolume" && r[2] == "snapshot" {
			t.Fatalf("a second member was taken inside the floor: %v", f.runs)
		}
	}
	if !strings.Contains(resp.Detail, "still current") {
		t.Errorf("detail = %q, want it to say why nothing was taken", resp.Detail)
	}
}

// TestInboundStartingTakesOneOutsideTheFloor is the other half: the rate limit must not be a mute
// button. A member older than the floor does not suppress the next one.
func TestInboundStartingTakesOneOutsideTheFloor(t *testing.T) {
	old := quadlet.SnapshotMember("home-assistant", quadlet.TriggerStart, time.Now().Add(-2*time.Hour))
	f := ringExec(strings.TrimPrefix(old, quadlet.SnapshotsDir))
	serve(t, f, "home-assistant", `{"verb":"service.starting"}`)
	var took bool
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "subvolume" && r[2] == "snapshot" {
			took = true
		}
	}
	if !took {
		t.Fatalf("an hours-old ring suppressed a new member: %v", f.runs)
	}
}

// TestInboundIgnoresAnotherServicesMembers: the rate limit is per service. A busy Home Assistant
// must not be able to suppress the broker's members, or one service's crash loop silently costs
// every other service its history.
func TestInboundIgnoresAnotherServicesMembers(t *testing.T) {
	recent := quadlet.SnapshotMember("mosquitto", quadlet.TriggerStart, time.Now().Add(-5*time.Second))
	f := ringExec(strings.TrimPrefix(recent, quadlet.SnapshotsDir))
	serve(t, f, "home-assistant", `{"verb":"service.starting"}`)
	var took bool
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "subvolume" && r[2] == "snapshot" {
			took = true
		}
	}
	if !took {
		t.Fatalf("another service's member suppressed this one: %v", f.runs)
	}
}

// TestInboundIgnoresStrangersInTheSnapshotsDir: `.snapshots` is a directory on a volume a human
// can reach. Anything whose name the ring did not write is not a ring member, and must not be
// able to stop one being taken — a stray directory called `backup` should cost nothing.
func TestInboundIgnoresStrangersInTheSnapshotsDir(t *testing.T) {
	f := ringExec("a-human-copy", "home-assistant-preupgrade", "notes.txt")
	serve(t, f, "home-assistant", `{"verb":"service.starting"}`)
	var took bool
	for _, r := range f.runs {
		if len(r) > 2 && r[1] == "subvolume" && r[2] == "snapshot" {
			took = true
		}
	}
	if !took {
		t.Fatalf("a stranger's directory entry suppressed a member: %v", f.runs)
	}
}

// TestInboundRequestCannotNameItsService is the trust boundary, asserted.
//
// The socket is bind-mounted into the service's container, so everything running as that service
// can reach it — for Home Assistant, every custom component the household ever installed. The
// identity therefore comes from the LISTENER and nothing in the request may change it: a request
// that could name its own service would let any container act for any other.
func TestInboundRequestCannotNameItsService(t *testing.T) {
	f := ringExec()
	serve(t, f, "home-assistant", `{"verb":"service.starting","service":"mosquitto","path":"/etc"}`)
	for _, r := range f.runs {
		for _, a := range r {
			if strings.Contains(a, "mosquitto") || a == "/etc" {
				t.Fatalf("the request body reached a command: %v", f.runs)
			}
		}
	}
	var took string
	for _, r := range f.runs {
		if len(r) > 5 && r[2] == "snapshot" {
			took = r[5]
		}
	}
	if svc, _ := quadlet.SnapshotMemberService(took); svc != "home-assistant" {
		t.Errorf("member %q was taken for the service the REQUEST named, not the listener's", took)
	}
}

// TestInboundRefusesAnUnknownVerb: the set is closed, and the name comes back because the
// realistic cause is a wrapper newer than the agent beneath it.
func TestInboundRefusesAnUnknownVerb(t *testing.T) {
	f := ringExec()
	resp := serve(t, f, "home-assistant", `{"verb":"data.restore"}`)
	if resp.Error == "" {
		t.Fatal("an unknown verb was accepted")
	}
	if !strings.Contains(resp.Error, "data.restore") {
		t.Errorf("error = %q, want the rejected verb named", resp.Error)
	}
	if len(f.runs) != 0 {
		t.Errorf("an unknown verb still ran something: %v", f.runs)
	}
}

// TestInboundAlwaysAnswers: the caller is holding its service stopped until this replies, so
// every path has to write a response — a handler that returned without answering would hang a
// household's Home Assistant on its own start.
func TestInboundAlwaysAnswers(t *testing.T) {
	for _, body := range []string{
		`{"verb":"service.starting"}`,
		`{"verb":"nonsense"}`,
		`not json at all`,
		``,
	} {
		var out bytes.Buffer
		_ = ServeInbound(context.Background(), ringExec(), "home-assistant", strings.NewReader(body), &out)
		if out.Len() == 0 {
			t.Errorf("request %q got no response; the caller would wait forever", body)
			continue
		}
		if !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			t.Errorf("request %q got an unterminated response %q; a line reader would wait forever", body, out.String())
		}
	}
}

// TestInboundRejectsAServiceNameThatIsAPath: the service name becomes a path element, and the
// flag that carries it is ours — but a rendering bug that let `../` through would point every
// derived path somewhere else entirely.
func TestInboundRejectsAServiceNameThatIsAPath(t *testing.T) {
	f := ringExec()
	resp := serve(t, f, "../../etc", `{"verb":"service.starting"}`)
	if resp.Error == "" {
		t.Fatal("a traversing service name was accepted")
	}
	if len(f.runs) != 0 {
		t.Errorf("a traversing service name still ran something: %v", f.runs)
	}
}
