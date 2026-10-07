// Package notify is the v1 alerting seam: the one signal that leaves the home.
//
// v0/v1 has no product cloud, so the agent delivers alerts out of the household itself
// (the "v1 shortcut") -- primarily the degraded-redundancy warning ("you've lost your
// backup node, one failure from an outage") derived from the DRBD/quorum state the agent
// already reads. Delivery is behind this Notifier seam so it's swappable (ntfy today,
// webhook/email later); v2's cloud reclaims the same responsibility through the same
// north-bound seam, so the agent code doesn't change when it lands.
package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Kind is what an alert does to the state of the thing its Key names. A key is either a
// CONDITION (something with state: its alerts are Open and Resolved) or an EVENT (something that
// happens: its alerts are all Event) -- never both, so a reader that keeps the latest alert per
// key can never mistake an event for the end of a condition.
type Kind string

const (
	Open     Kind = "open"     // starts a condition, or restates it with a different title
	Resolved Kind = "resolved" // ends a condition
	Event    Kind = "event"    // happened; changes no state
)

// Severity is how loud an alert is -- on the alert rather than the key, because one key can
// span two (reduced redundancy is a warning; no second copy is critical). A Resolved has none.
type Severity string

const (
	Info     Severity = "info"     // nothing to do; said so the household knows it happened
	Warning  Severity = "warning"  // degraded or at risk; act soon
	Critical Severity = "critical" // not serving, or one step from losing data; act now
)

// Alert is one record about a key: what it did to that key's state, how loud, and the words
// the owner reads. Key is `<name>[:<instance>]` (`redundancy`, `upgrade-rolled-back:os`);
// the name is the row every display, test and support conversation refers to -- titles
// carry node names and get reworded, keys do not.
type Alert struct {
	Key      string   `json:"key"`
	Kind     Kind     `json:"kind"`
	Severity Severity `json:"severity,omitempty"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
}

// Record is one alert as a node's alert store keeps it: the alert, and when it was written. The
// host writes the store; `briard alerts` and the dashboard's pushed copy read the same shape.
type Record struct {
	Alert
	At time.Time `json:"at"`
}

// OpenNow is what is wrong right now: the latest record of every key whose latest record is an
// Open, in the order they opened. Derived from the records, never stored beside them.
func OpenNow(recs []Record) []Record {
	latest := map[string]int{}
	for i, r := range recs {
		latest[r.Key] = i
	}
	var open []Record
	for i, r := range recs {
		if latest[r.Key] == i && r.Kind == Open {
			open = append(open, r)
		}
	}
	return open
}

// Notifier delivers an Alert out of the home. Implementations: Ntfy (the v1 default,
// zero-setup push) and Log (no external dependency -- records locally, the standalone
// fallback). Delivery is best-effort: a failed Notify is logged by the caller, never fatal.
type Notifier interface {
	Notify(ctx context.Context, a Alert) error
}

// Tee fans an Alert out to every notifier (the operator's copy plus the home owner's email). Best-effort -- it delivers to all channels, then returns the joined errors (nil if all
// succeeded), so one failed channel never suppresses the others. A nil/empty Tee is a no-op.
func Tee(notifiers ...Notifier) Notifier { return teeNotifier(notifiers) }

type teeNotifier []Notifier

func (t teeNotifier) Notify(ctx context.Context, a Alert) error {
	var errs []error
	for _, n := range t {
		if n == nil {
			continue
		}
		if err := n.Notify(ctx, a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Nop is a Notifier that delivers nowhere -- the default when no external endpoint is
// configured. The node still records every alert in its own store, so Nop just means
// "don't also push it out of the home".
func Nop() Notifier { return nopNotifier{} }

type nopNotifier struct{}

func (nopNotifier) Notify(context.Context, Alert) error { return nil }

// Ntfy posts alerts to an ntfy topic URL (e.g. https://ntfy.sh/my-briard-abc123) -- a
// dead-simple pub/sub that a phone app subscribes to, no account, so it's the ideal "one
// signal that leaves home" for v1. The body is the message; the title + priority + tags
// ride in headers (ntfy's documented HTTP contract). Stdlib only (CONTRIBUTING.md: no client framework).
func Ntfy(url string) Notifier {
	return ntfyNotifier{url: url, hc: &http.Client{Timeout: 10 * time.Second}}
}

type ntfyNotifier struct {
	url string
	hc  *http.Client
}

func (n ntfyNotifier) Notify(ctx context.Context, a Alert) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, strings.NewReader(a.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", a.Title)
	// Severity sets loudness; kind sets the glyph. A resolved is quiet by construction: it
	// carries no severity, so it takes the default priority and the check mark.
	switch a.Severity {
	case Critical:
		req.Header.Set("Priority", "urgent")
	case Warning:
		req.Header.Set("Priority", "high")
	default:
		req.Header.Set("Priority", "default")
	}
	switch {
	case a.Kind == Resolved:
		req.Header.Set("Tags", "white_check_mark")
	case a.Severity == Critical:
		req.Header.Set("Tags", "rotating_light")
	case a.Severity == Warning:
		req.Header.Set("Tags", "warning")
	default:
		req.Header.Set("Tags", "information_source")
	}
	resp, err := n.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ntfy %s: %s", n.url, resp.Status)
	}
	return nil
}
