package host

import (
	"strings"
	"testing"
)

// The agent must be able to tell a bug reporter which build it is. The id was
// always stamped (buildVersion, via -ldflags) and threaded into NodeStatus, but was
// never surfaced to a human -- so the first triage question had no answer the reporter
// could reach. These pin the two cases that matter for that purpose.
func TestVersionBanner(t *testing.T) {
	t.Run("stamped build names the version", func(t *testing.T) {
		got := versionBanner("1.4.2-abc123")
		if !strings.Contains(got, "1.4.2-abc123") {
			t.Errorf("banner must contain the version, got %q", got)
		}
	})

	// The failable half: an unstamped binary must SAY it is unstamped rather than
	// logging a blank where the version goes. A blank reads as a formatting bug and
	// invites the reporter to skip the field; "development build" is a fact.
	t.Run("unstamped build says so", func(t *testing.T) {
		got := versionBanner("")
		if got == "" {
			t.Fatal("banner must not be empty")
		}
		// Must not look like the stamped form. Checked against that exact prefix
		// rather than the bare word "version": the unstamped text legitimately
		// contains "no version stamped", and a looser match fires on it.
		if strings.Contains(got, "starting, version ") {
			t.Errorf("unstamped banner must not claim a version, got %q", got)
		}
		if !strings.Contains(strings.ToLower(got), "development build") {
			t.Errorf("unstamped banner must name itself a development build, got %q", got)
		}
	})
}

// The banner is logged by Run before anything can fail, so that a bug report from an
// agent that died during bring-up still carries the build id. Asserting on Run itself
// would mean booting QEMU; this pins the contract Run depends on instead -- that the
// value it passes is Config.Version, which is buildVersion unless AGENT_VERSION
// overrides it.
func TestVersionBannerUsesConfiguredVersion(t *testing.T) {
	t.Setenv("AGENT_VERSION", "from-env-9.9.9")
	cfg := ConfigFromEnv()
	if cfg.Version != "from-env-9.9.9" {
		t.Fatalf("Config.Version = %q, want the AGENT_VERSION override", cfg.Version)
	}
	if got := versionBanner(cfg.Version); !strings.Contains(got, "from-env-9.9.9") {
		t.Errorf("banner should carry Config.Version, got %q", got)
	}
}
