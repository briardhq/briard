// Command catalog-sign is a TEST/LAB HELPER (nixosTest/fixture-app.nix), not a product binary.
//
// It builds the smallest possible SIGNED CATALOG: a list naming one app version, a detached
// Ed25519 signature over exactly the list's bytes, the manifest beside it as the version file the
// list names by its hash, and the PEM keyring that verifies the list. That is the whole
// published-catalog contract (shared/manifest.Catalog.Fetch): `latest.json`, `latest.json.sig`,
// `<name>/<version>.json`, and a trust root the node is given out of band; a node's CATALOG_URL
// is the served directory's `latest.json`.
//
// WHY IT EXISTS. A harness that wants an app installed the way a USER installs one cannot
// shortcut the catalog: `fetchManifest` fails CLOSED with no keyring, and verifies the signature
// over the raw bytes of the list before parsing it. Both are deliberate, so the honest way to give the lab
// fleet an app is to give it a real signed catalog rather than to weaken the path under test.
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
// SEVERAL CATALOGS, ONE KEY, and that is what the pair form is for. A version ROTATION publishes
// a second manifest for the SAME app name over the first, and the node that fetches it holds
// exactly one keyring -- given to it, out of band, before any of this (the fleet and
// soak rigs rotate it). So each catalog gets its own directory (each has its own `latest.json`), copied
// over the served one to publish it, and they all verify against the same trust root. The private key never leaves this process.
//
//	catalog-sign <manifest.json> <outdir> [<manifest.json> <outdir>]...
//
// Each outdir also gets `<name>.identity` -- the manifest's content hash, which is what the cloud
// confirms an app rollout against (shared/manifest.Identity). It is written here because it is
// a property of the exact bytes signed here, and re-deriving it in a shell script is how the two
// drift.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"briard.io/shared/manifest"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 || len(args)%2 != 0 {
		fatal("usage: catalog-sign <manifest.json> <outdir> [<manifest.json> <outdir>]...")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatal("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		fatal("marshal public key: %v", err)
	}
	keyring := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	for i := 0; i < len(args); i += 2 {
		sign(args[i], args[i+1], priv, keyring)
	}
}

// sign writes one published catalog: the manifest as read, at `<name>/<version>.json`; a list
// naming it, `latest.json`, with the list's signature; the manifest's identity; and the keyring
// that verifies the list.
func sign(src, out string, priv ed25519.PrivateKey, keyring []byte) {
	raw, err := os.ReadFile(src)
	if err != nil {
		fatal("read manifest: %v", err)
	}
	// Parse to learn the NAME and VERSION the catalog serves it under -- and, incidentally, to
	// refuse here what the node would refuse later. A catalog that publishes an invalid manifest
	// is a slower way to discover the same thing.
	m, id, err := manifest.Parse(raw)
	if err != nil {
		fatal("parse manifest: %v", err)
	}
	// The list is the node's only signed read, and it carries what a page shows of the app: a
	// fixture's is the least the list's own validation accepts.
	list, err := json.Marshal(manifest.Index{Apps: []manifest.App{{
		Name: m.Name, Available: true, Version: m.Version, Identity: id,
		Title: m.Name, Functionality: "a test fixture", Description: "An app a test installs.",
		Homepage: "https://example.org",
	}}})
	if err != nil {
		fatal("marshal list: %v", err)
	}
	if _, err := manifest.ParseIndex(list); err != nil {
		fatal("list: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(out, m.Name), 0o755); err != nil {
		fatal("mkdir: %v", err)
	}
	write := func(name string, body []byte) {
		if err := os.WriteFile(filepath.Join(out, name), body, 0o644); err != nil {
			fatal("write %s: %v", name, err)
		}
	}
	// The version file is the bytes as READ, never a re-marshalling: the manifest's content hash
	// IS the app's identity, so serving anything else would serve a different app than the one
	// the list names.
	write(filepath.Join(m.Name, m.Version+".json"), raw)
	write(listName, list)
	write(listName+".sig", ed25519.Sign(priv, list))
	write(m.Name+".identity", []byte(id))
	write("keyring.pem", keyring)
	fmt.Printf("signed catalog for %s %s (%s) in %s\n", m.Name, m.Version, id, out)
}

// listName is the list a node reads: its CATALOG_URL is `<served root>/latest.json`.
const listName = "latest.json"

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "catalog-sign: "+format+"\n", a...)
	os.Exit(1)
}
