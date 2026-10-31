package peer

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/metrics"

	"github.com/motwok/npm-switchyard/internal/wire"
)

// spares waits until partner 0 has spare connections and returns them, removed from the pool.
func takeSpares(t *testing.T, m *Manager) []*wire.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.state[0].mu.Lock()
		ch := m.state[0].spares
		n := len(ch)
		m.state[0].mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			var out []*wire.Conn
			for len(ch) > 0 {
				out = append(out, <-ch)
			}
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killSpares replaces every spare by one whose socket is gone.
func killSpares(t *testing.T, m *Manager) {
	t.Helper()
	m.setAddr(0, "127.0.0.1:1") // no refills
	sp := takeSpares(t, m)
	m.state[0].mu.Lock()
	ch := m.state[0].spares
	m.state[0].mu.Unlock()
	for _, c := range sp {
		c.Conn.NetConn().Close()
		ch <- c
	}
}

func TestAcceptRetry(t *testing.T) {
	old := acceptRetry
	acceptRetry = 10 * time.Millisecond
	m := New(nil, nil, echoExporter{}, nil, logDiscard())
	acceptRetry = old
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.acceptLoop(ctx, l); close(done) }()
	l.Close() // Accept fails while ctx is alive: retried
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
}

func TestPingFails(t *testing.T) {
	old := pingEvery
	pingEvery = 20 * time.Millisecond
	a, _ := pair(t, true, false)
	pingEvery = old
	a.state[0].mu.Lock()
	c := a.state[0].ctrl
	a.state[0].mu.Unlock()
	_ = c.CloseWrite() // reads still work, the next ping cannot be written
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.state[0].mu.Lock()
		gone := a.state[0].ctrl != c
		a.state[0].mu.Unlock()
		if gone {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("channel kept after a failed ping")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestClosedSparePool(t *testing.T) {
	a, _ := pair(t, true, false)
	ch := make(chan *wire.Conn)
	close(ch)
	// A separate state with a closed pool, as after a lost control channel.
	s := &partnerState{idx: 0, name: "b", spares: ch}
	s.addr = a.state[0].addr
	c, err := a.streamConn(context.Background(), s) // falls back to a fresh dial
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// Only dead spares and no reachable partner: Open fails after the one fresh dial.
func TestDeadSparesNoRedial(t *testing.T) {
	a, _ := pair(t, true, false)
	killSpares(t, a)
	if _, _, err := a.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"}); err == nil {
		t.Fatal("open succeeded without a usable connection")
	}
}

// The dialer attaches with a dead spare: the write fails and the listener gets no stream.
func TestAttachDeadSpare(t *testing.T) {
	old := requestWait
	requestWait = 300 * time.Millisecond
	a, b := pair(t, true, false)
	requestWait = old
	killSpares(t, a)
	if _, _, err := b.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"}); err == nil {
		t.Fatal("stream without attach")
	}
}

// The listener cannot send its request on a half-closed control channel.
func TestRequestWriteFails(t *testing.T) {
	_, b := pair(t, true, false)
	b.state[0].mu.Lock()
	c := b.state[0].ctrl
	b.state[0].mu.Unlock()
	_ = c.CloseWrite()
	if _, _, err := b.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"}); err == nil {
		t.Fatal("request on a closed channel")
	}
}

// A second attach for an id whose slot is already filled is dropped.
func TestAttachSlotFull(t *testing.T) {
	a, b := pair(t, true, false)
	ch := make(chan *wire.Conn, 1)
	ch <- nil
	b.state[0].mu.Lock()
	b.state[0].pending[99] = ch
	b.state[0].mu.Unlock()
	c, err := a.dial(context.Background(), 0, wire.KindStream)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte{wire.StreamAttach, 0, 0, 0, 99})
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("attach into a full slot kept open")
	}
}

