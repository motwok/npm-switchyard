package wire

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
)

// countConn counts writes and fails write number failAt (1-based; 0 = never).
type countConn struct {
	net.Conn
	writes atomic.Int32
	failAt int32
}

func (c *countConn) Write(b []byte) (int, error) {
	if n := c.writes.Add(1); c.failAt > 0 && n >= c.failAt {
		return 0, errors.New("write fails")
	}
	return c.Conn.Write(b)
}

// runCounted performs one handshake with the dialer's (or listener's) writes counted.
func runCounted(t *testing.T, dialSide bool, failAt int32) (int32, error) {
	t.Helper()
	a, b := key(1), key(2)
	la, _ := NewLocal(a, []Partner{{Own: a, Peer: pub(b)}})
	lb, _ := NewLocal(b, []Partner{{Own: b, Peer: pub(a)}})
	c1, c2 := pairConns(t)
	var cc *countConn
	if dialSide {
		cc = &countConn{Conn: c1, failAt: failAt}
		ch := make(chan struct{})
		go func() { _, _ = lb.Accept(c2); c2.Close(); close(ch) }()
		_, err := la.Dial(cc, 0, KindControl)
		c1.Close()
		<-ch
		return cc.writes.Load(), err
	}
	cc = &countConn{Conn: c2, failAt: failAt}
	ch := make(chan struct{})
	go func() { _, _ = la.Dial(c1, 0, KindControl); c1.Close(); close(ch) }()
	_, err := lb.Accept(cc)
	c2.Close()
	<-ch
	return cc.writes.Load(), err
}

// The hello is the last write of each side; failing exactly that write is refused.
func TestHelloWriteFails(t *testing.T) {
	for _, dialSide := range []bool{true, false} {
		n, err := runCounted(t, dialSide, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runCounted(t, dialSide, n); err == nil {
			t.Fatalf("dialSide=%v: failed hello write accepted", dialSide)
		}
	}
}
