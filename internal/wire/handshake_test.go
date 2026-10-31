package wire

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

// pairConns gives two connected TCP sockets.
func pairConns(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c1, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c2, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c1.Close(); c2.Close() })
	return c1, c2
}

func ecdsaCert(t *testing.T) tls.Certificate {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

func TestVersionMismatch(t *testing.T) {
	a, b := key(1), key(2)
	la, _ := NewLocal(a, []Partner{{Own: a, Peer: pub(b)}})
	lb, _ := NewLocal(b, []Partner{{Own: b, Peer: pub(a)}})
	lb.verMin, lb.verMax = 2, 2
	if _, err, r := handshake(t, la, 0, lb); err == nil || r.err == nil {
		t.Fatalf("both sides must refuse: %v / %v", err, r.err)
	}
}

func TestListenerNotEd25519(t *testing.T) {
	a, b := key(1), key(2)
	la, _ := NewLocal(a, []Partner{{Own: a, Peer: pub(b)}})
	c1, c2 := pairConns(t)
	go func() {
		s := tls.Server(c2, &tls.Config{Certificates: []tls.Certificate{ecdsaCert(t)}, ClientAuth: tls.RequestClientCert})
		_ = s.Handshake()
		c2.Close()
	}()
	if _, err := la.Dial(c1, 0, KindControl); err == nil {
		t.Fatal("ECDSA listener accepted")
	}
}

func TestDialerNotEd25519(t *testing.T) {
	a, b := key(1), key(2)
	lb, _ := NewLocal(b, []Partner{{Own: b, Peer: pub(a)}})
	c1, c2 := pairConns(t)
	go func() {
		c := tls.Client(c1, &tls.Config{Certificates: []tls.Certificate{ecdsaCert(t)}, InsecureSkipVerify: true}) //nolint:gosec
		_ = c.Handshake()
		c1.Close()
	}()
	if _, err := lb.Accept(c2); err == nil {
		t.Fatal("ECDSA dialer accepted")
	}
}

// A dialer that sends an unknown SNI gets the global key; it is accepted if that is the key
// configured for it, and the dialer that closes before its hello is refused.
func TestGlobalKeyAndNoHello(t *testing.T) {
	a, b := key(1), key(2)
	la, _ := NewLocal(a, []Partner{{Own: a, Peer: pub(b)}})
	lb, _ := NewLocal(b, []Partner{{Own: b, Peer: pub(a)}})
	c1, c2 := pairConns(t)
	ch := make(chan error, 1)
	go func() { _, err := lb.Accept(c2); ch <- err }()
	cert := la.certs[Fingerprint(pub(a))]
	c := tls.Client(c1, &tls.Config{Certificates: []tls.Certificate{cert}, ServerName: "unknown", InsecureSkipVerify: true}) //nolint:gosec
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	c.Close() // no hello
	if err := <-ch; err == nil {
		t.Fatal("dialer without hello accepted")
	}
}

func TestDialNoHello(t *testing.T) {
	a, b := key(1), key(2)
	la, _ := NewLocal(a, []Partner{{Own: a, Peer: pub(b)}})
	lb, _ := NewLocal(b, []Partner{{Own: b, Peer: pub(a)}})
	c1, c2 := pairConns(t)
	go func() {
		s := tls.Server(c2, &tls.Config{Certificates: []tls.Certificate{lb.global}, ClientAuth: tls.RequireAnyClientCert})
		if s.Handshake() == nil {
			_, _ = readHello(s)
		}
		s.Close() // listener closes without its hello
	}()
	if _, err := la.Dial(c1, 0, KindControl); err == nil {
		t.Fatal("listener without hello accepted")
	}
}

func TestCertificateFailure(t *testing.T) {
	old := createCertificate
	defer func() { createCertificate = old }()
	createCertificate = func(io.Reader, *x509.Certificate, *x509.Certificate, any, any) ([]byte, error) {
		return nil, errors.New("no certificate")
	}
	if _, err := NewLocal(key(1), nil); err == nil {
		t.Fatal("global certificate error ignored")
	}
	calls := 0
	createCertificate = func(r io.Reader, a, b *x509.Certificate, p, k any) ([]byte, error) {
		if calls++; calls > 1 {
			return nil, errors.New("no certificate")
		}
		return old(r, a, b, p, k)
	}
	if _, err := NewLocal(key(1), []Partner{{Own: key(2), Peer: pub(key(3))}}); err == nil {
		t.Fatal("partner certificate error ignored")
	}
}
