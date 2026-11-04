package cli

import (
	"os"
	"path/filepath"
	"testing"

	"briard.io/agent/host"
)

// ⚠️ THE PIN, same discipline as the unit names and alertMarker above: the CLI reads the service
// cache the AGENT writes, so the two must name the same directory. A test may import agent/host
// where the shipped code may not.
func TestAppCacheDirMatchesTheAgents(t *testing.T) {
	os.Unsetenv("APP_CACHE")
	t.Setenv("BRIARD_CONFIG", filepath.Join(t.TempDir(), "no-such-config.env"))
	if got := host.ConfigFromEnv().AppCache; got != appCacheDir {
		t.Errorf("the CLI reads %q while the agent writes %q", appCacheDir, got)
	}
}

// The cache holds each app's manifest VERBATIM, so the name comes from the manifest's own
// parser rather than from a second reader of the format. install.sh used to `grep -o` for
// `"name":"..."` and take the first match, which is a different implementation of a format whose
// identity is its content hash.
func TestInstalledAppsReadsTheManifests(t *testing.T) {
	dir := t.TempDir()
	write := func(file, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("home-assistant.json", `{"schema":1,"name":"home-assistant","containers":[{"name":"ha","image":"x","primary":true}]}`)
	write("mosquitto.json", `{"schema":1,"name":"mosquitto","containers":[{"name":"m","image":"y","primary":true}]}`)
	// ⚠️ A manifest that will not parse is STILL an installed app, under a vaguer name: it is
	// on the node, and staying silent about it because we could not read its title is the one
	// answer that is certainly wrong.
	write("broken.json", `{not json at all`)
	write("notes.txt", `ignored: not a manifest`)

	got := installedApps(dir)
	want := []string{"broken", "home-assistant", "mosquitto"}
	if len(got) != len(want) {
		t.Fatalf("installedApps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("installedApps = %v, want %v (sorted)", got, want)
		}
	}
	// An absent cache is the shipped state of a fresh node, not an error.
	if got := installedApps(filepath.Join(t.TempDir(), "nothing-here")); got != nil {
		t.Errorf("an absent cache read as %v, want nothing", got)
	}
}

// The sentence, including the empty case -- which is empty ON PURPOSE. A fresh node hears "no
// app is installed yet" from the installer, where it is news; a household running the verb a
// month later does not need telling twice.
func TestInstalledLine(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"home-assistant"}, "home-assistant is installed on this machine"},
		{[]string{"home-assistant", "mosquitto"}, "home-assistant and mosquitto are installed on this machine"},
		{[]string{"a", "b", "c"}, "a, b and c are installed on this machine"},
	} {
		if got := installedLine(c.in); got != c.want {
			t.Errorf("installedLine(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