// The status cannot be written: the stream counts as an error and the target is closed.
func TestExportStatusWriteFails(t *testing.T) {
	a, _ := pair(t, true, false)
	c, err := a.dial(context.Background(), 0, wire.KindStream)
	if err != nil {
		t.Fatal(err)
	}
	c.Conn.NetConn().Close()
	a.export(context.Background(), a.state[0], c, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"})
}

// faultConn fails reads or writes on request.
type faultConn struct {
	net.Conn
	failRead, failWrite bool
}

func (f faultConn) Read(b []byte) (int, error) {
	if f.failRead {
		return 0, errors.New("read fails")
	}
	return f.Conn.Read(b)
}

func (f faultConn) Write(b []byte) (int, error) {
	if f.failWrite {
		return 0, errors.New("write fails")
	}
	return f.Conn.Write(b)
}

func TestPipeUDPExportFaults(t *testing.T) {
	// The target socket cannot be read: the stream side is closed.
	s1, s2 := net.Pipe()
	u1, u2 := net.Pipe()
	defer s2.Close()
	defer u2.Close()
	done := make(chan struct{})
	go func() { PipeUDPExport(s1, faultConn{Conn: u1, failRead: true}, func(string, int64) {}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("read fault not handled")
	}

	// A datagram cannot be sent to the target.
	s3, s4 := net.Pipe()
	u3, u4 := net.Pipe()
	defer u4.Close()
	done2 := make(chan struct{})
	go func() { PipeUDPExport(s3, faultConn{Conn: u3, failWrite: true}, func(string, int64) {}); close(done2) }()
	_ = wire.WriteDatagram(s4, []byte("x"))
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("write fault not handled")
	}
	s4.Close()
}

func logDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A second channel loses against one dialed by a lower instance ID and is closed at once.
func TestTieBreakLoser(t *testing.T) {
	loc, err := wire.NewLocal(ed25519.NewKeyFromSeed(make([]byte, 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	loc.ID = wire.InstanceID{1}
	m := New(loc, []config.Partner{{Name: "b"}}, echoExporter{}, metrics.New(), logDiscard())
	s := m.state[0]
	p1, p2 := net.Pipe()
	defer p2.Close()
	s.ctrl = &wire.Conn{Conn: tls.Client(p1, &tls.Config{}), Dialer: true} // dialed by us (ID 1)
	newer := &wire.Conn{Conn: tls.Client(p2, &tls.Config{}), PeerID: wire.InstanceID{9}}
	m.setControl(context.Background(), s, newer) // returns at once, keeps the old channel
	if s.ctrl.PeerID == newer.PeerID {
		t.Fatal("loser installed")
	}
}

// Cancelling the context ends the keepalive of a live channel.
func TestPingStopsOnCancel(t *testing.T) {
	old := pingEvery
	pingEvery = time.Hour
	a, b := pair(t, true, false)
	pingEvery = old
	a.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for a.Up(0) || b.Up(0) {
		if time.Now().After(deadline) {
			t.Fatal("channel kept after cancel")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// After a dead spare the fresh dial works but its first write fails too.
func TestRedialWriteFails(t *testing.T) {
	a, _ := pair(t, true, false)
	// Count the writes of one dial alone (no wrapper is shared), then fail the write after it.
	probe := &countConn{}
	a.setWrap(func(c net.Conn) net.Conn { probe.Conn = c; return probe })
	c, err := a.dial(context.Background(), 0, wire.KindStream)
	a.setWrap(nil)
	if err != nil {
		t.Fatal(err)
	}
	n := probe.writes.Load()
	c.Close()
	a.setWrap(func(c net.Conn) net.Conn { return &countConn{Conn: c, failAt: n + 1} })
	sp := takeSpares(t, a)
	a.state[0].mu.Lock()
	ch := a.state[0].spares
	a.state[0].mu.Unlock()
	for _, c := range sp {
		c.Conn.NetConn().Close()
		ch <- c
	}
	if _, _, err := a.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"}); err == nil {
		t.Fatal("open succeeded although every write failed")
	}
}

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

// The target answers but the datagram cannot be framed back to the partner.
func TestPipeUDPExportReplyFails(t *testing.T) {
	s1, s2 := net.Pipe()
	u1, u2 := net.Pipe()
	done := make(chan struct{})
	go func() { PipeUDPExport(faultConn{Conn: s1, failWrite: true}, u1, func(string, int64) {}); close(done) }()
	_, _ = u2.Write([]byte("reply"))
	s2.Close() // ends the partner side
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pipe did not end")
	}
	u2.Close()
}

// setWrap sets the dial wrapper under the partner lock.
func (m *Manager) setWrap(f func(net.Conn) net.Conn) {
	m.state[0].mu.Lock()
	m.wrap = f
	m.state[0].mu.Unlock()
}
