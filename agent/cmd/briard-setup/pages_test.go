package main

import (
	"strings"
	"testing"
)

func TestStepIsTheLineAsASentence(t *testing.T) {
	for in, want := range map[string]string{
		"briard: fetching the TAP network driver ...\r": "Fetching the TAP network driver ...",
		"  installing the guest  ":                      "Installing the guest",
		"briard: ":                                      "",
		"2026/10/30 08:54:47 install: verified nixos.qcow2.zst (248982529 bytes)": "Verified nixos.qcow2.zst (248982529 bytes)",
		"2026/10/30 08:54:47 the guest":                                           "The guest",
		"":                                                                        "",
	} {
		if got := step(in); got != want {
			t.Errorf("step(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEndingFollowsTheExitCode(t *testing.T) {
	const log = `C:\Temp\briard-setup.log`
	if p := ending(3010, nil, log); !p.restart || p.failed || p.running || !strings.Contains(p.content, "Restart, not Shut down") {
		t.Errorf("3010: %+v; want the restart page", p)
	}
	if p := ending(0, nil, log); p.restart || p.failed || p.instruction != "Briard is installed" {
		t.Errorf("0: %+v; want installed, closing", p)
	}
	for _, c := range []struct {
		name string
		code int
		tail []string
		why  string
	}{
		{"install.ps1's Die, the error record after it", 1, []string{
			"briard: installing the TAP network driver ...",
			"briard: ERROR: the TAP driver did not install (exit 2)",
			"At C:\\briard-setup-1\\install.ps1:73 char:24",
			"+ CategoryInfo          : OperationStopped",
		}, "The TAP driver did not install (exit 2)."},
		{"this program's own", 1, []string{"briard: unpacking the install ...", "briard: ERROR: stable's manifest: connection refused"},
			"Stable's manifest: connection refused."},
		{"a refusal said, not thrown", 1618, []string{"briard: another Briard install is already running on this computer: let it finish"},
			"Another Briard install is already running on this computer: let it finish."},
		{"nothing said", 5, []string{"   "}, "The install stopped (exit code 5)."},
	} {
		p := ending(c.code, c.tail, log)
		if !p.failed || p.restart || !strings.HasPrefix(p.content, c.why+"\n\n") || !strings.Contains(p.content, log) {
			t.Errorf("%s: content %q; want %q first and the log named", c.name, p.content, c.why)
		}
		if p.details != strings.Join(c.tail, "\n") {
			t.Errorf("%s: details %q; want the tail", c.name, p.details)
		}
	}
	long := make([]string, 40)
	for i := range long {
		long[i] = "briard: line"
	}
	if p := ending(1, long, log); strings.Count(p.details, "\n") != detailLines-1 {
		t.Errorf("details keep %d lines; want %d", strings.Count(p.details, "\n")+1, detailLines)
	}
}
