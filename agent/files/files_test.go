package files

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"briard.io/shared/manifest"
)

const image = "docker.io/gtstef/filebrowser@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func browser() manifest.Manifest {
	return manifest.Manifest{
		Name:    Name,
		Version: "2.0.2",
		Containers: []manifest.Container{{
			Name: "app", Image: image, Mounts: []manifest.Mount{{Tier: manifest.TierCache, Dir: "state", Path: "/home/filebrowser/data"}},
			Primary: true, Port: 80, HealthPath: "/health",
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

// After one Prepare the node holds the config, at its final path, carrying the secret the proxy
// signs with -- root's alone -- and the view's root exists for the container to bind.
func TestPrepareWritesTheConfigWithTheBootsSecret(t *testing.T) {
	x := &fake{}
	if err := Prepare(context.Background(), x, browser()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	key, conf := x.files[KeyPath], x.files[confPath]
	if len(key) != 64 {
		t.Fatalf("the secret is %q, want 64 hex characters", key)
	}
	if !strings.Contains(conf, `secret: "`+key+`"`) || strings.Contains(conf, secretToken) {
		t.Fatalf("the config does not carry the secret in place of the placeholder:\n%s", conf)
	}
	for _, want := range []string{"mkdir -p " + ViewDir, "chmod 0600 " + KeyPath + ".new", "chmod 0600 " + confPath + ".new"} {
		if !slices.Contains(x.runs, want) {
			t.Errorf("Prepare did not run %q: %v", want, x.runs)
		}
	}
	if _, ok := x.files[faviconPath]; !ok {
		t.Error("the mark the config names was not written")
	}
}

// A second converge in the same boot keeps the secret: a running Files verifies with the one it
// started with, and the proxy must go on signing with that one.
func TestTheSecretOutlivesAConverge(t *testing.T) {
	x := &fake{}
	if err := Prepare(context.Background(), x, browser()); err != nil {
		t.Fatal(err)
	}
	first := x.files[KeyPath]
	if err := Prepare(context.Background(), x, browser()); err != nil {
		t.Fatal(err)
	}
	if x.files[KeyPath] != first || !strings.Contains(x.files[confPath], first) {
		t.Fatalf("the secret changed across converges: %q -> %q", first, x.files[KeyPath])
	}
}

// Prepare is keyed on the name: another app gets no directory, no secret, no command.
func TestPrepareTouchesNothingForOtherApps(t *testing.T) {
	m := browser()
	m.Name = "something-else"
	x := &fake{}
	if err := Prepare(context.Background(), x, m); err != nil {
		t.Fatal(err)
	}
	if len(x.runs) != 0 || len(x.files) != 0 {
		t.Fatalf("Prepare acted for another app: runs=%v files=%v", x.runs, x.files)
	}
	if v := Volumes(m, m.Containers[0]); v != nil {
		t.Fatalf("another app was handed the view: %v", v)
	}
}
