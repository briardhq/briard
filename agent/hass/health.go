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

// A Verdict is Home Assistant's answer to Health. agent/services maps it onto services.Health,
// which is the one notion; this is only how Home Assistant says it.
type Verdict int

const (
	VerdictUnknown Verdict = iota
	VerdictHealthy
	VerdictStarting
	VerdictUnhealthy
)

// Health is Home Assistant's service health: whether the APP works, which its own
// `/manifest.json` cannot say. Recovery mode — a configuration that does not parse — keeps that
// answering 200 while every integration is down; `/api/config` reports it (measured by probe).
//
//   - `state` RUNNING and `recovery_mode` false is healthy; `recovery_mode` true is unhealthy;
//     NOT_RUNNING or STARTING is starting (still booting, and nothing else); any other state is
//     unknown (stopping);
//   - a 4xx from the token exchange or from `/api/config` is unknown: our login failed, and that is
//     not the app;
//   - no answer (refused, timed out) or a 5xx from either is unhealthy. A frozen Home Assistant
//     times out on the exchange before `/api/config` can be asked, which is why the exchange's
//     failures count too.
//   - no token on this node is unknown: the control channel was never materialised here.
func Health(ctx context.Context, x Executor, port int) Verdict {
	base, access, err := connect(ctx, x, port)
	if err != nil {
		return failed(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/config", nil)
	if err != nil {
		return VerdictUnknown
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := client.Do(req)
	if err != nil {
		return VerdictUnhealthy
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return failed(httpStatus{"hass: /api/config", resp.StatusCode})
	}
	var cfg struct {
		State        string `json:"state"`
		RecoveryMode bool   `json:"recovery_mode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return VerdictUnknown
	}
	switch {
	case cfg.RecoveryMode:
		return VerdictUnhealthy
	case cfg.State == "RUNNING":
		return VerdictHealthy
	case cfg.State == "NOT_RUNNING", cfg.State == "STARTING":
		return VerdictStarting
	}
	return VerdictUnknown
}

// errNoChannel marks a failure before anything was asked of Home Assistant: no usable token or
// port on this node.
var errNoChannel = errors.New("the control channel is not set up on this node")

// failed is the verdict a failure to reach Home Assistant gives: unhealthy when it is about the
// app — a transport failure (refused, reset, timed out) or a 5xx — and unknown when it is about
// us: a 4xx, or no usable token.
func failed(err error) Verdict {
	var st httpStatus
	if errors.As(err, &st) {
		if st.code >= 500 {
			return VerdictUnhealthy
		}
		return VerdictUnknown
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return VerdictUnhealthy
	}
	return VerdictUnknown
}
