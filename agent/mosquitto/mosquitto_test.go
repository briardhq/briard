package mosquitto

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"briard.io/shared/manifest"
)

const image = "docker.io/library/eclipse-mosquitto@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func broker() manifest.Manifest {
	return manifest.Manifest{
		Name:    Name,
		Version: "2.1.2",
		Containers: []manifest.Container{{
			Name: "broker", Image: image, Mount: DataMount,
			Primary: true, Port: HealthPort, HealthPath: HealthPath,
		}},
	}
}

// fake records every command and every write, and models `mv` — because atomic-rename is a
// property under test: a caller that forgot it would leave the content at the .new path.
type fake struct {
	files map[string]string
	runs  [][]string
	fail  func(name string, args []string) error
}

func (f *fake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.runs = append(f.runs, append([]string{name}, args...))
	if f.fail != nil {
		if err := f.fail(name, args); err != nil {
			return []byte("boom"), err
		}
	}
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

func (f *fake) ran(argv ...string) bool {
	for _, r := range f.runs {
		if strings.Join(r, " ") == strings.Join(argv, " ") {
			return true
		}
	}
	return false
}

// TestPrepareMaterialisesTheConfig: after one Prepare the node holds the config the container
// will bind, at the final path and readable by the broker's own user.
func TestPrepareMaterialisesTheConfig(t *testing.T) {
	f := &fake{}
	if err := Prepare(context.Background(), f, broker()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	got, ok := f.files[confPath]
	if !ok {
		t.Fatalf("%s was not written (files: %v)", confPath, f.files)
	}
	// The embedded template with the bind substituted -- the ONE thing Prepare does to it. Asserted
	// as "equal to the template with the token replaced" rather than "equal to the template", so a
	// second silent substitution could not hide here.
	if want := strings.Replace(confSource, bindToken, bindAddress(broker()), 1); got != want {
		t.Error("the written config is not the embedded one with the bind substituted")
	}
	if strings.Contains(got, bindToken) {
		t.Errorf("the written config still carries %s; the broker would listen nowhere valid", bindToken)
	}
	if _, stillThere := f.files[confPath+".new"]; stillThere {
		t.Error("the config was left at the .new path; the rename did not happen")
	}
	if !f.ran("mkdir", "-p", Dir) {
		t.Errorf("%s was never created (runs: %v)", Dir, f.runs)
	}
	// 0644 and not 0600: the broker runs as an unprivileged user inside the container and has to
	// read this file. A tightened mode here is a container that cannot start.
	if !f.ran("chmod", "0644", confPath+".new") {
		t.Errorf("the config was not made readable (runs: %v)", f.runs)
	}
}

// TestPrepareTouchesNothingForOtherServices: the registry dispatches on the name, and this
// package's guard is the second half of that. Home Assistant must not get a mosquitto config.
func TestPrepareTouchesNothingForOtherServices(t *testing.T) {
	m := broker()
	m.Name = "home-assistant"
	f := &fake{}
	if err := Prepare(context.Background(), f, m); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.files) != 0 || len(f.runs) != 0 {
		t.Errorf("a service that is not %s was touched: files=%v runs=%v", Name, f.files, f.runs)
	}
}

// TestPrepareReportsAFailure: a write that fails must come back as an error, because converge
// reads that error as "do not start this service" — the answer that keeps podman from creating
// the missing bind source as a root-owned directory.
func TestPrepareReportsAFailure(t *testing.T) {
	f := &fake{fail: func(name string, _ []string) error {
		if name == "mv" {
			return errors.New("read-only file system")
		}
		return nil
	}}
	err := Prepare(context.Background(), f, broker())
	if err == nil {
		t.Fatal("a failed install of the config was reported as success")
	}
	if !strings.Contains(err.Error(), "read-only file system") {
		t.Errorf("the cause was swallowed: %v", err)
	}
}

// TestVolumesBindTheConfigReadOnly over the image's baked one, and only for this service.
func TestVolumesBindTheConfigReadOnly(t *testing.T) {
	m := broker()
	got := Volumes(m, m.Containers[0])
	want := []string{confPath + ":" + containerConf + ":ro"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Volumes = %v, want %v", got, want)
	}
	other := broker()
	other.Name = "home-assistant"
	if v := Volumes(other, other.Containers[0]); v != nil {
		t.Errorf("another service was handed mosquitto's config: %v", v)
	}
	sidecar := m.Containers[0]
	sidecar.Primary = false
	if v := Volumes(m, sidecar); v != nil {
		t.Errorf("a non-primary container was handed the broker's config: %v", v)
	}
}

// TestConfigAgreesWithWhatEveryNodeIsToldToProbe — the pairing this package's constants exist
// for. The config is what OPENS the management listener and what places the persistence file;
// the manifest is what every node PROBES and what it BINDS. An edit to one without the other is
// a health gate that can never pass, or retained state written outside the replicated volume.
func TestConfigAgreesWithWhatEveryNodeIsToldToProbe(t *testing.T) {
	// The management listener. The ADDRESS is now the templated half -- it follows the pod's
	// networking (see bindToken) -- so what this asserts is the port and the token, which is the
	// part the manifest has to agree with.
	if want := fmt.Sprintf("listener %d %s", HealthPort, bindToken); !strings.Contains(confSource, want) {
		t.Errorf("config does not open the management listener as %q", want)
	}
	if !strings.Contains(confSource, "protocol http_api") {
		t.Error("config does not speak http_api on the management listener; nothing can probe it")
	}
	// The broker itself, open to the LAN — the port the install verb tells the household about.
	if want := fmt.Sprintf("listener %d\n", MQTTPort); !strings.Contains(confSource, want) {
		t.Errorf("config does not open the broker on %d", MQTTPort)
	}
	// Persistence, into the directory the manifest binds. Without the trailing slash mosquitto
	// treats the value as a path prefix, so this is checked as mosquitto reads it.
	if want := "persistence_location " + DataMount + "/"; !strings.Contains(confSource, want) {
		t.Errorf("config does not write its database into the replicated mount (%q)", want)
	}
	if !strings.Contains(confSource, "persistence true") {
		t.Error("persistence is off; retained state would not survive a restart, let alone a failover")
	}
}

// THE BIND FOLLOWS THE POD, and getting it backwards is a LAN exposure rather than a bug: under
// host networking the pod IS the guest, so 0.0.0.0 there is every interface the household can see.
// The two modes are asserted together because each is only meaningful against the other.
func TestTheManagementBindFollowsTheNetworkMode(t *testing.T) {
	private := broker()
	private.Network = manifest.NetworkPrivate
	host := broker()
	host.Network = manifest.NetworkHost

	for _, tc := range []struct {
		name string
		m    manifest.Manifest
		want string
	}{
		{"private pod", private, "listener 9883 0.0.0.0"},
		{"host networking", host, "listener 9883 127.0.0.1"},
		// Silence means private (shared/manifest), so it must bind like private and not like host.
		{"silent manifest", broker(), "listener 9883 0.0.0.0"},
	} {
		f := &fake{}
		if err := Prepare(context.Background(), f, tc.m); err != nil {
			t.Fatalf("%s: Prepare: %v", tc.name, err)
		}
		got := f.files[confPath]
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: config does not carry %q", tc.name, tc.want)
		}
	}
	// And the one that would be a LAN exposure: host networking must never produce 0.0.0.0.
	f := &fake{}
	if err := Prepare(context.Background(), f, host); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.files[confPath], "listener 9883 0.0.0.0") {
		t.Error("a host-networked broker binds its management API to every interface the household can see")
	}
}

// TestPrepareRefusesAConfigThatLostItsPlaceholder — the guard that the before-check exists for.
// Without it a template that stopped carrying @BIND@ would be installed verbatim, and the broker
// would listen wherever the leftover literal said — which is exactly what the comment above the
// check has always claimed to prevent, and what the check did not actually do.
func TestPrepareRefusesAConfigThatLostItsPlaceholder(t *testing.T) {
	saved := confSource
	t.Cleanup(func() { confSource = saved })
	confSource = "listener 9883 0.0.0.0\n"
	err := Prepare(context.Background(), &fake{}, manifest.Manifest{Name: Name})
	if err == nil {
		t.Fatal("a config with no placeholder was installed; the broker would bind whatever it says")
	}
	if !strings.Contains(err.Error(), bindToken) {
		t.Errorf("the error does not name the missing token: %v", err)
	}
}
