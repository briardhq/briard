package hass

import (
	"context"
	"net/http"
	"testing"
)

// TestHealthReadsTheAppNotTheWebServer (measured by probe): recovery mode keeps the web
// server answering while the app is down, and only /api/config says so. Every row is one case the
// design names.
func TestHealthReadsTheAppNotTheWebServer(t *testing.T) {
	for name, c := range map[string]struct {
		stub haStub
		want Verdict
	}{
		"running":                   {haStub{config: `{"state":"RUNNING","recovery_mode":false}`}, VerdictHealthy},
		"recovery mode":             {haStub{config: `{"state":"RUNNING","recovery_mode":true}`}, VerdictUnhealthy},
		"not running yet":           {haStub{config: `{"state":"NOT_RUNNING","recovery_mode":false}`}, VerdictStarting},
		"starting":                  {haStub{config: `{"state":"STARTING"}`}, VerdictStarting},
		"stopping":                  {haStub{config: `{"state":"STOPPING"}`}, VerdictUnknown},
		"our login is refused":      {haStub{tokenCode: http.StatusBadRequest}, VerdictUnknown},
		"/api/config refuses us":    {haStub{configCode: http.StatusUnauthorized}, VerdictUnknown},
		"the exchange fails inside": {haStub{tokenCode: http.StatusInternalServerError}, VerdictUnhealthy},
		"/api/config fails inside":  {haStub{configCode: http.StatusBadGateway}, VerdictUnhealthy},
	} {
		c.stub.token = "tok"
		port := c.stub.start(t)
		if got := Health(context.Background(), tokenFile("tok"), port); got != c.want {
			t.Errorf("%s: Health = %d, want %d", name, got, c.want)
		}
	}
}

// TestHealthOfAnAppThatDoesNotAnswer: a Home Assistant that is down or frozen answers nothing,
// and that is unhealthy -- not unknown.
func TestHealthOfAnAppThatDoesNotAnswer(t *testing.T) {
	if got := Health(context.Background(), tokenFile("tok"), 1); got != VerdictUnhealthy {
		t.Errorf("Health(nothing listening) = %d, want unhealthy", got)
	}
}

// TestHealthWithNoTokenIsUnknown: the control channel was never set up on this node, which says
// nothing about the app.
func TestHealthWithNoTokenIsUnknown(t *testing.T) {
	stub := &haStub{token: "tok", config: `{"state":"RUNNING"}`}
	port := stub.start(t)
	if got := Health(context.Background(), &fake{files: map[string]string{}}, port); got != VerdictUnknown {
		t.Errorf("Health(no token) = %d, want unknown", got)
	}
}
