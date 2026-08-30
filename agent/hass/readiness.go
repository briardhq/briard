package hass

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Entry is one Home Assistant config entry's identity and setup state — the triple the S1
// readiness gate reasons over (agent/guest/entrygate). It is also the WIRE shape of the
// service.home-assistant.readiness verb, which is why it carries HA's own JSON names: the guest decodes HA's
// answer straight into it and the host decodes the verb's answer into the same type, so there is
// one spelling of this triple in the tree rather than one per hop.
//
// The rest of HA's config-entry JSON (titles, flags, subentry counts) is ignored on purpose. The
// gate's signal is which entries were loaded and which stopped being loaded; nothing else in that
// document changes a verdict, and a narrow struct is what keeps it that way.
type Entry struct {
	ID     string `json:"entry_id"`
	Domain string `json:"domain"`
	State  string `json:"state"`
}

// client is this package's one HTTP client, with an explicit timeout. Short, because everything
// it talks to is on the loopback of the machine it runs on: a request that has not finished in
// this long is a Home Assistant that has stopped answering, which is a fact the caller wants
// quickly rather than a slow success. It bounds the install window too — a baseline capture that
// hung would eat the budget the install needs to finish or revert.
var client = &http.Client{Timeout: 10 * time.Second}

// Readiness samples Home Assistant's per-config-entry setup states, from inside the guest.
//
// IN THE GUEST BECAUSE BOTH HALVES ARE THERE: the token is on this node's tmpfs, and Home
// Assistant listens on the guest's loopback. Asking the guest is also what the liveness floor
// already does (`service.health`), and for the same reason — on the macvtap substrate the host
// cannot reach the guest's addresses at all.
//
// THIS IS NOT A DECISION, and the split matters ([[logic-on-host-by-default]]). What lives here
// is how you TALK to Home Assistant: where the token is, that a refresh token has to be exchanged
// for an access token first, which path lists config entries, and which three fields of the
// answer mean anything. What the samples MEAN — which regressions are the upgrade's fault, how
// long to let them settle, and whether to roll back — is the host's, in agent/guest/entrygate.
//
// The port is the manifest's, passed in rather than assumed: the catalog names it once, and the
// health gate already builds its URL from the same field.
func Readiness(ctx context.Context, x Executor, port int) ([]Entry, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("hass readiness: port %d out of range", port)
	}
	raw, err := x.ReadFile(TokenPath)
	if err != nil {
		// No token means the control channel was never materialised on this node — Prepare did
		// not run, or /run was cleared under a running guest. Say which, because the caller's
		// only alternative reading is "Home Assistant is broken", and it is not.
		return nil, fmt.Errorf("hass readiness: no control token at %s: %w", TokenPath, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, fmt.Errorf("hass readiness: the control token at %s is empty", TokenPath)
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	access, err := exchange(ctx, base, token)
	if err != nil {
		return nil, err
	}
	return entries(ctx, base, access)
}

// exchange trades the long-lived refresh token for a short-lived access token — HA's documented
// flow, with client_id omitted (a system token has none, and sending one is refused).
//
// Per use, never cached: an access token lives 30 minutes and the two samples this package is
// asked for are taken minutes apart across a service restart. Holding one would trade a
// negligible saving for a class of bug where the gate reads a stale credential.
func exchange(ctx context.Context, base, token string) (string, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/auth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("hass readiness: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("hass readiness: exchange token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A 400 here is the interesting one: it means HA does not know our token, i.e. the mint
		// did not stick. Worth naming, because the caller degrades to floor-only either way and
		// the log line is the only place the difference is visible.
		return "", fmt.Errorf("hass readiness: exchange token: HTTP %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("hass readiness: decode token: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("hass readiness: exchange returned no access token")
	}
	return out.AccessToken, nil
}

// entries lists the config entries. An EMPTY list is not an error and must not be treated as one:
// Home Assistant answers its HTTP stack before `default_config` has finished setting entries up,
// so a sample taken early is legitimately empty — measured, and it is what the differential gate
// is for. An empty PRE sample simply excludes everything, which is the safe direction.
func entries(ctx context.Context, base, access string) ([]Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/config/config_entries/entry", nil)
	if err != nil {
		return nil, fmt.Errorf("hass readiness: build entries request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hass readiness: list config entries: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hass readiness: list config entries: HTTP %d", resp.StatusCode)
	}
	var out []Entry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("hass readiness: decode config entries: %w", err)
	}
	return out, nil
}
