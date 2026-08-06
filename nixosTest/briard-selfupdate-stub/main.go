// Command briard-selfupdate-stub is a TEST-ONLY harness for the self-update pivot and the
// Signed-fetch path (nixosTest/agent-selfupdate.nix), built by nixosTest/selfupdate-stub.nix.
// It is not shipped with the product — it exists only so the mechanism can be exercised
// hermetically, without a full nested-VM guest bring-up.
//
// It has two roles. As the TRIAL binary it stands in for the one piece of *agent* code in the
// update loop — deciding whether to signal systemd readiness:
//
//	ready [id]  send READY=1 (from THIS pid, so NotifyAccess=main accepts it, exactly as the real
//	            agent does on healthy convergence) then block → systemd considers the unit started
//	            → ExecStartPost/briard-commit runs → briard-agent.next committed
//	    crash   exit 1 immediately → start fails → ExecStartPost never runs → revert
//	    hang    block WITHOUT sending READY → TimeoutStartSec trips → start fails → revert
//
// As the HARNESS it drives the REAL briard.io/agent/selfupdate + briard.io/shared/sdnotify
// against a live HTTP server and $NOTIFY_SOCKET (the unit tests only use fakes):
//
//	keygen <privOut> <pubPemOut>          write an Ed25519 seed + the PKIX-PEM release keyring
//	sign   <privPath> <artifactPath>      print base64(Ed25519 sign(artifact)) — the release signer
//	serve  <addr> <dir>                   http.FileServer(dir) at addr — the release host
//	fetch  <url> <sigB64> <keyringPem> <base> <runDir>
//	                                      the real Fetcher: fetch+verify+stage+arm, exit non-zero
//	                                      on a refusal (bad/absent signature) so the test can assert
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"time"

	"briard.io/agent/selfupdate"
	"briard.io/shared/sdnotify"
)

func main() {
	args := os.Args[1:]
	verb := "ready"
	if len(args) > 0 {
		verb = args[0]
	}
	switch verb {
	case "keygen":
		keygen(args[1:])
	case "sign":
		sign(args[1:])
	case "serve":
		serve(args[1:])
	case "fetch":
		fetch(args[1:])
	default: // ready / crash / hang — the trial binary
		trial(args)
	}
}

// trial is the stand-in agent the frozen briard-exec wrapper runs (ready/crash/hang).
func trial(args []string) {
	mode := "ready"
	if len(args) > 0 {
		mode = args[0]
	}
	id := ""
	if len(args) > 1 {
		id = args[1]
	}
	// Announce on stderr → the journal, so the test can prove the trial binary actually ran
	// (a non-vacuous revert proof) before the mechanism reverts it.
	fmt.Fprintf(os.Stderr, "briard-selfupdate-stub mode=%s id=%s\n", mode, id)
	switch mode {
	case "crash":
		os.Exit(1)
	case "hang":
		block() // never READY: TimeoutStartSec must catch the up-but-unhealthy hang
	default: // "ready"
		if err := sdnotify.Ready(); err != nil {
			fmt.Fprintf(os.Stderr, "briard-selfupdate-stub: sd_notify READY failed: %v\n", err)
			os.Exit(2)
		}
		block()
	}
}

// keygen writes a raw Ed25519 private key to privOut and its PKIX-PEM public key to pubOut (the
// release keyring the node trusts) — the offline release-signing root, stood in for by a fresh key.
func keygen(a []string) {
	if len(a) != 2 {
		fatal("keygen <privOut> <pubPemOut>")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	must(err)
	must(os.WriteFile(a[0], priv, 0o600))
	der, err := x509.MarshalPKIXPublicKey(pub)
	must(err)
	must(os.WriteFile(a[1], pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644))
}

// sign prints the base64 detached Ed25519 signature over the artifact bytes — the warm signer
// blessing a release. The test feeds this straight to `fetch`.
func sign(a []string) {
	if len(a) != 2 {
		fatal("sign <privPath> <artifactPath>")
	}
	priv, err := os.ReadFile(a[0])
	must(err)
	artifact, err := os.ReadFile(a[1])
	must(err)
	sig := ed25519.Sign(ed25519.PrivateKey(priv), artifact)
	fmt.Println(base64.StdEncoding.EncodeToString(sig))
}

// serve runs an http.FileServer over dir at addr — the release host the agent fetches from.
func serve(a []string) {
	if len(a) != 2 {
		fatal("serve <addr> <dir>")
	}
	must(http.ListenAndServe(a[0], http.FileServer(http.Dir(a[1]))))
}

// fetch drives the REAL selfupdate.Fetcher: fetch url, verify sig against the keyring, and — only
// on a valid signature — stage+arm under the flat layout. It exits non-zero on any refusal, so the
// test asserts refuse-and-stay end-to-end over real HTTP + real Ed25519 verification.
func fetch(a []string) {
	if len(a) != 5 {
		fatal("fetch <url> <sigB64> <keyringPem> <base> <runDir>")
	}
	url, sigB64, krPath, base, runDir := a[0], a[1], a[2], a[3], a[4]
	krPem, err := os.ReadFile(krPath)
	must(err)
	kr, err := selfupdate.NewKeyring(krPem)
	must(err)
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	must(err)
	f := &selfupdate.Fetcher{
		Layout:  selfupdate.New(base, runDir),
		Keyring: kr,
		Logf:    func(format string, v ...any) { fmt.Fprintf(os.Stderr, format+"\n", v...) },
	}
	if err := f.FetchAndStage(context.Background(), url, sig); err != nil {
		fmt.Fprintln(os.Stderr, "fetch REFUSED:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "fetch staged+armed")
}

// block sleeps forever — the healthy agent stays up; systemd's Restart=always never fires in
// steady state. (A bare select{} would panic as an all-goroutines-asleep deadlock.)
func block() {
	for {
		time.Sleep(time.Hour)
	}
}

func must(err error) {
	if err != nil {
		fatal(err.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "briard-selfupdate-stub:", msg)
	os.Exit(2)
}
