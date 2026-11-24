package navidrome

import (
	"context"
	"strings"
	"testing"

	"briard.io/shared/manifest"
	"briard.io/shared/routes"
)

func music() manifest.Manifest {
	return manifest.Manifest{Name: Name, Version: "0.64.2", Containers: []manifest.Container{{Name: "app", Primary: true, Port: 4533, HealthPath: "/ping"}}}
}

// fake records every command and every write, and models `mv`, because the rename is a property
// under test: a caller that forgot it would leave the settings at the .new path.
type fake struct {
	files map[string]string
	runs  []string
}

func (f *fake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.runs = append(f.runs, strings.Join(append([]string{name}, args...), " "))
	if name == "mv" && len(args) == 3 && args[0] == "-f" {
		if v, ok := f.files[args[1]]; ok {
			delete(f.files, args[1])
			f.files[args[2]] = v
		}
	}
	return nil, nil
}

func (f *fake) WriteFile(path string, data []byte) error {
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = string(data)
	return nil
}

// Prepare writes the settings at their final path, trusting the one address it is given and
// nothing wider, with the telemetry and sharing off and the logout on the dashboard.
func TestPrepareTrustsTheOneAddressAndTurnsOffTelemetry(t *testing.T) {
	f := &fake{}
	if err := Prepare(context.Background(), f, music(), "10.89.7.1"); err != nil {
		t.Fatal(err)
	}
	conf, ok := f.files[ConfigPath]
	if !ok || len(f.files) != 1 {
		t.Fatalf("the settings are not (only) at %s: %v", ConfigPath, f.files)
	}
	for _, want := range []string{
		`TrustedSources = "10.89.7.1/32"`,
		`UserHeader = "X-Forwarded-User"`,
		`LogoutURL = "http://briard.local/"`,
		"EnableInsightsCollector = false",
		"EnableSharing = false",
	} {
		if !strings.Contains(conf, want+"\n") {
			t.Errorf("the settings lack %q:\n%s", want, conf)
		}
	}
}

// With no address for the reverse proxy there is nothing to trust, and a Navidrome started with
// an empty list would take no header at all: refused, so the app is not started.
func TestPrepareRefusesWithNoAddressAndIgnoresOtherApps(t *testing.T) {
	if err := Prepare(context.Background(), &fake{}, music(), ""); err == nil {
		t.Fatal("prepared with no address to trust")
	}
	f := &fake{}
	if err := Prepare(context.Background(), f, manifest.Manifest{Name: "mealie"}, ""); err != nil || len(f.runs) != 0 {
		t.Fatalf("another app was prepared as Navidrome: %v %v", err, f.runs)
	}
}

// Only the Subsonic API is open: never /auth, where anyone could make the first admin of a
// Navidrome with no user yet, nor the pages, the native API or a share.
func TestOnlyTheSubsonicAPIIsOpen(t *testing.T) {
	a := routes.App{Name: Name, GateByAddress: true, Open: Open}
	if err := (routes.Table{Apps: []routes.App{a}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if !a.IsOpen("/rest/ping.view") {
		t.Error("the Subsonic API is not open")
	}
	for _, p := range []string{"/auth/createAdmin", "/auth/login", "/app/", "/api/user", "/share/x", "/"} {
		if a.IsOpen(p) {
			t.Errorf("%s is open", p)
		}
	}
}
