package main

import (
	"strings"
	"testing"
)

func TestSaidIsTheLineAsASentence(t *testing.T) {
	for in, want := range map[string]string{
		"briard: fetching the TAP network driver ...\r": "Fetching the TAP network driver ...",
		"  installing the guest  ":                      "Installing the guest",
		"briard: ":                                      "",
		"2026/10/30 08:54:47 install: verified nixos.qcow2.zst (248982529 bytes)": "Verified nixos.qcow2.zst (248982529 bytes)",
		"2026/10/30 08:54:47 the guest":                                           "The guest",
		"":                                                                        "",
	} {
		if got := said(in); got != want {
			t.Errorf("said(%q) = %q, want %q", in, got, want)
		}
	}
}

// A real install's output, in order (a Windows Home machine's), and the stage the window shows for
// each line: "" keeps the one before.
var realInstall = []struct{ line, stage string }{
	{"briard: unpacking the install ...", "Getting ready ..."},
	{"briard: bootstrapping the installer agent from https://get.briard.io (briard/stable) ...", "Getting ready ..."},
	{"briard: fetching the TAP network driver (tap-windows6, OpenVPN's signed build) ...", "Adding Briard's network adapter ..."},
	{"briard: installing the TAP network driver ...", "Adding Briard's network adapter ..."},
	{"briard: turning on the Windows Hypervisor Platform (starts at the restart) ...", "Turning on a Windows feature Briard needs ..."},
	{"WARNING: Restart is suppressed because NoRestart is specified.", ""},
	{"briard: checking host readiness ...", "Checking this computer can run Briard ..."},
	{"[PASS  ] admin     running as an administrator", ""},
	{"[WARN  ] hypervisor Windows Hypervisor Platform is off", ""},
	{"           -> the install enables it; it starts at the restart", ""},
	{"[PASS  ] disk      51.2 GB free", ""},
	{"result: this machine can run briard", ""},
	{"briard: fetching + verifying the signed artifact set (briard/stable + vm) ...", files},
	{"2026/11/09 04:39:22 install: briard/stable manifest verified — release v3.26110807.50ad632, 9 artifact(s) to fetch", files},
	{"2026/11/09 04:39:22 install: verified briard-agent.exe (14597816 bytes)", files},
	{"2026/11/09 04:39:23 install: vm/vm.26110807.50ad632 manifest verified — release vm.26110807.50ad632, 1 artifact(s) to fetch", files},
	{"2026/11/09 04:39:33 install: verified nixos.qcow2.zst (249648728 bytes)", files},
	{`briard: nightly backups go to Documents\Briard Backup`, "Installing Briard ..."},
	{`briard: restricting C:\ProgramData\briard to SYSTEM and Administrators`, "Installing Briard ..."},
	{"briard: letting Users run briard-agent.exe", "Installing Briard ..."},
	{"briard: registering the briard-agent service", "Installing Briard ..."},
	{`briard: laying the warden at C:\ProgramData\briard\warden\briard-warden.exe`, "Installing Briard ..."},
	{"briard: registering the briard-warden service", "Installing Briard ..."},
	{"briard: installing the agent's update script and its task", "Installing Briard ..."},
	{"briard: installing the notification-area icon", "Installing Briard ..."},
	{"briard: adding Briard to the Start menu", "Installing Briard ..."},
	{"briard: allowing the VM to reach the backup store (TCP 7791, briard-agent.exe only)", "Installing Briard ..."},
	{`briard: adding C:\ProgramData\briard\bin to the system PATH`, "Installing Briard ..."},
	{"briard: building the network bridge over Ethernet (the network drops for a moment)", network},
	{"briard: the bridge is built; Windows must restart before briard can use it", network},
	{"briard: removing the staging directory", "Finishing ..."},
	{"briard: flushing the installed files to disk", "Finishing ..."},
	{"briard: installed; Windows must restart before briard can start", "Finishing ..."},
	{"briard: installed. Windows must restart once for the network bridge and the hypervisor.", "Finishing ..."},
	{"briard: after the restart, log on: Briard finishes starting and opens your Briard page in your browser.", ""},
}

const (
	files   = "Getting Briard's files (this can take a few minutes) ..."
	network = "Connecting Briard to your network (the connection drops for a moment) ..."
)

func TestStepIsTheStageInPlainWords(t *testing.T) {
	at := 0 // the stage shown: a real install never goes back one
	for _, c := range realInstall {
		got := step(c.line)
		if got != c.stage {
			t.Errorf("step(%q) = %q, want %q", c.line, got, c.stage)
			continue
		}
		for i, s := range stages {
			if s.show == got {
				if i < at {
					t.Errorf("step(%q) went back to %q", c.line, got)
				}
				at = i
			}
		}
	}
	for _, l := range []string{"briard: ", "", "2026/11/09 04:39:22 something new", "briard: a line added later"} {
		if got := step(l); got != "" {
			t.Errorf("step(%q) = %q; a line that names no stage changes nothing", l, got)
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
