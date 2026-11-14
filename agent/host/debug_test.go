package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"briard.io/shared/api"
)

// The gate, and this test must fail if somebody widens it: every local-only kind is refused on the
// cloud's down-channel and accepted on the local admin door. It enumerates localOnlyKinds
// deliberately, so a verb added to that table without a thought about the cloud edge gets this
// assertion for free; a verb added to dispatch but not to the table is caught only if the
// companion test below names it. The acceptance half matters as much as the refusal: a gate that
// refused both origins would pass a refusal-only test while having broken the verb outright.
func TestLocalOnlyKindsAreRefusedFromTheCloud(t *testing.T) {
	if len(localOnlyKinds) == 0 {
		t.Fatal("localOnlyKinds is empty -- this test would assert nothing")
	}
	// No QMPSock, so the act itself also fails, with a message of its own. The state is therefore
	// not the discriminator: a refused directive and an attempted one both come back Failed, so a
	// test checking only State would pass with the gate deleted. The Detail and the journal line
	// separate "the cloud may not ask" from "this node has no guest to open", and carry the claim.
	cfg := Config{}
	for kind := range localOnlyKinds {
		var logged strings.Builder
		logf := func(f string, a ...any) { logged.WriteString(f) }

		o := cfg.dispatch(context.Background(), api.Directive{ID: "d1", Kind: kind}, originCloud, nil, nil, nil, nil, nil, nil, logf)
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

		// Same kind, local door: it reaches the handler. It may fail here for want of a guest or
		// a monitor, which is the point -- it got past the gate to a different outcome.
		o = cfg.dispatch(context.Background(), api.Directive{ID: "d1", Kind: kind}, originLocal, nil, nil, nil, nil, nil, nil, logf)
		if strings.Contains(o.Detail, "local admin socket") {
			t.Errorf("kind %q was refused on the LOCAL door too: %q", kind, o.Detail)
		}
	}
}

// A kind in the table that dispatch does not handle would be a refusal protecting nothing, and
// the reverse -- a local-only verb dispatch handles but the table does not name -- is the
// dangerous direction: it would be quietly reachable from the cloud. The debug, doctor and status
// kinds are asserted present by name, so deleting one of them from the table fails here rather
// than in the field.
func TestDebugKindsAreClassifiedLocalOnly(t *testing.T) {
	for _, kind := range []string{api.DirectiveDebugArm, api.DirectiveDebugDisarm, api.DirectiveDoctor, api.DirectiveStatus} {
		if !localOnlyKinds[kind] {
			t.Errorf("%q is dispatched but not in localOnlyKinds -- the cloud can reach it", kind)
		}
	}
}

// Origin is a parameter, and this asserts it cannot become a field on the wire type instead:
// api.Directive is what the cloud sends, and an origin field would be the sender's own claim, so a
// cloud could say "local" and walk through the gate. A marshalled Directive carries no origin.
func TestDirectiveCarriesNoOrigin(t *testing.T) {
	b, err := json.Marshal(api.Directive{Kind: api.DirectiveDebugArm})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "origin") {
		t.Errorf("api.Directive serialises an origin (%s) -- it must be the call site's, not the sender's", b)
	}
}
