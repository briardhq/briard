package guestfirmware

import (
	"context"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
)

// fakeExec records the commands the firmware shells out to and returns canned output; stands in
// for the guest. runFn, if set, overrides output/err (the systemctl fakes below use it).
type fakeExec struct {
	files    map[string]string
	runs     [][]string
	hostname string
	output   []byte
	err      error
	runFn    func(name string, args []string) ([]byte, error)
}

func (f *fakeExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.runs = append(f.runs, append([]string{name}, args...))
	if f.runFn != nil {
		return f.runFn(name, args)
	}
	return f.output, f.err
}

func (f *fakeExec) WriteFile(path string, data []byte) error {
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = string(data)
	return nil
}

// A file this fake was never given reads back as os.ErrNotExist rather than as "".
func (f *fakeExec) ReadFile(path string) ([]byte, error) {
	v, ok := f.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(v), nil
}

func (f *fakeExec) Sethostname(name string) error {
	f.hostname = name
	return nil
}

// dial wires a caller's Conn to the FIRMWARE's Serve over an in-memory pipe: the verbs go over
// the real framing, so a test proves the dispatch and not just the handler.
func dial(t *testing.T, x Executor) *Conn {
	t.Helper()
	cconn, sconn := net.Pipe()
	go Serve(context.Background(), sconn, x)
	c := NewConn(cconn)
	t.Cleanup(func() { c.Close() })
	return c
}

// THE FIRMWARE SERVES THE PROTOCOL AND NOTHING ELSE ([B.139]). A guest running it has not been
// dressed, so a verb belonging to the pushed agent is refused with what is actually wrong -- and
// the handshake says the same thing in advance, which is what makes the host dress it rather
// than drive it.
func TestFirmwareRefusesEveryVerbOutsideTheProtocol(t *testing.T) {
	binDirs(t)
	c := dial(t, &fakeExec{})
	var h Hello
	if err := c.Call(context.Background(), VerbHello, nil, &h); err != nil {
		t.Fatal(err)
	}
	if h.Version != GuestProtocol {
		t.Errorf("version = %d, want %d", h.Version, GuestProtocol)
	}
	if !reflect.DeepEqual(h.Capabilities, Capabilities) {
		t.Errorf("capabilities = %v, want the firmware's five: %v", h.Capabilities, Capabilities)
	}
	if h.Bundle != "" {
		t.Errorf("bundle = %q, want empty: the firmware is what runs before any push", h.Bundle)
	}
	// One verb from each family the pushed agent owns: bring-up, services, observation.
	for _, verb := range []string{"storage.node", "service.start", "sys.resources", "os.system"} {
		err := c.Call(context.Background(), verb, struct{}{}, nil)
		if err == nil {
			t.Errorf("%s was served by the firmware", verb)
			continue
		}
		if !strings.Contains(err.Error(), "has not been dressed") {
			t.Errorf("%s refused with %q; the refusal must say the guest is undressed, not just that the verb is unknown", verb, err)
		}
	}
}
