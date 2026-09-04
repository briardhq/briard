package hass

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The URL the browser is handed is HA's own resume path, and its `state` is what
// home-assistant-js-websocket decodes and checks against the page's origin under
// limitHassInstance: hassUrl WITHOUT the trailing slash, clientId WITH it ([V3b.31a](f)1).
func TestOnboardingURLCarriesTheStateTheFrontendChecks(t *testing.T) {
	u, err := url.Parse(OnboardingURL("http://briard-brave-elf-home-assistant.local", "abc"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/onboarding.html" || u.Query().Get("auth_callback") != "1" || u.Query().Get("code") != "abc" {
		t.Errorf("url = %s", u)
	}
	raw, err := base64.StdEncoding.DecodeString(u.Query().Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s["hassUrl"] != "http://briard-brave-elf-home-assistant.local" || s["clientId"] != "http://briard-brave-elf-home-assistant.local/" {
		t.Errorf("state = %v", s)
	}
}

func TestStepsDone(t *testing.T) {
	if (Steps{}).Done() {
		t.Error("no steps is not done")
	}
	if (Steps{StepUser: true, StepCoreConfig: false}).Done() {
		t.Error("an undone step is not done")
	}
	if !(Steps{StepUser: true, StepCoreConfig: true, StepAnalytics: true, StepIntegration: true}).Done() {
		t.Error("all done is done")
	}
}

// A 404 is what an onboarded, restarted HA answers: the views are not registered at all.
func TestOnboardingStepsTreats404AsDone(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	s, err := OnboardingSteps(context.Background(), srv.URL)
	if err != nil || !s.Done() {
		t.Errorf("404 -> %v, %v; want done", s, err)
	}
}

func TestCreateUserAndMarkAnalytics(t *testing.T) {
	var got map[string]string
	var bearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/onboarding/users":
			json.NewDecoder(r.Body).Decode(&got)
			json.NewEncoder(w).Encode(map[string]string{"auth_code": "c"})
		case "/api/onboarding/analytics":
			bearer = r.Header.Get("Authorization")
			http.Error(w, "Analytics config step already done", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	code, err := CreateUser(context.Background(), srv.URL, NewUser{Name: "K", Username: "k", Password: "p", ClientID: "http://x/", Language: "en"})
	if err != nil || code != "c" {
		t.Fatalf("CreateUser = %q, %v", code, err)
	}
	if got["client_id"] != "http://x/" || got["language"] != "en" || got["name"] != "K" {
		t.Errorf("user step got %v", got)
	}
	// Already done is the outcome asked for, not an error.
	if err := MarkAnalytics(context.Background(), srv.URL, "tok"); err != nil {
		t.Errorf("MarkAnalytics on an already-done step = %v; want nil", err)
	}
	if bearer != "Bearer tok" {
		t.Errorf("analytics bearer = %q", bearer)
	}
}
