// Command witness-forwarder is the anchor side of the DRBD cloud-witness link: it
// listens on a localhost address (the anchor's DRBD dials it as if it were the witness peer) and
// tunnels each connection over mTLS to the cloud witness-proxy, presenting the anchor's client
// cert. So the anchor's DRBD kernel speaks only plaintext-localhost; all WAN exposure is this hop.
//
//	witness-forwarder -addr 127.0.0.1:7789 -target witness.example:7788 \
//	    -cert n1.crt -key n1.key -ca briard-ca.crt -servername witness.test
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"log"
	"net"
	"os"
	"time"

	"briard.io/shared/tlspipe"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7789", "localhost address the anchor's DRBD dials (the witness peer)")
	target := flag.String("target", "", "the cloud witness-proxy address to tunnel to")
	certFile := flag.String("cert", "", "PEM client certificate (the anchor's identity)")
	keyFile := flag.String("key", "", "PEM private key for -cert")
	caFile := flag.String("ca", "", "PEM CA bundle the witness-proxy's server cert must chain to")
	serverName := flag.String("servername", "", "expected server name (SAN) on the witness-proxy cert")
	flag.Parse()
	if *target == "" || *certFile == "" || *keyFile == "" || *caFile == "" || *serverName == "" {
		log.Fatalf("witness-forwarder: -target, -cert, -key, -ca and -servername are required")
	}

	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		log.Fatalf("witness-forwarder: load cert/key: %v", err)
	}
	caPEM, err := os.ReadFile(*caFile)
	if err != nil {
		log.Fatalf("witness-forwarder: read ca: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		log.Fatalf("witness-forwarder: no certificates parsed from %s", *caFile)
	}

	fwd := &tlspipe.Forwarder{
		Target: *target,
		TLS: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			ServerName:   *serverName,
			MinVersion:   tls.VersionTLS13,
		},
		Dialer: &net.Dialer{Timeout: 10 * time.Second},
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("witness-forwarder: listen %s: %v", *addr, err)
	}
	log.Printf("witness-forwarder: %s -> mTLS %s", ln.Addr(), *target)
	if err := fwd.Serve(context.Background(), ln); err != nil {
		log.Fatalf("witness-forwarder: %v", err)
	}
}
