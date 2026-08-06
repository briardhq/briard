package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxArtifactSize bounds an agent-update download so a malicious or misconfigured URL can't
// exhaust memory/disk before the signature is even checked. The static agent binary is ~20 MB;
// 128 MB is generous headroom while still a hard ceiling.
const maxArtifactSize = 128 << 20

// Fetcher downloads a signed agent artifact, verifies it against the release keyring, and — only
// on a valid signature — stages+arms it for the trial (StageNext + Arm). It is the 4c
// glue between the CloudClient seam (a DirectiveAgentUpdate offers a URL + signature) and the
// frozen self-update pivot. It deliberately does NOT restart the unit: arming is enough, and the
// caller owns the restart (a detached helper, so the process being replaced isn't the one issuing
// the restart — the decoupling).
type Fetcher struct {
	Layout  Layout
	Keyring *Keyring
	Client  *http.Client         // nil -> a default client with a bounded timeout
	Logf    func(string, ...any) // nil -> discard
}

// FetchAndStage downloads url, verifies sig against the keyring, and stages+arms the artifact.
// The ORDER is the safety property: it verifies BEFORE it writes anything, so any fetch or
// signature failure returns before StageNext and the committed binary is untouched
// (refuse-and-stay). A returned error means "current kept"; nil means agent.next is staged and
// armed for the next restart. The whole artifact is held in memory to verify the detached
// Ed25519 signature over the exact bytes (bounded by maxArtifactSize).
func (f *Fetcher) FetchAndStage(ctx context.Context, url string, sig []byte) error {
	logf := f.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if f.Keyring == nil {
		return ErrNoKeys // no ring wired == fail closed, same as an empty ring
	}

	artifact, err := f.fetch(ctx, url)
	if err != nil {
		return fmt.Errorf("selfupdate: fetch %s: %w", url, err)
	}
	// Verify FIRST — nothing touches disk until the signature checks out.
	if err := f.Keyring.Verify(artifact, sig); err != nil {
		return err // ErrUnsigned / ErrBadSignature / ErrNoKeys — the caller alerts + keeps current
	}
	logf("selfupdate: artifact verified (%d bytes) — staging + arming", len(artifact))
	if err := f.Layout.StageNext(bytes.NewReader(artifact)); err != nil {
		return fmt.Errorf("selfupdate: stage: %w", err)
	}
	if err := f.Layout.Arm(); err != nil {
		return fmt.Errorf("selfupdate: arm: %w", err)
	}
	return nil
}

// Fetch GETs url and returns its body, refusing a non-200 or an over-limit response.
func (f *Fetcher) fetch(ctx context.Context, url string) ([]byte, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	// +1 so a body of exactly maxArtifactSize+1 is caught rather than silently truncated.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArtifactSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxArtifactSize {
		return nil, fmt.Errorf("artifact exceeds %d bytes", maxArtifactSize)
	}
	return data, nil
}
