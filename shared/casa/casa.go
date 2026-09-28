// Package casa is the contract of the casa name service: `<flock>.briard.casa`, the free
// name a household may claim from the dashboard, with a wildcard certificate the node keeps
// renewed. It is shared because three parties speak it and none may drift from the others:
// the node (claims, signs, renews), the cloud's casa API (verifies the email, binds the
// name, signs the CSR), and the Worker that holds the zone (verifies the node's signed
// address writes -- in JavaScript, held to this file by a golden the cloud's tests mint).
//
// What the node holds is one long-term Ed25519 keypair, generated on the host at claim
// time and never sent anywhere; the cloud binds its public key to the name once, at
// registration, and from then on the node authenticates by SIGNING requests rather than by
// presenting a secret. Two verifiers, and one of them is the cloud itself: a bearer token
// shown to the cloud on every request would let a compromised cloud impersonate any node to
// the Worker, which is the one thing the design moved the zone token to the Worker to
// prevent. A signature is bound to one request (the name, what it asks, and when), so a
// captured request is not an owned name, and nothing stored in the cloud or the Worker is a
// secret.
//
// The canonical forms are deliberately three lines, not JSON: JSON canonicalization is a
// place two languages disagree, and a newline-joined tuple is not.
package casa

import (
	"encoding/base64"
	"strconv"
	"time"
)

// Zone is the household zone. `briard.io` is the cloud's own; names for people live here.
const Zone = "briard.casa"

// Names returns the two DNS names a flock's certificate covers and its A records answer for:
// the apex (`<flock>.briard.casa`, the dashboard) and the wildcard (every service under it).
// A wildcard does not match its own apex, so both are needed and both are always written
// together -- a name is one unit, never two records that can disagree.
func Names(flock string) []string {
	return []string{flock + "." + Zone, "*." + flock + "." + Zone}
}

// Cloud casa API paths, relative to the cloud's base URL. A separate surface from the managed
// node channel (shared/api): a casa node sends a claim, its polls, and a CSR -- nothing
// periodic, and never a heartbeat.
const (
	// ClaimPath: POST a ClaimRequest, get a ClaimResponse. GET ClaimPath + "/" + id polls it
	// (a ClaimStatus). The claim is anonymous -- the node has no key the cloud trusts yet.
	ClaimPath = "/casa/claim"
	// VerifyPath + secret is the magic link the email carries; a browser GETs it and sees
	// a page. Single use, 20 minutes.
	VerifyPath = "/casa/verify/"
	// CertPath: POST a CertRequest signed with the bound key, get a CertResponse. The node
	// drives its own renewal through this path, at 30 days out.
	CertPath = "/casa/cert"
)

// APath is the Worker's node verb: POST an ARequest signed with the bound key, and both A
// records for the name point at the address. The cloud is not in this path.
const APath = "/a"

// Skew is how far a signed request's timestamp may sit from the verifier's clock. Five minutes
// covers a home's clock being wrong by more than any NTP-synced node ever is, and bounds how
// long a captured request could be replayed to the same effect.
const Skew = 5 * time.Minute

// Fresh reports whether ts (unix seconds) is within Skew of now.
func Fresh(ts int64, now time.Time) bool {
	d := now.Unix() - ts
	return d <= int64(Skew/time.Second) && d >= -int64(Skew/time.Second)
}

// ClaimRequest opens a claim: this flock name, for this email, bound to this key. PubKey is
// the raw 32-byte Ed25519 public key (base64 on the wire, as encoding/json does).
type ClaimRequest struct {
	Flock  string `json:"flock"`
	Email  string `json:"email"`
	PubKey []byte `json:"pubkey"`
}

// ClaimResponse is the handle the node polls with. It is not the secret in the email: a node
// that could verify its own claim would make the email verification decorative.
type ClaimResponse struct {
	ID string `json:"id"`
}

// ClaimStatus is what a poll returns.
type ClaimStatus struct {
	State  string `json:"state"`            // one of the Claim* states below
	Reason string `json:"reason,omitempty"` // for ClaimRefused: what to tell the person
}

const (
	ClaimPending    = "pending"    // link not clicked yet
	ClaimRegistered = "registered" // verified and bound: the node may write its address and ask for a cert
	ClaimExpired    = "expired"    // the link's 20 minutes passed; claim again
	ClaimRefused    = "refused"    // the name belongs to another email; rename and claim again
)

// CertRequest asks the cloud to sign a CSR for the flock's two names. Signed with the bound
// key over CanonicalCert.
type CertRequest struct {
	Flock string `json:"flock"`
	CSR   []byte `json:"csr"` // DER
	TS    int64  `json:"ts"`  // unix seconds
	Sig   []byte `json:"sig"`
}

// CertResponse carries the signed chain. No key: the node made it and kept it.
type CertResponse struct {
	Cert string `json:"cert"` // PEM, leaf first
}

// ARequest points the flock's names at an address. Signed with the bound key over
// CanonicalA. IP must be a private IPv4 address -- the Worker refuses anything else, which
// is what makes a stranger's subdomain of our brand worthless for phishing.
type ARequest struct {
	Name string `json:"name"` // the flock label, not a FQDN
	IP   string `json:"ip"`
	TS   int64  `json:"ts"`
	Sig  []byte `json:"sig"`
}

// CanonicalA is the bytes an ARequest's signature covers.
func CanonicalA(name, ip string, ts int64) []byte {
	return []byte(name + "\n" + ip + "\n" + strconv.FormatInt(ts, 10))
}

// CanonicalCert is the bytes a CertRequest's signature covers. The CSR is base64 (standard
// alphabet, padded -- what encoding/json puts on the wire) so the form stays printable and
// the signature binds the exact CSR, not a digest of it.
func CanonicalCert(flock string, csrDER []byte, ts int64) []byte {
	return []byte(flock + "\n" + base64.StdEncoding.EncodeToString(csrDER) + "\n" + strconv.FormatInt(ts, 10))
}
