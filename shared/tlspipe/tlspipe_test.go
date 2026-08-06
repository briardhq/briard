package tlspipe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// --- compact in-memory CA + leaf minting ------------------------------------

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newCA(t *testing.T) *ca {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tlspipe-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &ca{cert: cert, key: key, pool: pool}
}

func (c *ca) issue(t *testing.T, cn string, server bool) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{"witness.test"}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// --- an mTLS echo server (the stand-in for the cloud witness proxy) ----------

// mtlsEcho listens with RequireAndVerifyClientCert and echoes bytes; sawClientCert records that a
// verified client cert was actually presented (proves the forwarder did mTLS, not plain TLS).
func mtlsEcho(t *testing.T, c *ca, sawClientCert *atomic.Bool) net.Listener {
	t.Helper()
	cfg := &tls.Config{
		Certificates: []tls.Certificate{c.issue(t, "witness", true)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    c.pool,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				tc := conn.(*tls.Conn)
				if tc.Handshake() == nil && len(tc.ConnectionState().PeerCertificates) > 0 {
					sawClientCert.Store(true)
				}
				io.Copy(conn, conn)
				conn.Close()
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// --- test --------------------------------------------------------------------

func TestForwarderTunnelsLocalToMTLSTarget(t *testing.T) {
	c := newCA(t)
	var saw atomic.Bool
	server := mtlsEcho(t, c, &saw)

	f := &Forwarder{
		Target: server.Addr().String(),
		TLS: &tls.Config{
			Certificates: []tls.Certificate{c.issue(t, "flockA:n1", false)}, // the anchor's client cert
			RootCAs:      c.pool,
			ServerName:   "witness.test",
		},
		Dialer: &net.Dialer{Timeout: 2 * time.Second},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0") // the localhost endpoint the local DRBD would dial
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Serve(ctx, ln)

	// A plaintext local connection is tunnelled over mTLS to the echo server and back.
	local, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	local.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := local.Write([]byte("drbd-bytes")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("drbd-bytes"))
	if _, err := io.ReadFull(local, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "drbd-bytes" {
		t.Fatalf("echo = %q, want %q", buf, "drbd-bytes")
	}
	if !saw.Load() {
		t.Fatal("the forwarder must present the anchor's client cert (mTLS), but the server saw none")
	}
}
