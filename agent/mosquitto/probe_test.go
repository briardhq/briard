package mosquitto

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// execFake models `podman exec` closely enough to test the probe's SEQUENCE: what each command
// returns, in what order they ran, and which of them failed.
type execFake struct {
	runs [][]string
	// answers maps a subscribed topic to what the broker hands back. A topic that is absent
	// makes mosquitto_sub fail, which is what an empty retained topic really does (exit 27).
	answers map[string]string
	// failPublish and failSignal model the two halves of the write going wrong.
	failPublish, failSignal bool
}

func (f *execFake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.runs = append(f.runs, append([]string{name}, args...))
	argv := strings.Join(args, " ")
	switch {
	case strings.Contains(argv, "mosquitto_pub"):
		if f.failPublish {
			return []byte("connection refused"), errors.New("exit 1")
		}
	case strings.Contains(argv, "SIGUSR1"):
		if f.failSignal {
			return []byte("no such container"), errors.New("exit 125")
		}
	case strings.Contains(argv, "mosquitto_sub"):
		topic := args[len(args)-5] // ... -t <topic> -C 1 -W n
		if v, ok := f.answers[topic]; ok {
			return []byte(v + "\n"), nil
		}
		return []byte("Timed out"), errors.New("exit 27")
	}
	return nil, nil
}

func (f *execFake) WriteFile(string, []byte) error  { return nil }
func (f *execFake) ReadFile(string) ([]byte, error) { return nil, errors.New("no such file") }
func (f *execFake) index(match string) int {
	for i, r := range f.runs {
		if strings.Contains(strings.Join(r, " "), match) {
			return i
		}
	}
	return -1
}

func serving(extra map[string]string) *execFake {
	answers := map[string]string{versionTopic: "mosquitto version 2.1.2"}
	for k, v := range extra {
		answers[k] = v
	}
	return &execFake{answers: answers}
}

// TestWriteIsPublishedRetainedAndThenPersisted. The ORDER is the assertion: asking the broker to
// write its database before the token is published would flush everything except the one fact the
// gate is about.
func TestWriteIsPublishedRetainedAndThenPersisted(t *testing.T) {
	f := serving(nil)
	if _, err := Probe(context.Background(), f, "briard-mosquitto-broker", "tok-1"); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	pub, sig := f.index("mosquitto_pub"), f.index("SIGUSR1")
	if pub < 0 || sig < 0 {
		t.Fatalf("the write did not publish and persist (runs: %v)", f.runs)
	}
	if sig < pub {
		t.Errorf("the broker was told to persist BEFORE the token was published (runs: %v)", f.runs)
	}
	argv := strings.Join(f.runs[pub], " ")
	if !strings.Contains(argv, "-r") {
		t.Errorf("the token was not published RETAINED, so nothing would survive a restart: %s", argv)
	}
	if !strings.Contains(argv, ProbeTopic) || !strings.Contains(argv, "tok-1") {
		t.Errorf("the publish does not carry the token on the probe topic: %s", argv)
	}
}

// TestAReadWritesNothing: Assess must not disturb what it is judging.
func TestAReadWritesNothing(t *testing.T) {
	f := serving(map[string]string{ProbeTopic: "tok-1"})
	s, err := Probe(context.Background(), f, "briard-mosquitto-broker", "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if s.Token != "tok-1" || !s.Serving {
		t.Fatalf("Probe = %+v, want the stored token from a serving broker", s)
	}
	if i := f.index("mosquitto_pub"); i >= 0 {
		t.Errorf("a read published something: %v", f.runs[i])
	}
	if i := f.index("SIGUSR1"); i >= 0 {
		t.Errorf("a read signalled the broker: %v", f.runs[i])
	}
}

// TestAnEmptyTopicIsAnAnswerNotAnError — the whole reason the control read exists. A broker that
// came back with nothing must produce a SAMPLE saying so, because that is the finding; turning it
// into an error would make total data loss look like the gate failing, which is the one thing
// that never rolls back.
func TestAnEmptyTopicIsAnAnswerNotAnError(t *testing.T) {
	f := serving(nil) // control answers, probe topic does not
	s, err := Probe(context.Background(), f, "briard-mosquitto-broker", "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !s.Serving {
		t.Error("a broker that answered the control read was reported as not serving")
	}
	if s.Token != "" {
		t.Errorf("Token = %q, want empty", s.Token)
	}
}

// TestNothingAnsweringIsNotServing: when even the control read fails, no client can use this
// broker — and that is a different finding from "the token is gone", which is why the control
// read is taken first.
func TestNothingAnsweringIsNotServing(t *testing.T) {
	f := &execFake{answers: map[string]string{}} // not even $SYS
	s, err := Probe(context.Background(), f, "briard-mosquitto-broker", "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if s.Serving {
		t.Error("a broker that answered nothing was reported as serving")
	}
}

// TestAFailedWriteIsAnError, never a sample: the caller must be able to tell "I could not store
// the token" from "the token is not there", or it would report its own failure as data loss.
func TestAFailedWriteIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *execFake
	}{
		{"publish", &execFake{answers: map[string]string{versionTopic: "v"}, failPublish: true}},
		{"persist", &execFake{answers: map[string]string{versionTopic: "v"}, failSignal: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Probe(context.Background(), tc.f, "briard-mosquitto-broker", "tok"); err == nil {
				t.Fatal("a failed write was reported as success")
			}
		})
	}
}

// TestArgumentsAreRefusedRatherThanExecuted. Both of these become podman arguments, and the
// schema's whole discipline is that nothing a catalogued service carries can reach past its own
// box — so they are checked before they are run with, not after.
func TestArgumentsAreRefusedRatherThanExecuted(t *testing.T) {
	f := serving(nil)
	if _, err := Probe(context.Background(), f, "briard-mosquitto-broker; rm -rf /", "tok"); err == nil {
		t.Error("a container name with a shell metacharacter was accepted")
	}
	if _, err := Probe(context.Background(), f, "briard-mosquitto-broker", "tok -r -t other"); err == nil {
		t.Error("a token carrying further arguments was accepted")
	}
	if len(f.runs) != 0 {
		t.Errorf("something ran before the arguments were checked: %v", f.runs)
	}
}
