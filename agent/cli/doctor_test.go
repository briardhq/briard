package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"briard.io/agent/reportcard"
	"briard.io/shared/api"
)

// The agent's silence is a finding, not an empty report: an unreachable agent, and an agent too
// old to know the directive, each come back as a FAIL naming it.
func TestAgentChecks(t *testing.T) {
	cs := agentChecks(api.DirectiveOutcome{}, errors.New("dial: no such file"), "failed")
	if len(cs) != 1 || cs[0].Status != reportcard.Refuse || !strings.Contains(cs[0].Detail, "failed") {
		t.Errorf("unreachable agent = %+v, want one FAIL carrying systemd's state", cs)
	}
	cs = agentChecks(api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "unknown directive kind"}, nil, "active")
	if len(cs) != 1 || cs[0].Status != reportcard.Refuse {
		t.Errorf("agent without the verb = %+v, want one FAIL", cs)
	}
	cs = agentChecks(api.DirectiveOutcome{State: api.OutcomeDone, Detail: `[{"Name":"guest","Status":"pass","Detail":"ok","Fix":""}]`}, nil, "active")
	if len(cs) != 2 || cs[0].Name != "agent" || cs[1].Name != "guest" || cs[1].Status != reportcard.Pass {
		t.Errorf("answered = %+v, want the agent's pass followed by its checks", cs)
	}
}

// The exit code is what a script reads: 1 on any FAIL, 0 on warnings alone.
func TestPrintDoctorExitCode(t *testing.T) {
	var b bytes.Buffer
	if code := printDoctor(&b, []reportcard.Check{{Name: "clock", Status: reportcard.Warn, Detail: "x", Fix: "y"}}); code != 0 {
		t.Errorf("warnings only exited %d, want 0", code)
	}
	b.Reset()
	if code := printDoctor(&b, []reportcard.Check{{Name: "guest", Status: reportcard.Refuse, Detail: "silent", Fix: "rescue"}}); code != 1 {
		t.Errorf("a FAIL exited %d, want 1", code)
	}
	if !strings.Contains(b.String(), "[FAIL] guest") || !strings.Contains(b.String(), "-> rescue") {
		t.Errorf("output = %q, want the FAIL line and its fix", b.String())
	}
}
