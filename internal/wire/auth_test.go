package wire

import (
	"bytes"
	"crypto/ed25519"
	"io"
	"net"
	"testing"
)

func key(b byte) ed25519.PrivateKey { return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, 32)) }

func pub(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

type result struct {
	c   *Conn
	err error
}

func handshake(t *testing.T, d *Local, idx int, l *Local) (*Conn, error, result) {
	t.Helper()
	// Real sockets: net.Pipe is unbuffered and stalls on TLS alerts until the deadline.
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
	ch := make(chan result, 1)
	go func() {
		c, err := l.Accept(c2)
		if err != nil {
			c2.Close()
		}
		ch <- result{c, err}
	}()
	dc, err := d.Dial(c1, idx, KindControl)
	if err != nil {
		c1.Close()
	}
	return dc, err, <-ch
}

func TestHandshake(t *testing.T) {
	a, b, x := key(1), key(2), key(3)
	// b knows partners x (index 0) and a (index 1).
	la, _ := NewLocal(a, []Partner{{Own: a, Peer: pub(b)}})
	lb, _ := NewLocal(b, []Partner{{Own: b, Peer: pub(x)}, {Own: b, Peer: pub(a)}})
	d, err, r := handshake(t, la, 0, lb)
	if err != nil || r.err != nil {
		t.Fatal(err, r.err)
	}
	if r.c.Partner != 1 || r.c.Kind != KindControl || r.c.PeerID != la.ID || d.PeerID != lb.ID || !d.Dialer {
		t.Fatalf("listener %+v dialer %+v", r.c, d)
	}
	go func() { _, _ = d.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r.c, buf); err != nil || string(buf) != "ping" {
		t.Fatal(err)
	}
}

func TestWrongListenerKey(t *testing.T) {
	a, b, x := key(1), key(2), key(3)
	la, _ := NewLocal(a, []Partner{{Own: a, Peer: pub(x)}}) // expects x, talks to b
	lb, _ := NewLocal(b, []Partner{{Own: b, Peer: pub(a)}})
	if _, err, _ := handshake(t, la, 0, lb); err == nil {
		t.Fatal("dialer accepted an unexpected listener key")
	}
}

func TestUnknownDialer(t *testing.T) {
	a, b := key(1), key(2)
	la, _ := NewLocal(a, []Partner{{Own: a, Peer: pub(b)}})
	lb, _ := NewLocal(b, []Partner{{Own: b, Peer: pub(key(9))}})
	if _, _, r := handshake(t, la, 0, lb); r.err == nil {
		t.Fatal("listener accepted an unknown dialer key")
	}
}

// The listener presents its per-partner key, chosen by the SNI fingerprint (PAR-2).
func TestPerPartnerKey(t *testing.T) {
	a, b, bForA := key(1), key(2), key(4)
	la, _ := NewLocal(a, []Partner{{Own: a, Peer: pub(bForA)}})
	lb, _ := NewLocal(b, []Partner{{Own: bForA, Peer: pub(a)}})
	if _, err, r := handshake(t, la, 0, lb); err != nil || r.err != nil {
		t.Fatal(err, r.err)
	}
}

// A partner that pins b's global key while b has an own key for it is refused.
func TestOverrideMismatch(t *testing.T) {
	a, b, bForA := key(1), key(2), key(4)
	la, _ := NewLocal(a, []Partner{{Own: a, Peer: pub(b)}})
	lb, _ := NewLocal(b, []Partner{{Own: bForA, Peer: pub(a)}})
	if _, _, r := handshake(t, la, 0, lb); r.err == nil {
		t.Fatal("listener used the global key for a partner with an own key")
	}
}

func TestNegotiate(t *testing.T) {
	if v, err := Negotiate(1, 3, 2, 5); err != nil || v != 3 {
		t.Fatal(v, err)
	}
	if _, err := Negotiate(1, 1, 2, 2); err == nil {
		t.Fatal("no common version expected")
	}
}

func TestInstanceIDLess(t *testing.T) {
	a := InstanceID{1}
	b := InstanceID{2}
	if !a.Less(b) || b.Less(a) || a.Less(a) {
		t.Fatal("Less")
	}
}

func TestFingerprint(t *testing.T) {
	f := Fingerprint(pub(key(1)))
	if len(f) != 25 || f[0] != 'k' || f != Fingerprint(pub(key(1))) || f == Fingerprint(pub(key(2))) {
		t.Fatal(f)
	}
}

func TestFrames(t *testing.T) {
	var b bytes.Buffer
	tg := Target{Proto: UDP, Port: 5353, Name: "dns"}
	_ = WriteMsg(&b, MsgRequest, MarshalRequest(17, tg))
	typ, p, err := ReadMsg(&b)
	if err != nil || typ != MsgRequest {
		t.Fatal(err)
	}
	id, got, err := UnmarshalRequest(p)
	if err != nil || id != 17 || got != tg {
		t.Fatal(id, got, err)
	}
	_ = WriteDatagram(&b, []byte("hello"))
	buf := make([]byte, 16)
	n, err := ReadDatagram(&b, buf)
	if err != nil || string(buf[:n]) != "hello" {
		t.Fatal(err)
	}
}
