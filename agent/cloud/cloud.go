package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"briard.io/shared/api"
)

// CloudClient is the agent↔cloud seam -- the one path to the control plane (CONTRIBUTING.md: one way per concern).
// Report fuses status-up + directives-down into the single node-initiated call the wire
// actually makes (shared/api ReportRequest/ReportResponse), so it works from behind NAT.
// HTTP talks to the real control plane; Stub keeps tests off the network.
type CloudClient interface {
	Register(ctx context.Context, info api.NodeInfo) (api.Assignment, error)
	// Report POSTs the node's report envelope (status + any queued CSR for a DirectiveCertRequest
	// / terminal directive outcomes) up, and returns the directives handed back.
	Report(ctx context.Context, req api.ReportRequest) ([]api.Directive, error)
	// ReportMetrics uploads this node's hourly telemetry aggregates -- a separate
	// up-channel from Report (raw stays home; only rolled-up min/max/avg cross the seam).
	ReportMetrics(ctx context.Context, node string, aggs []api.MetricAggregate) error
}

// HTTP is the real CloudClient: it speaks the shared/api seam to the control plane over
// HTTP. Node-initiated (outbound POST), so it works from behind NAT -- the v2-cloud shape.
type HTTP struct {
	reportURL   string
	registerURL string
	metricsURL  string
	token       string // bearer presented on every call; "" -> none
	hc          *http.Client
}

// NewHTTP builds a client that talks to base (the controller/cloud root URL), presenting
// token as a bearer on every call (token "" -> no Authorization header).
func NewHTTP(base, token string) *HTTP {
	base = strings.TrimRight(base, "/")
	return &HTTP{
		reportURL:   base + api.ReportPath,
		registerURL: base + api.RegisterPath,
		metricsURL:  base + api.MetricsPath,
		token:       token,
		hc:          &http.Client{Timeout: 5 * time.Second},
	}
}

// Auth sets the bearer header on req when a token is configured.
func (h *HTTP) auth(req *http.Request) {
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
}

// Report POSTs this node's status up to the control plane and returns any directives it
// hands back in the same response.
func (h *HTTP) Report(ctx context.Context, req api.ReportRequest) ([]api.Directive, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.reportURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	h.auth(hreq)
	resp, err := h.hc.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("controller %s: %s", h.reportURL, resp.Status)
	}
	var rr api.ReportResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return nil, err
	}
	return rr.Directives, nil
}

// Register POSTs this node's NodeInfo up and returns the tenant/role Assignment the cloud
// hands back -- the node's first-contact identity call.
func (h *HTTP) Register(ctx context.Context, info api.NodeInfo) (api.Assignment, error) {
	body, err := json.Marshal(info)
	if err != nil {
		return api.Assignment{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.registerURL, bytes.NewReader(body))
	if err != nil {
		return api.Assignment{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	h.auth(req)
	resp, err := h.hc.Do(req)
	if err != nil {
		return api.Assignment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.Assignment{}, fmt.Errorf("cloud %s: %s", h.registerURL, resp.Status)
	}
	var a api.Assignment
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return api.Assignment{}, err
	}
	return a, nil
}

// ReportMetrics POSTs this node's hourly aggregates up. A nil/empty batch is a
// no-op (nothing to upload this cycle) so callers can call it unconditionally.
func (h *HTTP) ReportMetrics(ctx context.Context, node string, aggs []api.MetricAggregate) error {
	if len(aggs) == 0 {
		return nil
	}
	body, err := json.Marshal(api.MetricsReport{Node: node, Aggregates: aggs})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.metricsURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	h.auth(req)
	resp, err := h.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("controller %s: %s", h.metricsURL, resp.Status)
	}
	return nil
}

// Stub is an in-memory CloudClient for tests and standalone wiring: Report returns its
// canned directives and drops the status; Register returns its canned Assignment;
// ReportMetrics records the last uploaded batch so tests can assert on it.
type Stub struct {
	Assignment   api.Assignment
	Directives   []api.Directive
	CSR          []byte                 // last CSR uploaded via Report
	Outcomes     []api.DirectiveOutcome // last directive outcomes uploaded via Report
	Metrics      []api.MetricAggregate  // last ReportMetrics batch
	MetricsCalls int
}

func (s *Stub) Register(context.Context, api.NodeInfo) (api.Assignment, error) {
	return s.Assignment, nil
}
func (s *Stub) Report(_ context.Context, req api.ReportRequest) ([]api.Directive, error) {
	s.CSR = req.CSR
	s.Outcomes = req.Outcomes
	return s.Directives, nil
}
func (s *Stub) ReportMetrics(_ context.Context, _ string, aggs []api.MetricAggregate) error {
	s.MetricsCalls++
	s.Metrics = aggs
	return nil
}

var (
	_ CloudClient = (*HTTP)(nil)
	_ CloudClient = (*Stub)(nil)
)
