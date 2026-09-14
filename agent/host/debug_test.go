package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"briard.io/shared/api"
)

// THE GATE, AND IT MUST FAIL IF SOMEBODY WIDENS IT ([B.142a]). Every local-only kind is refused
// when it arrives on the cloud's down-channel and accepted on the local admin door. Enumerating
// localOnlyKinds rather than naming the two by hand is deliberate: a third local-only verb added
// to that table without a thought about the cloud edge gets this assertion for free, and one
// added to dispatch but NOT to the table fails the companion test below.
//
// The acceptance half matters as much as the refusal: a gate that refused both origins would
// pass a refusal-only test while having broken the verb outright.
func TestLocalOnlyKindsAreRefusedFromTheCloud(t *testing.T) {
	if len(localOnlyKinds) == 0 {
		t.Fatal("localOnlyKinds is empty -- this test would assert nothing")
	}
	// No QMPSock, so the act itself also fails -- with a message of its own. THE STATE IS
	// THEREFORE NOT THE DISCRIMINATOR: a refused directive and an attempted one both come back
	// Failed here, so a test checking only State would still pass with the gate deleted
	// (measured, by deleting it). What separates "the cloud may not ask" from "this node has no
	// guest to open" is the Detail and the journal line, and those carry the claim.
	cfg := Config{}
	for kind := range localOnlyKinds {
		var logged strings.Builder
		logf := func(f string, a ...any) { logged.WriteString(f) }

		o := cfg.dispatch(context.Background(), api.Directive{ID: "d1", Kind: kind}, originCloud, nil, nil, nil, nil, nil, logf)
		if o.State != api.OutcomeFailed {
			t.Errorf("kind %q from the cloud = %q, want %q", kind, o.State, api.OutcomeFailed)
		}
		if !strings.Contains(o.Detail, "local admin socket") {
			t.Errorf("kind %q refused from the cloud should say why, got detail %q", kind, o.Detail)
		}
		if o.ID != "d1" {
			t.Errorf("kind %q: refusal dropped the directive ID (%q) -- the asker cannot match it", kind, o.ID)
		}
		if !strings.Contains(logged.String(), "refused") {
			t.Errorf("kind %q refused from the cloud without a journal line: %q", kind, logged.String())
		}

		// Same kind, local door: it reaches the handler. It fails here for want of a monitor,
		// which is the point -- it got PAST the gate to a different failure.
		o = cfg.dispatch(context.Background(), api.Directive{ID: "d1", Kind: kind}, originLocal, nil, nil, nil, nil, nil, logf)
		if strings.Contains(o.Detail, "local admin socket") {
			t.Errorf("kind %q was refused on the LOCAL door too: %q", kind, o.Detail)
		}
	}
}

// A kind in the table that dispatch does not handle would be a refusal protecting nothing, and
// the reverse -- a local-only verb dispatch handles but the table does not name -- is the
// dangerous direction: it would be quietly reachable from the cloud. Both debug kinds are
// asserted present by name, so deleting one from the table fails here rather than in the field.
func TestDebugKindsAreClassifiedLocalOnly(t *testing.T) {
	for _, kind := range []string{api.DirectiveDebugArm, api.DirectiveDebugDisarm} {
		if !localOnlyKinds[kind] {
			t.Errorf("%q is dispatched but not in localOnlyKinds -- the cloud can reach it", kind)
		}
	}
}

// Origin is a PARAMETER, and this is the assertion that it cannot become a field on the wire
// type instead. api.Directive is what the cloud sends: an origin field on it would be the
// sender's own claim about itself, so a cloud could simply say "local" and walk through the gate.
// Marshalling a Directive must therefore carry no origin at all.
func TestDirectiveCarriesNoOrigin(t *testing.T) {
	b, err := json.Marshal(api.Directive{Kind: api.DirectiveDebugArm})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "origin") {
		t.Errorf("api.Directive serialises an origin (%s) -- it must be the call site's, not the sender's", b)
	}
}
