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

// The later open ([V3b.31d]): the minter is asked with the control channel's bearer and the
// browser's client_id, and its answers map to the two refusals the dashboard surfaces by name.
func TestMintLogin(t *testing.T) {
	var got map[string]string
	var bearer string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/briard/login" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		bearer = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		if status == http.StatusOK {
			json.NewEncoder(w).Encode(map[string]string{"auth_code": "owner-code"})
		}
	}))
	defer srv.Close()
	code, err := MintLogin(context.Background(), srv.URL, "tok", "http://x/")
	if err != nil || code != "owner-code" {
		t.Fatalf("MintLogin = %q, %v", code, err)
	}
	if got["client_id"] != "http://x/" || bearer != "Bearer tok" {
		t.Errorf("the minter was asked %v with %q", got, bearer)
	}
	status = http.StatusConflict
	if _, err := MintLogin(context.Background(), srv.URL, "tok", "http://x/"); err != ErrNoOwner {
		t.Errorf("409 -> %v; want ErrNoOwner", err)
	}
	if _, err := MintLogin(context.Background(), srv.URL+"/elsewhere", "tok", "http://x/"); err != ErrNoMinter {
		t.Errorf("404 -> %v; want ErrNoMinter", err)
	}
}

// The reset ([V3b.31e]): the new password reaches the integration with the control channel's
// bearer, the owner's username comes back, and the refusals are the minter's.
func TestResetPassword(t *testing.T) {
	var got map[string]string
	var bearer string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/briard/password" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		bearer = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		if status == http.StatusOK {
			json.NewEncoder(w).Encode(map[string]string{"username": "kostas"})
		}
	}))
	defer srv.Close()
	user, err := ResetPassword(context.Background(), srv.URL, "tok", "new-secret-20chars")
	if err != nil || user != "kostas" {
		t.Fatalf("ResetPassword = %q, %v", user, err)
	}
	if got["password"] != "new-secret-20chars" || bearer != "Bearer tok" {
		t.Errorf("the integration was asked %v with %q", got, bearer)
	}
	status = http.StatusConflict
	if _, err := ResetPassword(context.Background(), srv.URL, "tok", "x"); err != ErrNoOwner {
		t.Errorf("409 -> %v; want ErrNoOwner", err)
	}
	if _, err := ResetPassword(context.Background(), srv.URL+"/elsewhere", "tok", "x"); err != ErrNoMinter {
		t.Errorf("404 -> %v; want ErrNoMinter", err)
	}
}

// LoginURL is HA's own end-of-onboarding redirect: the front page's auth callback with the same
// state as the onboarding resume, plus storeToken so the tokens outlive the tab.
func TestLoginURL(t *testing.T) {
	u, err := url.Parse(LoginURL("http://briard-x-home-assistant.local", "abc"))
	if err != nil || u.Path != "/" {
		t.Fatalf("LoginURL = %v, %v", u, err)
	}
	q := u.Query()
	if q.Get("auth_callback") != "1" || q.Get("code") != "abc" || q.Get("storeToken") != "true" {
		t.Errorf("query = %v", q)
	}
	raw, _ := base64.StdEncoding.DecodeString(q.Get("state"))
	var state map[string]string
	json.Unmarshal(raw, &state)
	if state["hassUrl"] != "http://briard-x-home-assistant.local" || state["clientId"] != "http://briard-x-home-assistant.local/" {
		t.Errorf("state = %v", state)
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
