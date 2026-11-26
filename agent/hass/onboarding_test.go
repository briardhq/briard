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
// limitHassInstance: hassUrl without the trailing slash, clientId with it (measured).
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

// The later open: the minter is asked with the control channel's bearer and the
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

// The companion app's sign-in: read with a GET that sends nothing, set with a POST of the
// password, both on the control channel's bearer, and the same two refusals as the minter.
func TestPhoneSignIn(t *testing.T) {
	kept := ""
	var bearer, method string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/briard/phone" {
			http.NotFound(w, r)
			return
		}
		bearer, method = r.Header.Get("Authorization"), r.Method
		w.WriteHeader(status)
		if status != http.StatusOK {
			return
		}
		if r.Method == http.MethodPost {
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			kept = in["password"]
			json.NewEncoder(w).Encode(map[string]string{"username": "kostas"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"username": "kostas", "password": kept})
	}))
	defer srv.Close()
	ctx := context.Background()
	if u, p, err := PhoneSignIn(ctx, srv.URL, "tok"); err != nil || u != "kostas" || p != "" || method != http.MethodGet || bearer != "Bearer tok" {
		t.Fatalf("nothing kept: PhoneSignIn = %q, %q, %v by %s %q", u, p, err, method, bearer)
	}
	if u, err := SetPhonePassword(ctx, srv.URL, "tok", "one-two-three-four"); err != nil || u != "kostas" || method != http.MethodPost {
		t.Fatalf("SetPhonePassword = %q, %v by %s", u, err, method)
	}
	if u, p, err := PhoneSignIn(ctx, srv.URL, "tok"); err != nil || u != "kostas" || p != "one-two-three-four" {
		t.Fatalf("kept: PhoneSignIn = %q, %q, %v", u, p, err)
	}
	status = http.StatusConflict
	if _, _, err := PhoneSignIn(ctx, srv.URL, "tok"); err != ErrNoOwner {
		t.Errorf("409 -> %v; want ErrNoOwner", err)
	}
	if _, err := SetPhonePassword(ctx, srv.URL, "tok", "one-two-three-four"); err != ErrNoOwner {
		t.Errorf("409 on a set -> %v; want ErrNoOwner", err)
	}
	if _, _, err := PhoneSignIn(ctx, srv.URL+"/elsewhere", "tok"); err != ErrNoMinter {
		t.Errorf("404 -> %v; want ErrNoMinter", err)
	}
}
