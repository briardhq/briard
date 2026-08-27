// Command catalog-sign is a TEST/LAB HELPER (nixosTest/fixture-service.nix), not a product binary.
//
// It builds the smallest possible SIGNED CATALOG: the manifest bytes, a detached Ed25519
// signature over exactly those bytes, and the PEM keyring that verifies it. That is the whole
// published-catalog contract (shared/manifest.Catalog.Fetch): `<name>.json`, `<name>.json.sig`,
// and a trust root the node is given out of band.
//
// WHY IT EXISTS. A harness that wants a service installed the way a USER installs one cannot
// shortcut the catalog: `fetchManifest` fails CLOSED with no keyring, and verifies the signature
// over the raw bytes before parsing them. Both are deliberate, so the honest way to give the lab
// fleet a service is to give it a real signed catalog rather than to weaken the path under test.
// (Seeding the node-local manifest cache instead was tried and is NOT equivalent: it reproduces
// what an install RECORDS but not what it DOES -- no data subvolume, so the container cannot
// start. Measured on a fleet run, 2026-08-27.)
//
// THE KEY IS GENERATED PER BUILD, and is a lab key with no relationship to the release keyring.
// Generating beats committing one: a private key in the tree is a private key in the tree, even
// a worthless one, and a reader has to stop and work out which it is. Ed25519 keys do not
// expire, so this carries none of the build-time-artifact staleness that bit a cert minted in a
// derivation.
//
//	catalog-sign <manifest.json> <outdir>
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"briard.io/shared/manifest"
)

func main() {
	if len(os.Args) != 3 {
		fatal("usage: catalog-sign <manifest.json> <outdir>")
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fatal("read manifest: %v", err)
	}
	// Parse to learn the NAME the catalog serves it under -- and, incidentally, to refuse here
	// what the node would refuse later. A catalog that publishes an invalid manifest is a slower
	// way to discover the same thing.
	m, _, err := manifest.Parse(raw)
	if err != nil {
		fatal("parse manifest: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatal("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		fatal("marshal public key: %v", err)
	}
	out := os.Args[2]
	if err := os.MkdirAll(out, 0o755); err != nil {
		fatal("mkdir: %v", err)
	}
	write := func(name string, body []byte) {
		if err := os.WriteFile(filepath.Join(out, name), body, 0o644); err != nil {
			fatal("write %s: %v", name, err)
		}
	}
	// The signature is over the bytes as READ, never over a re-marshalling: the manifest's
	// content hash IS the service identity, so signing anything else would sign a different
	// service than the one served.
	write(m.Name+".json", raw)
	write(m.Name+".json.sig", ed25519.Sign(priv, raw))
	write("keyring.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	fmt.Printf("signed catalog for %s in %s\n", m.Name, out)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "catalog-sign: "+format+"\n", a...)
	os.Exit(1)
}
