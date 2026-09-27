package hass

import (
	"context"
	"net/http"
	"testing"
)

// TestHealthReadsTheAppNotTheWebServer ([B.167], [B.167b]'s probes): recovery mode keeps the web
// server answering while the app is down, and only /api/config says so. Every row is one case the
// design names.
func TestHealthReadsTheAppNotTheWebServer(t *testing.T) {
	for name, c := range map[string]struct {
		stub           haStub
		healthy, known bool
	}{
		"running":                   {haStub{config: `{"state":"RUNNING","recovery_mode":false}`}, true, true},
		"recovery mode":             {haStub{config: `{"state":"RUNNING","recovery_mode":true}`}, false, true},
		"starting":                  {haStub{config: `{"state":"NOT_RUNNING","recovery_mode":false}`}, false, false},
		"stopping":                  {haStub{config: `{"state":"STOPPING"}`}, false, false},
		"our login is refused":      {haStub{tokenCode: http.StatusBadRequest}, false, false},
		"/api/config refuses us":    {haStub{configCode: http.StatusUnauthorized}, false, false},
		"the exchange fails inside": {haStub{tokenCode: http.StatusInternalServerError}, false, true},
		"/api/config fails inside":  {haStub{configCode: http.StatusBadGateway}, false, true},
	} {
		c.stub.token = "tok"
		port := c.stub.start(t)
		healthy, known := Health(context.Background(), tokenFile("tok"), port)
		if healthy != c.healthy || known != c.known {
			t.Errorf("%s: Health = (healthy=%t, known=%t), want (%t, %t)", name, healthy, known, c.healthy, c.known)
		}
	}
}

// TestHealthOfAnAppThatDoesNotAnswer: a Home Assistant that is down or frozen answers nothing,
// and that is unhealthy -- not unknown.
func TestHealthOfAnAppThatDoesNotAnswer(t *testing.T) {
	if healthy, known := Health(context.Background(), tokenFile("tok"), 1); healthy || !known {
		t.Errorf("Health(nothing listening) = (%t, %t), want unhealthy", healthy, known)
	}
}

// TestHealthWithNoTokenIsUnknown: the control channel was never set up on this node, which says
// nothing about the app.
func TestHealthWithNoTokenIsUnknown(t *testing.T) {
	stub := &haStub{token: "tok", config: `{"state":"RUNNING"}`}
	port := stub.start(t)
	if healthy, known := Health(context.Background(), &fake{files: map[string]string{}}, port); healthy || known {
		t.Errorf("Health(no token) = (%t, %t), want unknown", healthy, known)
	}
}
