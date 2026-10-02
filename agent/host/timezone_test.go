package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Timezone detection. Every case builds a fake /etc, so these assert what the
// agent will read off a real machine rather than what this machine happens to be set to -- a
// test that read the developer's own zone would pass everywhere and prove nothing.

// fakeRoot builds a root dir; a "" value means "do not create this file".
func fakeRoot(t *testing.T, localtimeTarget, etcTimezone string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if localtimeTarget != "" {
		if err := os.Symlink(localtimeTarget, filepath.Join(root, "etc", "localtime")); err != nil {
			t.Fatal(err)
		}
	}
	if etcTimezone != "" {
		if err := os.WriteFile(filepath.Join(root, "etc", "timezone"), []byte(etcTimezone+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLocalTimezone(t *testing.T) {
	for _, c := range []struct {
		name       string
		localtime  string
		etcTZ      string
		tzEnv      string
		want       string
		wantReason string
	}{
		{
			name: "the systemd convention", localtime: "/usr/share/zoneinfo/Europe/Athens",
			want: "Europe/Athens", wantReason: "what timedatectl leaves behind",
		},
		{
			name: "a relative symlink", localtime: "../usr/share/zoneinfo/America/Los_Angeles",
			want: "America/Los_Angeles", wantReason: "distros write these relative as often as not",
		},
		{
			name: "the posix/ subtree", localtime: "/usr/share/zoneinfo/posix/Europe/Athens",
			want: "Europe/Athens", wantReason: "same zone, a path prefix that is not part of its name",
		},
		{
			name: "Debian's plain file when there is no symlink", etcTZ: "Europe/Athens",
			want: "Europe/Athens", wantReason: "a pre-systemd or minimal image",
		},
		{
			name: "the symlink wins over the file", localtime: "/usr/share/zoneinfo/Europe/Athens",
			etcTZ: "America/Los_Angeles", want: "Europe/Athens",
			wantReason: "the symlink is what the machine's clock actually follows",
		},
		{
			name: "TZ overrides everything", localtime: "/usr/share/zoneinfo/Europe/Athens",
			tzEnv: "America/Los_Angeles", want: "America/Los_Angeles",
			wantReason: "TZ already overrides this process's own clock, so it must not be contradicted",
		},
		{
			name: "a machine genuinely on UTC", localtime: "/usr/share/zoneinfo/UTC",
			want: "UTC", wantReason: "a headless install on UTC is not a fault",
		},
		{
			name: "nothing configured", want: "",
			wantReason: "no answer, which the cloud reports as a home it cannot schedule",
		},
		{
			name: "a zone that does not exist", localtime: "/usr/share/zoneinfo/Mars/Olympus_Mons",
			want: "", wantReason: "a zone the cloud cannot load is worse than no zone -- it looks scheduled",
		},
		{
			name:      "an unloadable symlink falls through to the file",
			localtime: "/usr/share/zoneinfo/Mars/Olympus_Mons", etcTZ: "Europe/Athens",
			want: "Europe/Athens", wantReason: "one broken source must not hide a good one",
		},
		{
			name: "a copied zone blob rather than a symlink", etcTZ: "",
			want: "", wantReason: "a regular /etc/localtime carries no name to read",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("TZ", c.tzEnv)
			root := fakeRoot(t, c.localtime, c.etcTZ)
			if got := localTimezone(root); got != c.want {
				t.Errorf("localTimezone = %q, want %q (%s)", got, c.want, c.wantReason)
			}
		})
	}
}

// A regular file at /etc/localtime -- what a container image or an old installer leaves -- is
// read as no answer rather than crashing or returning a path.
func TestLocalTimezoneIgnoresACopiedZoneFile(t *testing.T) {
	t.Setenv("TZ", "")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "localtime"), []byte("TZif2\x00binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := localTimezone(root); got != "" {
		t.Errorf("localTimezone = %q, want \"\" -- a zone blob has no name in it", got)
	}
}

type fakeZoneGuest struct {
	old  bool
	err  error
	told []string
}

func (f *fakeZoneGuest) SetTimezone(_ context.Context, zone string) error {
	f.told = append(f.told, zone)
	return f.err
}
func (f *fakeZoneGuest) SupportsTimezone() bool { return !f.old }

// TestTheGuestIsToldTheHostsZone, and nothing else: no zone, an older guest and a guest that
// refuses all leave it on UTC with a log line, never a failed bring-up (pushTimezone has no
// error to return).
func TestTheGuestIsToldTheHostsZone(t *testing.T) {
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	g := &fakeZoneGuest{}
	pushTimezone(context.Background(), g, "Europe/Athens", logf)
	if len(g.told) != 1 || g.told[0] != "Europe/Athens" {
		t.Fatalf("told %v, want Europe/Athens", g.told)
	}
	for _, tc := range []struct {
		name string
		g    *fakeZoneGuest
		zone string
	}{
		{"no zone on the host", &fakeZoneGuest{}, ""},
		{"an older guest", &fakeZoneGuest{old: true}, "Europe/Athens"},
		{"the guest refuses", &fakeZoneGuest{err: errors.New("not in this image's database")}, "Europe/Athens"},
	} {
		lines = nil
		pushTimezone(context.Background(), tc.g, tc.zone, logf)
		if len(lines) != 1 || !strings.Contains(lines[0], "stays on UTC") {
			t.Errorf("%s: logged %v, want one line saying the guest stays on UTC", tc.name, lines)
		}
		if tc.zone == "" || tc.g.old {
			if len(tc.g.told) != 0 {
				t.Errorf("%s: the guest was told %v", tc.name, tc.g.told)
			}
		}
	}
}
