// Command briard-selfupdate-stub is a TEST-ONLY harness for the self-update and the
// signed-channel serving (nixosTest/agent-selfupdate.nix), built by nixosTest/selfupdate-stub.nix.
// It is not shipped with the product — it exists only so the mechanism can be exercised
// hermetically, without a full nested-VM guest bring-up.
//
// It has two roles. As the CANDIDATE binary it stands in for the one piece of *agent* code the
// update depends on — whether it signals systemd readiness, and how it stops:
//
//	ready [id]     send READY=1 (from THIS pid, so NotifyAccess=main accepts it, exactly as the
//	               real agent does at loop entry) then block → the updater's start succeeds →
//	               the swap is committed
//	slowstop [id]  as ready, but on SIGTERM it takes a few seconds to finish (an act in flight)
//	               and leaves /run/briard/drained-<id> before exiting cleanly
//	    crash      exit 1 immediately → the start fails → the updater reverts
//	    hang       block WITHOUT sending READY → TimeoutStartSec trips → the updater reverts
//
// As the HARNESS it drives the REAL briard.io/agent/selfupdate + briard.io/shared/sdnotify
// against a live HTTP server and $NOTIFY_SOCKET (the unit tests only use fakes):
//
//	keygen <privOut> <pubPemOut>          write an Ed25519 seed + the PKIX-PEM release keyring
//	sign   <privPath> <artifactPath>      print base64(Ed25519 sign(artifact)) — the release signer
//	serve  <addr> <dir>                   http.FileServer(dir) at addr — the release host
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	default: // ready / slowstop / crash / hang — the candidate
		trial(args)
	}
}

// trial is the stand-in agent the service runs (ready/slowstop/crash/hang).
func trial(args []string) {
	mode := "ready"
	if len(args) > 0 {
		mode = args[0]
	}
	id := ""
	if len(args) > 1 {
		id = args[1]
	}
	// Announce on stderr → the journal, so the test can prove the candidate actually ran
	// (a non-vacuous revert proof) before the mechanism reverts it.
	fmt.Fprintf(os.Stderr, "briard-selfupdate-stub mode=%s id=%s\n", mode, id)
	switch mode {
	case "crash":
		os.Exit(1)
	case "hang":
		block() // never READY: TimeoutStartSec must catch the up-but-unhealthy hang
	case "slowstop":
		// The real agent finishes its act in flight on SIGTERM before it exits; a stop that
		// waits for that is the updater's, one that kills it at TimeoutStopSec is not.
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		if err := sdnotify.Ready(); err != nil {
			os.Exit(2)
		}
		<-term
		fmt.Fprintf(os.Stderr, "briard-selfupdate-stub id=%s: stopping, finishing the act in flight\n", id)
		time.Sleep(8 * time.Second)
		must(os.WriteFile("/run/briard/drained-"+id, nil, 0o644))
		fmt.Fprintf(os.Stderr, "briard-selfupdate-stub id=%s: stopped cleanly\n", id)
		os.Exit(0)
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

// block sleeps forever — the healthy agent stays up; systemd's Restart= never fires in
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
