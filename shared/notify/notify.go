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

// Level classifies an alert (maps to notifier priority/tags).
type Level string

const (
	Warning   Level = "warning"   // a degradation the user should act on
	Recovered Level = "recovered" // a prior warning cleared
)

// Alert is one notification: a level, a short title, and a human-readable body.
type Alert struct {
	Level Level
	Title string
	Body  string
}

// LogMarker is what `briard alerts` looks for. Every alert on a node writes a line beginning
// with it, whatever the notifier -- so the local trail is findable by one substring across
// surfaces that share no logger: the host agent's journal and the guest's serial console.
//
// A SUBSTRING rather than a structured record on purpose. The two surfaces are read back as
// plain text (journalctl output and a file qemu appends the guest's console to), so anything
// richer would have to survive being interleaved with kernel lines -- and the reader is also a
// human with grep, who should not need this tool to find an alert.
const LogMarker = "alert ["

// LogLine renders an alert as the one local-trail line every emitter writes BEFORE attempting
// delivery. Delivery is the half that can be absent (the free tier configures no notifier at
// all) or can fail; the trail is the half that is always there, which is what makes
// `briard alerts` truthful on a node that pushes nothing anywhere.
//
// Emitters that hold a plain string rather than an Alert format this shape by hand -- the guest
// deadman, which must not link this package (see its Alert field). Keep them in step: this
// function is the shape of record.
func LogLine(a Alert) string {
	return fmt.Sprintf("%s%s] %s — %s", LogMarker, a.Level, a.Title, a.Body)
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
// configured. The agent still logs every alert locally (the journal trail), so Nop just
// means "don't also push it out of the home".
func Nop() Notifier { return nopNotifier{} }

type nopNotifier struct{}

func (nopNotifier) Notify(context.Context, Alert) error { return nil }

// Log is a Notifier that records the alert via logf -- an alternative sink (e.g. to route
// alerts through a specific logger) available to callers that want delivery to be a log.
func Log(logf func(string, ...any)) Notifier { return logNotifier{logf: logf} }

type logNotifier struct{ logf func(string, ...any) }

func (l logNotifier) Notify(_ context.Context, a Alert) error {
	l.logf("%s", LogLine(a)) // the same shape the emitters' own trail uses, so `briard alerts` finds both
	return nil
}

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
	switch a.Level {
	case Warning:
		req.Header.Set("Priority", "high")
		req.Header.Set("Tags", "warning")
	default:
		req.Header.Set("Priority", "default")
		req.Header.Set("Tags", "white_check_mark")
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
