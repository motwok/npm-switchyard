package wire

import (
	"context"
	"testing"
)

func TestStreamInfo(t *testing.T) {
	if StreamFrom(context.Background()) != (StreamInfo{}) {
		t.Fatal("empty context")
	}
	s := StreamInfo{ID: "abcd", Peer: "b"}
	if StreamFrom(WithStream(context.Background(), s)) != s {
		t.Fatal("round trip")
	}
	a, b := NewStreamID(), NewStreamID()
	if len(a) != 8 || a == b {
		t.Fatalf("ids %q %q", a, b)
	}
}

// Both ends of a connection derive the same stream ID; another connection gets another.
func TestStreamIDBothEnds(t *testing.T) {
	a, b := key(1), key(2)
	la, _ := NewLocal(a, []Partner{{Own: a, Peer: pub(b)}})
	lb, _ := NewLocal(b, []Partner{{Own: b, Peer: pub(a)}})
	d, err, r := handshake(t, la, 0, lb)
	if err != nil || r.err != nil {
		t.Fatal(err, r.err)
	}
	if len(d.StreamID()) != 8 || d.StreamID() != r.c.StreamID() {
		t.Fatalf("dialer %q listener %q", d.StreamID(), r.c.StreamID())
	}
	d2, _, _ := handshake(t, la, 0, lb)
	if d2.StreamID() == d.StreamID() {
		t.Fatal("same id for two connections")
	}
}
