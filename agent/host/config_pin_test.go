package host

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The Windows twin of briard-update (config_pin_linux_test.go), briard-update.ps1, carries the same
// second copy for the same reason. Its keyring default derives from the Windows prefix, a constant
// that is this package's prefixDir only on Windows: the channel is compared everywhere, the prefix
// where it can be.
func TestWindowsUpdateScriptDefaultsMatchTheAgents(t *testing.T) {
	os.Unsetenv("CHANNEL_URL")
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "agent", "briard-update.ps1"))
	if err != nil {
		t.Fatalf("read the shipped updater: %v", err)
	}
	script := string(b)
	cases := []struct{ what, want string }{
		{"channel", `else { '` + ConfigFromEnv().ChannelURL + `' }`},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, struct{ what, want string }{"prefix", `$Prefix = '` + prefixDir + `'`})
	}
	for _, c := range cases {
		if !strings.Contains(script, c.want) {
			t.Errorf("briard-update.ps1's %s default has drifted from the agent's; want %q", c.what, c.want)
		}
	}
}
