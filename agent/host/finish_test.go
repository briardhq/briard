package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/agent/install"
	"briard.io/agent/selfupdate"
	"briard.io/shared/notify"
)

// The wait a failed finish asks for is decided by what the next attempt would cost: the channel
// not answering costs nothing to retry, anything else downloads the release again.
func TestRetryAfterSplitsTheUnreachableFromTheRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want time.Duration
	}{
		{"no route to the channel", fmt.Errorf("install: fetch manifest: %w", &url.Error{Op: "Get", URL: "https://get.briard.io/x", Err: errors.New("dial tcp: no route to host")}), finishRetryNet},
		{"a download cut short", fmt.Errorf("install: download nixos.qcow2.zst: %w", io.ErrUnexpectedEOF), finishRetryNet},
		{"the update's verdict: the channel", fmt.Errorf("the update to stable failed: %s%s", install.UnreachablePrefix, "install: fetch manifest: dial tcp: i/o timeout"), finishRetryNet},
		{"the frozen script's verdict", fmt.Errorf("the update to stable failed: %shttps://get.briard.io/briard/stable/windows-amd64/briard-agent.exe", frozenFetchFailed), finishRetryNet},
		{"an image that does not match its manifest", fmt.Errorf("%w: nixos.qcow2.zst sha256 a != manifest b", install.ErrArtifactMismatch), finishRetryRefused},
		{"a release refused on this host", errors.New("the update to stable failed: host bundle v3.1 refused on this host: qemu exited 1"), finishRetryRefused},
		{"too old to upgrade", fmt.Errorf("the update to stable failed: %v", install.ErrTooOldToUpgrade), finishRetryRefused},
		{"a candidate that timed out starting", fmt.Errorf("host bundle v3.2 did not start on this host and was reverted: %w", context.DeadlineExceeded), finishRetryRefused},
		{"no verdict in time", fmt.Errorf("no verdict from the update to stable: %w", os.ErrDeadlineExceeded), finishRetryRefused},
	} {
		if got := retryAfter(c.err); got != c.want {
			t.Errorf("%s: retryAfter = %s, want %s", c.name, got, c.want)
		}
	}
}

// The hold survives the process: a revert ends the agent that asked for the update, and the one
// it starts again reads the records and waits out what is left of the hour.
func TestFinishHoldWaitsOutWhatTheRecordsAsk(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name    string
		failure time.Duration // age of a revert's failure record; 0 = none
		result  string        // the verdict line; "" = none
		resAge  time.Duration
		want    time.Duration
		cause   string
	}{
		{name: "no records", want: 0},
		{name: "a revert ten minutes ago", failure: 10 * time.Minute, want: 50 * time.Minute, cause: "v3.2 did not start"},
		{name: "a revert two hours ago", failure: 2 * time.Hour, want: 0},
		{name: "a revert stamped in the future (clock set back)", failure: -time.Hour, want: finishRetryRefused, cause: "v3.2"},
		{name: "the channel unreachable 30 s ago", result: "failed " + install.UnreachablePrefix + "dial tcp: i/o timeout", resAge: 30 * time.Second, want: 90 * time.Second, cause: "could not reach"},
		{name: "the frozen fetch failed 30 s ago", result: "failed " + frozenFetchFailed + "https://x", resAge: 30 * time.Second, want: 90 * time.Second},
		{name: "a refusal ten minutes ago", result: "failed host bundle v3.2 refused on this host: qemu exited 1", resAge: 10 * time.Minute, want: 50 * time.Minute, cause: "refused"},
		{name: "a success", result: "ok updated to v3.2", resAge: time.Second, want: 0},
		{name: "a revert, and the channel since", failure: 10 * time.Minute, result: "failed " + install.UnreachablePrefix + "x", resAge: 30 * time.Second, want: 50 * time.Minute, cause: "did not start"},
	} {
		t.Run(c.name, func(t *testing.T) {
			lay := selfupdate.New(t.TempDir(), t.TempDir())
			if c.failure != 0 {
				if err := lay.WriteFailure("v3.2", "not READY in 5m0s"); err != nil {
					t.Fatal(err)
				}
				at := now.Add(-c.failure)
				if err := os.Chtimes(lay.FailurePath(), at, at); err != nil {
					t.Fatal(err)
				}
			}
			if c.result != "" {
				if err := os.WriteFile(lay.ResultPath(), []byte(c.result+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				at := now.Add(-c.resAge)
				if err := os.Chtimes(lay.ResultPath(), at, at); err != nil {
					t.Fatal(err)
				}
			}
			got, cause := finishHold(lay, now)
			if got.Round(time.Second) != c.want {
				t.Errorf("hold = %s, want %s", got, c.want)
			}
			if (got == 0) != (cause == nil) {
				t.Errorf("hold %s with cause %v: a hold always says why, and only a hold", got, cause)
			}
			if c.cause != "" && (cause == nil || !strings.Contains(cause.Error(), c.cause)) {
				t.Errorf("cause = %v, want it to say %q", cause, c.cause)
			}
			if _, err := os.Stat(lay.FailurePath()); c.failure != 0 && err != nil {
				t.Errorf("the hold consumed the failure record (%v): the agent's escalation takes it after the finish", err)
			}
		})
	}
}

// The frozen scripts' fetch-failure line is matched here, and no release can change it there:
// this keeps the two copies one.
func TestFrozenFetchFailedMatchesTheScripts(t *testing.T) {
	for _, s := range []string{"briard-update", "briard-update.ps1"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "agent", s))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), strings.TrimSpace(frozenFetchFailed)) {
			t.Errorf("%s no longer says %q when it cannot fetch the target's agent", s, frozenFetchFailed)
		}
	}
}

// A start that finds the image asserts the finish resolved: a finish that failed after the image
// landed left its alert open, and no later start runs the finish that would close it. One with no
// image setting, or no image, says nothing here.
func TestFinishInstallResolvesAtEveryStartWithTheImage(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "nixos.qcow2")
	if err := os.WriteFile(image, []byte("qcow2"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeNotifier{}
	if err := (Config{GuestImage: image}).finishInstall(context.Background(), nil, f, t.Logf); err != nil {
		t.Fatal(err)
	}
	if len(f.alerts) != 1 || f.alerts[0].Key != finishAlertKey || f.alerts[0].Kind != notify.Resolved {
		t.Errorf("alerts = %+v, want one %s resolved", f.alerts, finishAlertKey)
	}
	f = &fakeNotifier{}
	if err := (Config{}).finishInstall(context.Background(), nil, f, t.Logf); err != nil || len(f.alerts) != 0 {
		t.Errorf("no image setting: err %v, alerts %+v; want neither", err, f.alerts)
	}
}
