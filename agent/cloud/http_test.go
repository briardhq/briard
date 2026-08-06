package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"briard.io/shared/api"
	"briard.io/shared/model"
)

// HTTP.Report POSTs a well-formed report to ReportPath and returns the directives handed
// back (the fused status-up + directives-down call).
func TestReportRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.ReportPath {
			t.Errorf("path = %q, want %q", r.URL.Path, api.ReportPath)
		}
		var req api.ReportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Status.NodeName != "n1" {
			t.Errorf("reported node = %q, want n1", req.Status.NodeName)
		}
		json.NewEncoder(w).Encode(api.ReportResponse{
			Directives: []api.Directive{{Kind: api.DirectiveLog, Payload: "hi"}},
		})
	}))
	defer srv.Close()

	ds, err := NewHTTP(srv.URL, "").Report(context.Background(), api.ReportRequest{Status: api.NodeStatus{NodeName: "n1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Kind != api.DirectiveLog || ds[0].Payload != "hi" {
		t.Errorf("directives = %+v, want one log/hi", ds)
	}
}

// HTTP.Register POSTs NodeInfo up and returns the Assignment the cloud hands back.
func TestRegisterRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.RegisterPath {
			t.Errorf("path = %q, want %q", r.URL.Path, api.RegisterPath)
		}
		var info api.NodeInfo
		if err := json.NewDecoder(r.Body).Decode(&info); err != nil {
			t.Fatal(err)
		}
		if info.NodeName != "n1" {
			t.Errorf("registered node = %q, want n1", info.NodeName)
		}
		json.NewEncoder(w).Encode(api.Assignment{Tenant: "default", Role: info.Role})
	}))
	defer srv.Close()

	a, err := NewHTTP(srv.URL, "").Register(context.Background(), api.NodeInfo{NodeName: "n1", Role: model.RoleAnchor})
	if err != nil {
		t.Fatal(err)
	}
	if a.Tenant != "default" || a.Role != model.RoleAnchor {
		t.Errorf("assignment = %+v, want default/anchor", a)
	}
}

// HTTP.ReportMetrics POSTs the node's aggregates to MetricsPath; an empty batch is a no-op
// (no request at all), so the observe loop can call it unconditionally.
func TestReportMetricsRoundTrip(t *testing.T) {
	var got api.MetricsReport
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != api.MetricsPath {
			t.Errorf("path = %q, want %q", r.URL.Path, api.MetricsPath)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	h := NewHTTP(srv.URL, "")

	// An empty batch never hits the wire.
	if err := h.ReportMetrics(context.Background(), "n1", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("empty batch made %d calls, want 0", calls)
	}

	aggs := []api.MetricAggregate{{Metric: "load1", Min: 0.1, Max: 0.9, Avg: 0.5, Samples: 3}}
	if err := h.ReportMetrics(context.Background(), "n1", aggs); err != nil {
		t.Fatal(err)
	}
	if got.Node != "n1" || len(got.Aggregates) != 1 || got.Aggregates[0].Metric != "load1" {
		t.Errorf("uploaded = %+v, want node n1 with one load1 aggregate", got)
	}
}

// A control-plane error surfaces (so the observe loop can log + ride it out), not a panic.
func TestReportServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := NewHTTP(srv.URL, "").Report(context.Background(), api.ReportRequest{Status: api.NodeStatus{NodeName: "n1"}}); err == nil {
		t.Error("expected an error on 500")
	}
}

// A configured token is presented as a bearer on every call; "" sends no auth header.
func TestReportPresentsBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(api.ReportResponse{})
	}))
	defer srv.Close()

	if _, err := NewHTTP(srv.URL, "sekret").Report(context.Background(), api.ReportRequest{Status: api.NodeStatus{NodeName: "n1"}}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sekret")
	}

	gotAuth = "unset"
	if _, err := NewHTTP(srv.URL, "").Report(context.Background(), api.ReportRequest{Status: api.NodeStatus{NodeName: "n1"}}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("empty token must send no Authorization header, got %q", gotAuth)
	}
}
