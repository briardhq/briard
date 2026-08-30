package mosquitto

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// The S1 readiness signal for a broker, and why it is a WRITE where Home Assistant's is a read.
//
// The liveness floor asks whether the service answers; S1 asks whether it still does the thing it
// was doing. For Home Assistant that is legible without touching anything — it publishes which
// integrations are loaded, so the gate samples and compares. A broker publishes no such thing.
// Its work is accepting clients and keeping what they told it to keep, and NEITHER is observable
// from the management API: `/api/v1/listeners` reports the listener is configured, which stays
// true of a broker that refuses every connection and of one that came back with an empty store.
//
// So the probe does what a client does. Before the change it publishes a RETAINED token; after,
// it reads that token back. Surviving that round trip is the whole claim, and it covers the two
// ways an upgrade breaks a broker while leaving it answering:
//
//   - it lost its persisted state (a mount that no longer covers where it writes, a database
//     format it cannot read) — the token is gone, and so is every retained message the household
//     had, which no floor can see because the broker is up and serving nothing;
//   - it stopped accepting clients (an auth default that moved, a listener bound elsewhere) —
//     nothing can connect at all.
//
// THE TWO ARE DISTINGUISHED, deliberately, by a control read of `$SYS/broker/version`: the broker
// publishes it retained, so it comes back instantly to any client that can connect (measured on
// the pinned image). If that answers and our token does not, the broker is serving and has lost
// its state. If it does not answer, the broker is not serving. The verdicts differ in reason and
// the household deserves the right one.
//
// IT BORROWS THE SERVICE'S OWN TOOLS, exactly as agent/hass borrows Home Assistant's Python: the
// image ships mosquitto_pub and mosquitto_sub, so nothing here implements MQTT and the guest gains
// no dependency. Dumb hands: this is HOW to ask a broker, never what the answer means — the
// verdict is the host's (agent/host/readiness.go).

// ProbeTopic is the one topic Briard owns on a household's broker. Namespaced so it cannot
// collide with a device's, and a single retained message rather than a series: it is overwritten
// with a fresh token at every upgrade, so a value left by an earlier one can never be mistaken for
// this one's survival.
const ProbeTopic = "briard/probe"

// versionTopic is the control. Every mosquitto publishes it retained, so a client that can
// connect gets it immediately — which is what makes "the token is missing" and "nothing can
// connect" two different findings rather than one ambiguous failure.
const versionTopic = "$SYS/broker/version"

// probeWait bounds each subscribe. A retained message is delivered on subscribe, so this is not a
// poll interval — it is how long to wait for a broker that is not going to answer. Short, because
// it is spent twice inside an install window that has to finish or revert.
const probeWait = 5

// Sample is one observation of the broker as a CLIENT sees it.
type Sample struct {
	// Serving is true when a client connected and the broker answered the control read. False
	// means no client can use this broker at all, whatever its management API says.
	Serving bool
	// Token is what was stored at ProbeTopic, or "" when the broker is serving and the topic is
	// empty — which is the data-loss signal, not an error.
	Token string
}

// Probe stores `token` in the broker's own retained state when one is given, and returns what the
// broker holds. An empty token makes it a pure read.
//
// The container is named by the caller rather than derived here: the renderer decides unit and
// container names (agent/quadlet), and a second place that reconstructs them is a second place to
// get them wrong.
func Probe(ctx context.Context, x Executor, container, token string) (Sample, error) {
	if err := safeName(container); err != nil {
		return Sample{}, err
	}
	if token != "" {
		if err := publish(ctx, x, container, token); err != nil {
			return Sample{}, err
		}
	}
	// The control first. If the broker is not serving clients, what is stored is unanswerable
	// rather than empty, and the caller must not read one as the other.
	if _, err := subscribe(ctx, x, container, versionTopic); err != nil {
		return Sample{Serving: false}, nil
	}
	stored, err := subscribe(ctx, x, container, ProbeTopic)
	if err != nil {
		// Serving, and the topic is empty. That is an ANSWER — the broker kept nothing — so it
		// comes back as a sample rather than as an error. The two are separated one line above.
		return Sample{Serving: true}, nil
	}
	return Sample{Serving: true, Token: stored}, nil
}

// publish writes the token retained, then makes the broker persist it.
//
// THE SIGNAL IS THE POINT, and without it this probe would be a race it could not win: mosquitto
// holds retained state in memory and writes it on a clean stop or every autosave_interval (30s,
// mosquitto.conf). A token published seconds before an upgrade would usually be flushed by the
// stop and occasionally not — and the miss would read as data loss, i.e. an automatic rollback of
// a perfectly good upgrade. SIGUSR1 is mosquitto's documented "write the persistence database
// now" (measured on the pinned image: the log says `Saving in-memory database`), which turns a
// probability into a fact before anything is judged on it.
func publish(ctx context.Context, x Executor, container, token string) error {
	if strings.ContainsAny(token, " \t\n\r\x00") || token == "" {
		return fmt.Errorf("mosquitto probe: token %q is not a bare word", token)
	}
	if out, err := x.Run(ctx, "podman", "exec", container,
		"mosquitto_pub", "-h", "127.0.0.1", "-p", strconv.Itoa(MQTTPort),
		"-t", ProbeTopic, "-m", token, "-r", "-q", "1"); err != nil {
		return fmt.Errorf("mosquitto probe: publish: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := x.Run(ctx, "podman", "kill", "--signal", "SIGUSR1", container); err != nil {
		return fmt.Errorf("mosquitto probe: ask the broker to persist: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// subscribe reads one retained message. A topic with nothing retained makes mosquitto_sub time out
// and exit non-zero, which is why the caller reads an error here as "empty" only after the control
// read has established that the broker is answering at all.
func subscribe(ctx context.Context, x Executor, container, topic string) (string, error) {
	out, err := x.Run(ctx, "podman", "exec", container,
		"mosquitto_sub", "-h", "127.0.0.1", "-p", strconv.Itoa(MQTTPort),
		"-t", topic, "-C", "1", "-W", strconv.Itoa(probeWait))
	if err != nil {
		return "", fmt.Errorf("mosquitto probe: read %s: %w", topic, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// safeName refuses anything that is not a plain container name. It becomes an argument to podman,
// and the schema's whole discipline is that a catalogued service cannot reach past its own box —
// so a name that arrived from anywhere gets checked before it is executed with.
func safeName(name string) error {
	if name == "" {
		return fmt.Errorf("mosquitto probe: no container named")
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("mosquitto probe: container name %q is not a plain name", name)
	}
	return nil
}
