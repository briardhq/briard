package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"briard.io/shared/api"
)

// `briard update -catalog` asks the agent for one read of the list and prints what it lists. It
// moves nothing, so a release to converge to (-to) or the other chain (-vm) beside it is refused
// before anything is asked, rather than silently dropped.
func TestUpdateCatalogAsksTheAgentForARead(t *testing.T) {
	sock, seen := fakeAgent(t, api.DirectiveOutcome{State: api.OutcomeDone, Detail: "read https://x/catalog/dev.json: 41 apps listed, 5 installable"})
	var out, errOut bytes.Buffer
	if code := Main(context.Background(), []string{"update", "-catalog", "-sock", sock}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "5 installable") {
		t.Errorf("stdout = %q, want the agent's answer", out.String())
	}
	for _, args := range [][]string{{"-to", "dev"}, {"-vm"}, {"extra"}} {
		out.Reset()
		errOut.Reset()
		if code := Main(context.Background(), append([]string{"update", "-catalog", "-sock", sock}, args...), &out, &errOut); code != 2 {
			t.Errorf("update -catalog %v: exit = %d, want 2 (stderr %q)", args, code, errOut.String())
		}
	}
	if d := seen(); len(d) != 1 || d[0].Kind != api.DirectiveCatalogRead {
		t.Errorf("the agent was asked %+v, want one catalog-read", d)
	}
}

// A failed read exits 1 with the agent's reason.
func TestUpdateCatalogReportsAFailedRead(t *testing.T) {
	sock, _ := fakeAgent(t, api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "no route (the list read before stands)"})
	var out, errOut bytes.Buffer
	if code := Main(context.Background(), []string{"update", "-catalog", "-sock", sock}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "the list read before stands") {
		t.Errorf("exit = %d, stderr %q", code, errOut.String())
	}
}
