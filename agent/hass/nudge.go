package hass

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// EventReconsider is what briard fires on Home Assistant's own event bus when something outside
// Home Assistant changed that briard's integration may want to act on.
//
// The name is half the contract; the other half is that it carries nothing. It does not say what
// changed, and adding a payload later would be the wrong repair: the integration re-derives its
// whole world when it wakes (is there an mqtt entry? is a broker answering?), so a lost event, a
// duplicate event and an event fired for an unrelated reason all cost the same — nothing. A
// message that carried state would have to be right, and would need a reply to know that it was.
// For the same reason a second feature brings no second event: it adds a re-derivation on the
// integration's side, not a message type on this one.
const EventReconsider = "briard_reconsider"

// Nudge tells a running Home Assistant to reconsider, and that it is running is the whole reason
// this exists. Everything briard's integration does, it does when Home Assistant starts, and
// converge deliberately restarts only the apps whose rendered bytes changed, so installing the
// broker next to a Home Assistant that is already up leaves it running, unwired and unaware, until
// something happens to restart it — for a household, "briard says the broker is installed and
// Home Assistant disagrees, indefinitely".
//
// The transport is Home Assistant's own inbound API and nothing new: POST /api/events/<type> is
// how anything outside HA puts something on its bus, our system user is already an admin (which
// that view requires), and the token and the loopback are the ones the readiness gate uses. It
// adds no mount, no socket and no listener; the alternatives (a file the integration watches, a
// callback socket back to the guest agent) each add a mechanism to carry the same single bit.
//
// Fire and forget, one way: a 200 says HA put the event on its bus, never that the integration
// acted on it, and asking for more would need a return channel this deliberately does not have.
// The caller treats a failure as nothing worse than the state it was already in — an HA that will
// pick this up at its next start.
//
// It runs in the guest for the same reason as Readiness: the token is on this node's tmpfs and
// Home Assistant listens on the guest's loopback, which the host cannot reach at all under
// macvtap. Still dumb hands — when a running Home Assistant is worth telling is the host's
// decision (it fires this after every install and uninstall), and nothing about it is encoded
// here.
func Nudge(ctx context.Context, x Executor, port int) error {
	base, access, err := connect(ctx, x, port)
	if err != nil {
		return err
	}
	// An empty JSON object, not an empty body: HA's view parses whatever body it is given and
	// refuses anything that is not an object, so `{}` is the encoding of "no data" that cannot be
	// mistaken for one.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/events/"+EventReconsider, strings.NewReader("{}"))
	if err != nil {
		return fmt.Errorf("hass nudge: build event request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("hass nudge: fire %s: %w", EventReconsider, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A 401/403 here means the token is no longer admin, which is worth telling apart from a
		// Home Assistant that is simply not answering: the event view is admin-gated, and our
		// system user is put in that group at every mint (ensure-token.py).
		return fmt.Errorf("hass nudge: fire %s: HTTP %d", EventReconsider, resp.StatusCode)
	}
	return nil
}
