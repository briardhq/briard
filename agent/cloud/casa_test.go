package cloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"briard.io/shared/casa"
)

// A fake cloud + Worker on one server: records what the node sent, answers what the test scripts.
func casaServer(t *testing.T, status map[string]int) (*Casa, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(body))
		if st, ok := status[r.URL.Path]; ok && st != 200 {
			w.WriteHeader(st)
			if st == http.StatusConflict {
				json.NewEncoder(w).Encode(casa.ClaimStatus{State: casa.ClaimRefused, Reason: "taken"})
			} else {
				io.WriteString(w, `{"error":"nope"}`)
			}
			return
		}
		switch {
		case r.URL.Path == casa.ClaimPath:
			json.NewEncoder(w).Encode(casa.ClaimResponse{ID: "c1"})
		case strings.HasPrefix(r.URL.Path, casa.ClaimPath+"/"):
			json.NewEncoder(w).Encode(casa.ClaimStatus{State: casa.ClaimPending})
		case r.URL.Path == casa.CertPath:
			json.NewEncoder(w).Encode(casa.CertResponse{Cert: "PEM"})
		case r.URL.Path == casa.APath:
			io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return NewCasa(srv.URL+"/", srv.URL), &seen
}

func TestCasaVerbs(t *testing.T) {
	ctx := context.Background()
	c, seen := casaServer(t, nil)

	id, refused, err := c.Claim(ctx, casa.ClaimRequest{Flock: "alert-fox", Email: "a@x.org", PubKey: []byte("k")})
	if err != nil || refused != nil || id != "c1" {
		t.Fatalf("Claim = %q %v %v", id, refused, err)
	}
	st, err := c.ClaimStatus(ctx, "c1")
	if err != nil || st.State != casa.ClaimPending {
		t.Fatalf("ClaimStatus = %+v %v", st, err)
	}
	pem, err := c.Cert(ctx, casa.CertRequest{Flock: "alert-fox", CSR: []byte{1}, TS: 7, Sig: []byte{2}})
	if err != nil || pem != "PEM" {
		t.Fatalf("Cert = %q %v", pem, err)
	}
	if err := c.SetA(ctx, casa.ARequest{Name: "alert-fox", IP: "10.0.0.7", TS: 7, Sig: []byte{2}}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`POST /casa/claim {"flock":"alert-fox","email":"a@x.org","pubkey":"aw=="}`,
		`GET /casa/claim/c1 `,
		`POST /casa/cert {"flock":"alert-fox","csr":"AQ==","ts":7,"sig":"Ag=="}`,
		`POST /a {"name":"alert-fox","ip":"10.0.0.7","ts":7,"sig":"Ag=="}`,
	}
	if len(*seen) != len(want) {
		t.Fatalf("requests = %q", *seen)
	}
	for i := range want {
		if (*seen)[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, (*seen)[i], want[i])
		}
	}
}

// A refusal is an answer with a reason; every other non-200 is an error carrying the server's line.
func TestCasaRefusalAndErrors(t *testing.T) {
	ctx := context.Background()
	c, _ := casaServer(t, map[string]int{casa.ClaimPath: 409, casa.CertPath: 401, casa.APath: 400})
	id, refused, err := c.Claim(ctx, casa.ClaimRequest{})
	if err != nil || id != "" || refused == nil || refused.State != casa.ClaimRefused || refused.Reason != "taken" {
		t.Fatalf("Claim = %q %+v %v", id, refused, err)
	}
	if _, err := c.Cert(ctx, casa.CertRequest{}); err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Cert err = %v", err)
	}
	if err := c.SetA(ctx, casa.ARequest{}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("SetA err = %v", err)
	}
}
