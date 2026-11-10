package mealie

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"briard.io/shared/manifest"
)

const image = "ghcr.io/mealie-recipes/mealie@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func recipes() manifest.Manifest {
	return manifest.Manifest{
		Name:    Name,
		Version: "3.30.0",
		Containers: []manifest.Container{{
			Name: "app", Image: image, Mounts: []manifest.Mount{{Tier: manifest.TierData, Dir: "app", Path: "/app/data"}},
			Primary: true, Port: 9000, HealthPath: "/api/app/about",
		}},
	}
}

// fake records every command and every write, and models `mv`, because atomic rename is a
// property under test: a caller that forgot it would leave the content at the .new path.
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

func (f *fake) ReadFile(path string) ([]byte, error) {
	if v, ok := f.files[path]; ok {
		return []byte(v), nil
	}
	return nil, errors.New("no such file")
}

// After one Prepare every setting is a file at its final path, owned by Mealie's uid and readable
// by it alone, and the directory is bound read-only where Mealie reads its settings.
func TestPrepareWritesEverySettingForMealiesUidAlone(t *testing.T) {
	x := &fake{}
	if err := Prepare(context.Background(), x, recipes()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := map[string]string{
		"ALLOW_PASSWORD_LOGIN":              "false",
		"REVERSE_PROXY_AUTH_ENABLED":        "true",
		"REVERSE_PROXY_AUTH_SIGNUP_ENABLED": "false",
		"BASE_URL":                          "http://briard-mealie.local",
	}
	for k, v := range want {
		p := SettingsDir + "/" + k
		if x.files[p] != v {
			t.Errorf("%s = %q, want %q", k, x.files[p], v)
		}
		for _, cmd := range []string{"chown 911:911 " + p + ".new", "chmod 0400 " + p + ".new"} {
			if !slices.Contains(x.runs, cmd) {
				t.Errorf("Prepare did not run %q", cmd)
			}
		}
	}
	if key := x.files[KeyPath]; len(key) != 64 {
		t.Fatalf("the secret is %q, want 64 hex characters", key)
	}
	for _, cmd := range []string{"chown 911:911 " + SettingsDir, "chmod 0500 " + SettingsDir} {
		if !slices.Contains(x.runs, cmd) {
			t.Errorf("Prepare did not run %q", cmd)
		}
	}
	m := recipes()
	if v := Volumes(m, m.Containers[0]); !slices.Equal(v, []string{SettingsDir + ":/run/secrets:ro"}) {
		t.Errorf("Volumes = %v, want the settings read-only at /run/secrets", v)
	}
}

// A second converge in the same boot keeps the secret: a running Mealie compares with the one it
// started with, and the door must go on sending that one.
func TestTheSecretOutlivesAConverge(t *testing.T) {
	x := &fake{}
	if err := Prepare(context.Background(), x, recipes()); err != nil {
		t.Fatal(err)
	}
	first := x.files[KeyPath]
	if err := Prepare(context.Background(), x, recipes()); err != nil {
		t.Fatal(err)
	}
	if x.files[KeyPath] != first {
		t.Fatalf("the secret changed across converges: %q -> %q", first, x.files[KeyPath])
	}
}

// Prepare is keyed on the name: another app gets no directory, no setting, no command.
func TestPrepareTouchesNothingForOtherApps(t *testing.T) {
	m := recipes()
	m.Name = "something-else"
	x := &fake{}
	if err := Prepare(context.Background(), x, m); err != nil {
		t.Fatal(err)
	}
	if len(x.runs) != 0 || len(x.files) != 0 {
		t.Fatalf("Prepare acted for another app: runs=%v files=%v", x.runs, x.files)
	}
	if v := Volumes(m, m.Containers[0]); v != nil {
		t.Fatalf("another app was handed Mealie's settings: %v", v)
	}
}
