package freshrss

import (
	"context"
	"strings"
	"testing"

	"briard.io/shared/manifest"
	"briard.io/shared/routes"
)

func reader() manifest.Manifest {
	return manifest.Manifest{Name: Name, Version: "1.31.0", Containers: []manifest.Container{{Name: "app", Primary: true, Port: 80, HealthPath: "/api/"}}}
}

// fake records every command and every write, and models `mv`, because the rename is a property
// under test: a caller that forgot it would leave the file at the .new path.
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

// Prepare writes the file at its final path: trusting the one address it is given and nothing
// wider, naming the user from the reverse proxy's header and dropping the other one FreshRSS reads.
func TestPrepareTrustsTheOneAddressAndMapsTheHeader(t *testing.T) {
	f := &fake{}
	if err := Prepare(context.Background(), f, reader(), "10.89.7.1"); err != nil {
		t.Fatal(err)
	}
	conf, ok := f.files[ConfigPath]
	if !ok || len(f.files) != 1 {
		t.Fatalf("the file is not (only) at %s: %v", ConfigPath, f.files)
	}
	for _, want := range []string{
		"SetEnv TRUSTED_PROXY 10.89.7.1/32",
		"RequestHeader unset X-WebAuth-User",
		`RequestHeader set Remote-User "expr=%{req:X-Forwarded-User}"`,
	} {
		if !strings.Contains(conf, want+"\n") {
			t.Errorf("the file lacks %q:\n%s", want, conf)
		}
	}
}

// With no address for the reverse proxy there is nothing to trust: refused, so the app is not
// started with the image's own trust. Another app is not FreshRSS's to prepare.
func TestPrepareRefusesWithNoAddressAndIgnoresOtherApps(t *testing.T) {
	if err := Prepare(context.Background(), &fake{}, reader(), ""); err == nil {
		t.Fatal("prepared with no address to trust")
	}
	f := &fake{}
	if err := Prepare(context.Background(), f, manifest.Manifest{Name: "navidrome"}, ""); err != nil || len(f.runs) != 0 {
		t.Fatalf("another app was prepared as FreshRSS: %v %v", err, f.runs)
	}
}

// The file is bound read-only over the image's hook, in FreshRSS's container alone.
func TestTheFileIsBoundOverTheHook(t *testing.T) {
	m := reader()
	got := Volumes(m, m.Containers[0])
	if len(got) != 1 || got[0] != "/run/briard/freshrss/freshrss.conf:/var/www/FreshRSS/p/i/.htaccess:ro" {
		t.Errorf("binds %v", got)
	}
	if v := Volumes(manifest.Manifest{Name: "navidrome"}, m.Containers[0]); v != nil {
		t.Errorf("another app gets FreshRSS's file: %v", v)
	}
}

// Only the Google Reader API is open: never the pages, where the header signs in, nor the rest of
// the API.
func TestOnlyTheGoogleReaderAPIIsOpen(t *testing.T) {
	a := routes.App{Name: Name, GateByAddress: true, Open: Open}
	if err := (routes.Table{Apps: []routes.App{a}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if !a.IsOpen("/api/greader.php/accounts/ClientLogin") || !a.IsOpen("/api/greader.php/reader/api/0/stream/contents") {
		t.Error("the Google Reader API is not open")
	}
	for _, p := range []string{"/i/", "/api/", "/api/fever.php", "/api/query.php", "/api/pshb.php", "/api/greader.php", "/"} {
		if a.IsOpen(p) {
			t.Errorf("%s is open", p)
		}
	}
}
