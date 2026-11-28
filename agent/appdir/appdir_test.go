package appdir

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// fake records what Write runs and holds what it wrote.
type fake struct {
	files map[string]string
	ran   []string
	fail  string // a command that fails
}

func (f *fake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.ran = append(f.ran, strings.Join(append([]string{name}, args...), " "))
	if name == f.fail {
		return []byte("no"), errors.New("exit 1")
	}
	if name == "mv" {
		f.files[args[2]] = f.files[args[1]]
		delete(f.files, args[1])
	}
	return nil, nil
}

func (f *fake) WriteFile(path string, data []byte) error {
	f.files[path] = string(data)
	return nil
}

func (f *fake) ReadFile(path string) ([]byte, error) {
	v, ok := f.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(v), nil
}

// The file is given its owner and mode beside, and only then renamed over: a container that
// starts mid-write finds the old file or the whole new one, never one it cannot read.
func TestWriteRenamesOverAfterOwnerAndMode(t *testing.T) {
	f := &fake{files: map[string]string{"/run/briard/app/conf": "old"}}
	if err := Write(context.Background(), f, "/run/briard/app/conf", []byte("new"), "911:911", "0400"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"chown 911:911 /run/briard/app/conf.new",
		"chmod 0400 /run/briard/app/conf.new",
		"mv -f /run/briard/app/conf.new /run/briard/app/conf",
	}
	if !reflect.DeepEqual(f.ran, want) {
		t.Errorf("ran %q, want %q", f.ran, want)
	}
	if got := f.files["/run/briard/app/conf"]; got != "new" {
		t.Errorf("the file holds %q", got)
	}
}

// No owner named means none is set, and a step that fails leaves the old file in place.
func TestWriteWithoutAnOwnerAndAFailedStep(t *testing.T) {
	f := &fake{files: map[string]string{"/d/conf": "old"}, fail: "chmod"}
	err := Write(context.Background(), f, "/d/conf", []byte("new"), "", "0644")
	if err == nil || !strings.Contains(err.Error(), "chmod /d/conf") {
		t.Fatalf("a failed chmod answered %v", err)
	}
	if !reflect.DeepEqual(f.ran, []string{"chmod 0644 /d/conf.new"}) {
		t.Errorf("ran %q", f.ran)
	}
	if got := f.files["/d/conf"]; got != "old" {
		t.Errorf("the file holds %q after a failed write", got)
	}
}

// The secret already written is kept; anything else there, or nothing, is a new one.
func TestSecretIsKeptForTheBoot(t *testing.T) {
	f := &fake{files: map[string]string{}}
	first, fresh, err := Secret(f, "/d/key", 32)
	if err != nil || !fresh || len(first) != 64 {
		t.Fatalf("a first secret = %q, fresh %v, %v", first, fresh, err)
	}
	f.files["/d/key"] = first + "\n"
	if again, fresh, _ := Secret(f, "/d/key", 32); fresh || again != first {
		t.Errorf("the kept secret came back as %q, fresh %v", again, fresh)
	}
	f.files["/d/key"] = "short"
	if other, fresh, _ := Secret(f, "/d/key", 32); !fresh || other == first || len(other) != 64 {
		t.Errorf("a file that is no secret gave %q, fresh %v", other, fresh)
	}
}
