package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ⚠️ THE ONE DUPLICATED DEFAULT IN THE PRODUCT, pinned here because it is deliberate and therefore
// easy to let drift ([B.157], owner's call).
//
// scripts/agent/briard-update is FROZEN, agent-independent shell: it is what replaces the agent, so
// it must not ask the agent anything. Its unit hands it config.env as an EnvironmentFile, but
// config.env carries a key only when an operator named one -- so on an ordinary install there is
// nothing to read and the script needs its own answer for the two values it cannot do without.
//
// Two literals, one meaning. This test is what keeps them one: a release that moves the channel or
// the keyring in config.go and forgets the script would leave every node updating from the old
// place, which is the failure that cannot be fixed by an update.
func TestUpdateScriptDefaultsMatchTheAgents(t *testing.T) {
	// Cleared, so what is compared is the agent's DEFAULT rather than whatever this process
	// happens to have in its environment.
	os.Unsetenv("CHANNEL_URL")
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "agent", "briard-update"))
	if err != nil {
		t.Fatalf("read the shipped updater: %v", err)
	}
	script := string(b)
	for _, c := range []struct{ what, want string }{
		{"channel", `CHANNEL="${CHANNEL_URL:-` + ConfigFromEnv().ChannelURL + `}"`},
		{"keyring", `KEYRING="${UPDATE_KEYRING:-` + prefixDir + `/keyring.pem}"`},
	} {
		if !strings.Contains(script, c.want) {
			t.Errorf("briard-update's %s default has drifted from the agent's; want the line %q", c.what, c.want)
		}
	}
}
