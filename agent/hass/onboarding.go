package hass

// Home Assistant's ONBOARDING, from the outside ([V3b.31a](d), measured in (f)).
//
// The install ends by handing the household a logged-in Home Assistant, and the way it does so is
// HA's own resume path: briard creates the first user through the onboarding API, and sends the
// browser to HA's onboarding page with the code that call returns. The page sees the user step
// done, exchanges the code from the URL, and continues at the first undone step -- location --
// with HA's own map and detection, then its discovered-devices page, then a fresh code and the
// logged-in dashboard. Every function here is one call on that path; none of them is a page.
//
// THE CODE IS SINGLE-USE AND BELONGS TO THE BROWSER. Anything briard needs to do to HA before the
// hand-off (marking analytics done, so the page is skipped and analytics stays off) is done with
// the control channel's OWN token, never by spending the user's code -- measured in [V3b.31a](f):
// the exchange pops the code, and there is no API that mints another for a user we hold a token
// for. (The in-HA integration will mint on demand later; this path does not need it.)

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Steps is HA's own view of onboarding: step name -> done.
type Steps map[string]bool

// The four steps, in the order HA runs them.
const (
	StepUser        = "user"
	StepCoreConfig  = "core_config"
	StepAnalytics   = "analytics"
	StepIntegration = "integration"
)

// Done says whether onboarding is complete.
func (s Steps) Done() bool {
	if len(s) == 0 {
		return false
	}
	for _, d := range s {
		if !d {
			return false
		}
	}
	return true
}

// OnboardingSteps reads `GET /api/onboarding`, which needs no auth -- it is the thing the
// dashboard's button reads to decide what it does.
//
// A 404 IS "DONE": once every step is complete AND HA has restarted, the onboarding views are not
// registered at all, so a 404 is what an onboarded HA answers. (Within the same process the views
// stay up and report every step done; both shapes mean the same thing, and HA's own frontend
// treats them identically.)
func OnboardingSteps(ctx context.Context, base string) (Steps, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/onboarding", nil)
	if err != nil {
		return nil, fmt.Errorf("hass: build onboarding request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hass: onboarding status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Steps{StepUser: true, StepCoreConfig: true, StepAnalytics: true, StepIntegration: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hass: onboarding status: HTTP %d", resp.StatusCode)
	}
	var raw []struct {
		Step string `json:"step"`
		Done bool   `json:"done"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("hass: decode onboarding status: %w", err)
	}
	s := make(Steps, len(raw))
	for _, r := range raw {
		s[r.Step] = r.Done
	}
	return s, nil
}

// NewUser is what `POST /api/onboarding/users` needs: five strings, all required by HA.
type NewUser struct {
	Name     string
	Username string
	Password string
	// ClientID is the browser's own origin WITH a trailing slash -- HA's `genClientId()` -- and
	// the code that comes back is bound to it. The `state` OnboardingURL builds must carry the
	// same value, or HA's frontend refuses the callback ([V3b.31a](f)1).
	ClientID string
	Language string
}

// CreateUser runs the user step and returns the auth code the browser will exchange. The user is
// the first non-system one, so HA makes it the OWNER -- the control channel's system user does
// not count ([V3b.31a](e), measured in (f)4).
//
// Refused (403) once the step is done: this is a fresh-HA-only call, by HA's own rule.
func CreateUser(ctx context.Context, base string, u NewUser) (string, error) {
	body, err := json.Marshal(map[string]string{
		"name": u.Name, "username": u.Username, "password": u.Password,
		"client_id": u.ClientID, "language": u.Language,
	})
	if err != nil {
		return "", fmt.Errorf("hass: encode user: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/onboarding/users", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("hass: build user request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("hass: create user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("hass: create user: HTTP %d", resp.StatusCode)
	}
	var out struct {
		AuthCode string `json:"auth_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("hass: decode user response: %w", err)
	}
	if out.AuthCode == "" {
		return "", fmt.Errorf("hass: the user step returned no auth code")
	}
	return out.AuthCode, nil
}

// MarkAnalytics marks the analytics step done WITHOUT setting preferences, which is how analytics
// stays off and the page is skipped ([V3b.31a](f)2). Any admin's access token will do -- the
// control channel's is the one to use, so the household's code is never spent. Already done is
// not an error: HA answers 403 for it, and the outcome is the one asked for.
func MarkAnalytics(ctx context.Context, base, access string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/onboarding/analytics", strings.NewReader("{}"))
	if err != nil {
		return fmt.Errorf("hass: build analytics request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("hass: mark analytics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusForbidden {
		return fmt.Errorf("hass: mark analytics: HTTP %d", resp.StatusCode)
	}
	return nil
}

// CoreState reads HA's own lifecycle state from `/api/config` -- "RUNNING" is the boundary an
// action may be taken on. A Home Assistant serving HTTP is not yet one that will act on what it
// is asked ([B.127]); the dashboard's button waits for this, not for a 200.
func CoreState(ctx context.Context, base, access string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/config", nil)
	if err != nil {
		return "", fmt.Errorf("hass: build config request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("hass: core state: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("hass: core state: HTTP %d", resp.StatusCode)
	}
	var out struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("hass: decode config: %w", err)
	}
	return out.State, nil
}

// SystemAccess exchanges the control channel's token (TokenPath) for an access token, the way
// every consumer of the channel does. It returns HA's in-guest base URL beside it.
func SystemAccess(ctx context.Context, x Executor, port int) (base, access string, err error) {
	return connect(ctx, x, port)
}

// OnboardingURL is where the browser goes with the user step's code: HA's own onboarding page,
// which resumes at the first undone step. `origin` is HA as the BROWSER reaches it (scheme +
// host, no trailing slash). The `state` is what home-assistant-js-websocket's `getAuth` decodes
// and checks against the page's own origin under `limitHassInstance` -- hassUrl without the
// trailing slash, clientId with it -- so it is built here, once, next to ClientID's rule.
func OnboardingURL(origin, authCode string) string {
	state, _ := json.Marshal(struct {
		HassURL  string `json:"hassUrl"`
		ClientID string `json:"clientId"`
	}{origin, ClientID(origin)})
	q := url.Values{}
	q.Set("auth_callback", "1")
	q.Set("code", authCode)
	q.Set("state", base64.StdEncoding.EncodeToString(state))
	return origin + "/onboarding.html?" + q.Encode()
}

// ClientID is HA's `genClientId()` for an origin: the origin with a trailing slash.
func ClientID(origin string) string { return origin + "/" }
