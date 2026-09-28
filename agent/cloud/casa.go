package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"briard.io/shared/casa"
)

// Casa is the node's client for the casa name service (shared/casa): the cloud's casa API for
// the claim and the certificate, and the Worker for the address. A surface of its own beside
// the managed channel (CloudClient), because a casa node is not a managed one -- it opens a
// claim, polls it, asks for a certificate at 30 days out, and writes its own address; nothing
// periodic goes up, and no bearer token exists: every request after the claim is signed by the
// node with the key the cloud bound at registration.
type Casa struct {
	base   string // the cloud's public origin, e.g. https://api.briard.io
	worker string // the Worker's origin, e.g. https://casa.briard.io
	hc     *http.Client
	// certHC is the client for CertPath alone: the cloud runs a real ACME DNS-01 solve inline
	// (the Worker publishes the TXT, Let's Encrypt looks it up, the order finalizes), which is
	// a minute or two on a bad day and must not be cut off after the cert was issued.
	certHC *http.Client
}

// NewCasa builds a client for the cloud at base and the Worker at worker.
func NewCasa(base, worker string) *Casa {
	return &Casa{
		base:   strings.TrimRight(base, "/"),
		worker: strings.TrimRight(worker, "/"),
		hc:     &http.Client{Timeout: 10 * time.Second},
		certHC: &http.Client{Timeout: 4 * time.Minute},
	}
}

// Claim opens a claim. A 409 is not an error but an answer: the name belongs to another email,
// and refused carries what to tell the person.
func (c *Casa) Claim(ctx context.Context, req casa.ClaimRequest) (id string, refused *casa.ClaimStatus, err error) {
	resp, err := c.post(ctx, c.hc, c.base+casa.ClaimPath, req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var cr casa.ClaimResponse
		if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
			return "", nil, fmt.Errorf("casa claim: decode: %w", err)
		}
		return cr.ID, nil, nil
	case http.StatusConflict:
		var st casa.ClaimStatus
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			return "", nil, fmt.Errorf("casa claim: decode refusal: %w", err)
		}
		return "", &st, nil
	}
	return "", nil, failure("casa claim", resp)
}

// ClaimStatus polls a claim by the id Claim returned.
func (c *Casa) ClaimStatus(ctx context.Context, id string) (casa.ClaimStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+casa.ClaimPath+"/"+id, nil)
	if err != nil {
		return casa.ClaimStatus{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return casa.ClaimStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return casa.ClaimStatus{}, failure("casa claim status", resp)
	}
	var st casa.ClaimStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return casa.ClaimStatus{}, fmt.Errorf("casa claim status: decode: %w", err)
	}
	return st, nil
}

// Cert asks the cloud to sign the CSR in req (already signed by the node's key). Returns the PEM
// chain, leaf first.
func (c *Casa) Cert(ctx context.Context, req casa.CertRequest) (string, error) {
	resp, err := c.post(ctx, c.certHC, c.base+casa.CertPath, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", failure("casa cert", resp)
	}
	var cr casa.CertResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("casa cert: decode: %w", err)
	}
	return cr.Cert, nil
}

// SetA points the flock's names at an address, directly at the Worker -- the cloud is not in
// this path, so a home whose address moved does not wait on our box.
func (c *Casa) SetA(ctx context.Context, req casa.ARequest) error {
	resp, err := c.post(ctx, c.hc, c.worker+casa.APath, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return failure("casa address", resp)
	}
	return nil
}

func (c *Casa) post(ctx context.Context, hc *http.Client, url string, v any) (*http.Response, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return hc.Do(req)
}

// failure renders a non-200 as an error carrying the server's one line, which is written for the
// person (the cloud's refusals say what to do; the Worker's say what it would not accept).
func failure(what string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	text := strings.TrimSpace(string(raw))
	var j struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &j) == nil && j.Error != "" {
		text = j.Error
	}
	if text == "" {
		text = resp.Status
	}
	return fmt.Errorf("%s: HTTP %d: %s", what, resp.StatusCode, text)
}
