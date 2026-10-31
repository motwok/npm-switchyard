package wire

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net"
	"testing"
)

var keyA = bytes.Repeat([]byte{1}, 32)
var keyB = bytes.Repeat([]byte{2}, 32)

func TestHandshake(t *testing.T) {
	dl, _ := NewLocal()
	ll, _ := NewLocal()
	c1, c2 := net.Pipe()
	type res struct {
		c   *Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ll.Accept(c2, [][]byte{keyA, keyB})
		ch <- res{c, err}
	}()
	d, err := dl.Dial(c1, keyB, KindControl)
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.c.Partner != 1 || r.c.Kind != KindControl || r.c.PeerID != dl.ID || d.PeerID != ll.ID || !d.Dialer {
		t.Fatalf("listener %+v dialer %+v", r.c, d)
	}
	go func() { _, _ = d.Write([]byte("ping")) }()
	b := make([]byte, 4)
	if _, err := io.ReadFull(r.c, b); err != nil || string(b) != "ping" {
		t.Fatal(err)
	}
}

func TestWrongKey(t *testing.T) {
	dl, _ := NewLocal()
	ll, _ := NewLocal()
	c1, c2 := net.Pipe()
	go func() {
		_, _ = ll.Accept(c2, [][]byte{keyA})
		c2.Close()
	}()
	if _, err := dl.Dial(c1, keyB, KindControl); err == nil {
		t.Fatal("dial with unknown key succeeded")
	}
}

// A listener without the PSK must not pass by echoing the dialer's proof (PAR-2).
func TestNoReflection(t *testing.T) {
	ekm := bytes.Repeat([]byte{9}, 32)
	if bytes.Equal(Proof(keyA, "dialer", ekm), Proof(keyA, "listener", ekm)) {
		t.Fatal("proofs of both roles are equal")
	}
	dl, _ := NewLocal()
	c1, c2 := net.Pipe()
	go func() {
		// Impostor: complete TLS, read the dialer's hello and proof, send them back.
		il, _ := NewLocal()
		tc := tlsServer(c2, il)
		if tc.Handshake() != nil {
			return
		}
		buf := make([]byte, 25+sha256.Size)
		if _, err := io.ReadFull(tc, buf); err != nil {
			return
		}
		_, _ = tc.Write(buf)
	}()
	if _, err := dl.Dial(c1, keyA, KindControl); err == nil {
		t.Fatal("reflected proof accepted")
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
