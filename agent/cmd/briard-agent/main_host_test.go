package main

import (
	"errors"
	"strings"
	"testing"
)

func TestUninstalledIsPlainWhenItWorked(t *testing.T) {
	said := []string{"powering the guest off cleanly", "stopping the briard-agent service", "briard is uninstalled"}
	kept := uninstalled(said, false, nil)
	if !strings.HasPrefix(kept, "Briard is uninstalled.") || !strings.Contains(kept, dataTree) || strings.Contains(kept, "guest") {
		t.Errorf("kept: %q; want the plain outcome naming where the data stays, and no log line", kept)
	}
	if strings.Contains(kept, "Restart") {
		t.Errorf("kept: %q; nothing was left for the restart", kept)
	}
	if del := uninstalled(said, true, nil); !strings.Contains(del, "are deleted") || strings.Contains(del, dataTree) {
		t.Errorf("deleted: %q; want the data said deleted", del)
	}
	// The line the Windows uninstall writes when files wait for the restart.
	left := append(said, "3 files and folders still in use go at the next restart of Windows; restart it to finish")
	if got := uninstalled(left, false, nil); !strings.Contains(got, "Restart Windows to finish") {
		t.Errorf("files left: %q; want the restart asked for", got)
	}
}

func TestUninstalledKeepsTheLogWhenItFailed(t *testing.T) {
	said := []string{"powering the guest off cleanly", "stopping the briard-agent service"}
	got := uninstalled(said, false, errors.New("stopping the briard-agent service: timeout"))
	if !strings.HasPrefix(got, "Briard was not fully uninstalled: stopping the briard-agent service: timeout") ||
		!strings.Contains(got, helpLink) || !strings.HasSuffix(got, strings.Join(said, "\n\n")) {
		t.Errorf("failed: %q; want the error, where to ask, and everything the uninstall did", got)
	}
}
