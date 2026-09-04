// Package dashboard is the host→guest HANDOFF behind the household dashboard's one-time code
// ([V3b.31b]): the file the host writes onto the guest's tmpfs through the `dashboard.handoff`
// verb, and the dashboard consumes exactly once.
//
// The code is the whole of the dashboard's bootstrap authentication. Proof of identity is proof
// of access to the `briard` CLI ([V3b.31a](a)) -- whoever can drive it already owns the node --
// so the CLI asks the agent to mint a code, the agent hands it to the guest here, and the browser
// that presents it becomes a trusted device. Nothing about a person is authenticated beyond that,
// on purpose: there is no briard password to invent, hide or reset, and `briard dashboard` IS the
// reset.
//
// It also carries what the host knows about the OS account, because that is where the account
// is visible: the dashboard creates Home Assistant's first user from it ([V3b.31a](d)).
package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Dir is the guest-side directory, on tmpfs and node-local like the routing table beside it: a
// code is minted for the node that holds the VIP now, and a failover mints a new one.
const Dir = "/run/briard/dashboard"

// HandoffPath is the file the verb writes (0600, root) and the dashboard reads then removes.
const HandoffPath = Dir + "/handoff.json"

// TTL is how long a code is presentable. Ten minutes matches Home Assistant's own auth-code
// lifetime and is long enough to walk from the terminal to a phone.
const TTL = 10 * time.Minute

// Handoff is the file's shape.
type Handoff struct {
	// Code is 32 random bytes, hex. Compared in constant time, consumed on first use.
	Code string `json:"code"`
	// Name, Username and Language describe the OS account the CLI ran under, for the Home
	// Assistant user the dashboard creates. Empty means the dashboard picks defaults.
	Name     string `json:"name,omitempty"`
	Username string `json:"username,omitempty"`
	Language string `json:"language,omitempty"`
	// Issued is when the host minted the code; TTL runs from it.
	Issued time.Time `json:"issued"`
}

// NewCode mints a fresh code.
func NewCode() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("dashboard: mint code: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Expired says whether a handoff is past its TTL at `now`.
func (h Handoff) Expired(now time.Time) bool {
	return now.After(h.Issued.Add(TTL))
}

// URL is where the household opens the dashboard with this code: the node's own name at the
// front door, which forwards every name it does not route to the dashboard.
func URL(flock, code string) string {
	return "http://briard-" + flock + ".local/?code=" + code
}
