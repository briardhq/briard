package hass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// httpStatus is an answer Home Assistant gave with a status other than 200. It is a type rather
// than a string because Health reads the CLASS of the code: a 4xx is our login failing, a 5xx is
// Home Assistant failing, and the two mean opposite things about the app.
type httpStatus struct {
	what string
	code int
}

func (e httpStatus) Error() string { return fmt.Sprintf("%s: HTTP %d", e.what, e.code) }

// Health is Home Assistant's service health ([B.167]): whether the APP works, which its own
// `/manifest.json` cannot say. Recovery mode — a configuration that does not parse — keeps that
// answering 200 while every integration is down; `/api/config` reports it ([B.167b]'s probes).
//
// It answers healthy, unhealthy, or known=false (unknown):
//   - `state` RUNNING and `recovery_mode` false is healthy; `recovery_mode` true is unhealthy; any
//     other state is unknown, because Home Assistant is starting or stopping;
//   - a 4xx from the token exchange or from `/api/config` is unknown: our login failed, and that is
//     not the app;
//   - no answer (refused, timed out) or a 5xx from either is unhealthy. A frozen Home Assistant
//     times out on the exchange before `/api/config` can be asked, which is why the exchange's
//     failures count too.
//   - no token on this node is unknown: the control channel was never materialised here.
func Health(ctx context.Context, x Executor, port int) (healthy, known bool) {
	base, access, err := connect(ctx, x, port)
	if err != nil {
		return false, answered(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/config", nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := client.Do(req)
	if err != nil {
		return false, true
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, answered(httpStatus{"hass: /api/config", resp.StatusCode})
	}
	var cfg struct {
		State        string `json:"state"`
		RecoveryMode bool   `json:"recovery_mode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return false, false
	}
	switch {
	case cfg.RecoveryMode:
		return false, true
	case cfg.State == "RUNNING":
		return true, true
	}
	return false, false
}

// errNoChannel marks a failure before anything was asked of Home Assistant: no usable token or
// port on this node.
var errNoChannel = errors.New("the control channel is not set up on this node")

// answered says whether a failure to reach Home Assistant is a verdict about the app (unhealthy)
// rather than about us (unknown). Only a transport failure (refused, reset, timed out) or a 5xx
// is one.
func answered(err error) bool {
	var st httpStatus
	if errors.As(err, &st) {
		return st.code >= 500
	}
	var ue *url.Error
	return errors.As(err, &ue)
}
