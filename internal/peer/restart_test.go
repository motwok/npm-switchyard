package peer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/metrics"
	"github.com/motwok/npm-switchyard/internal/wire"
)

// A restarted partner (new instance ID) replaces the stale channel even when the plain
// tie-break would keep it.
func TestRestartedPartnerReplacesStaleChannel(t *testing.T) {
	pairSetup = func(a, _ *Manager) { a.local.ID = wire.InstanceID{1} }
	a, b := pair(t, true, false)
	pairSetup = nil
	a.setAddr(0, "127.0.0.1:1") // the old instance does not come back

	ka := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	lo, err := wire.NewLocal(ka, []wire.Partner{{Own: ka, Peer: a.partners[0].Peer}})
	if err != nil {
		t.Fatal(err)
	}
	lo.ID = wire.InstanceID{2} // higher than the stale channel's dialer: would lose the tie-break
	a2 := New(lo, a.partners, a.exp, metrics.New(), logDiscard())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a2.Run(ctx, nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		b.state[0].mu.Lock()
		c := b.state[0].ctrl
		b.state[0].mu.Unlock()
		if c != nil && c.PeerID == lo.ID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new instance not installed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	roundTrip(t, b)
}

// The spares of a replaced channel are closed with it.
func TestReplacedChannelClosesSpares(t *testing.T) {
	loc, err := wire.NewLocal(ed25519.NewKeyFromSeed(make([]byte, 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	loc.ID = wire.InstanceID{5}
	m := New(loc, []config.Partner{{Name: "b"}}, echoExporter{}, metrics.New(), logDiscard())
	s := m.state[0]
	p1, p2 := net.Pipe()
	defer p2.Close()
	q1, q2 := net.Pipe()
	defer q2.Close()
	peerID := wire.InstanceID{1}
	s.ctrl = &wire.Conn{Conn: tls.Client(p1, &tls.Config{}), Dialer: true, PeerID: peerID} // dialed by us, ID 5
	s.spares = make(chan *wire.Conn, 2)
	s.spares <- &wire.Conn{Conn: tls.Client(q1, &tls.Config{})}
	// Same instance, dialed by it with ID 1 < 5: it wins and the old channel goes.
	winner := &wire.Conn{Conn: tls.Client(p2, &tls.Config{}), PeerID: peerID}
	m.setControl(context.Background(), s, winner) // the old pipe is closed, the new handshake fails, returns
	_ = q2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := q2.Read(make([]byte, 1)); err == nil {
		t.Fatal("spare of the replaced channel still open")
	}
}

// keepSpares stops once its channel generation is over: before it looks, or while a dial
// was in flight (the fresh connection is closed).
func TestKeepSparesStops(t *testing.T) {
	a, _ := pair(t, true, false)
	s := a.state[0]
	s.mu.Lock()
	g := s.gen
	s.mu.Unlock()
	a.keepSpares(context.Background(), s, g+1) // not its generation: returns at once

	s2 := &partnerState{idx: 0, name: "b", spares: make(chan *wire.Conn, 1), pending: map[uint32]chan *wire.Conn{}}
	a.setWrap(func(c net.Conn) net.Conn { // the generation moves on during the dial
		s2.mu.Lock()
		s2.gen++
		s2.mu.Unlock()
		return c
	})
	a.keepSpares(context.Background(), s2, 0)
	a.setWrap(nil)
	if len(s2.spares) != 0 {
		t.Fatal("stale generation filled the pool")
	}
}

// A stream attached after the requester gave up is closed by forget.
func TestForgetClosesLateAttach(t *testing.T) {
	s := &partnerState{pending: map[uint32]chan *wire.Conn{}}
	ch := make(chan *wire.Conn, 1)
	s.pending[7] = ch
	p1, p2 := net.Pipe()
	defer p2.Close()
	ch <- &wire.Conn{Conn: tls.Client(p1, &tls.Config{})}
	s.forget(7, ch)
	if _, ok := s.pending[7]; ok {
		t.Fatal("request not forgotten")
	}
	_ = p2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := p2.Read(make([]byte, 1)); err == nil {
		t.Fatal("late stream not closed")
	}
	s.forget(7, ch) // nothing left: no-op
}

// An attach that finds no waiting request is closed, even when the slot existed a moment ago.
func TestAttachAfterForget(t *testing.T) {
	a, b := pair(t, true, false)
	ch := make(chan *wire.Conn, 1)
	b.state[0].mu.Lock()
	b.state[0].pending[42] = ch
	b.state[0].mu.Unlock()
	b.state[0].forget(42, ch)
	c, err := a.dial(context.Background(), 0, wire.KindStream)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte{wire.StreamAttach, 0, 0, 0, 42})
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("attach without requester kept open")
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Handshake failures of an identified partner and version mismatches are errors with the
// partner's name; anything else is a warning without one (LOG-2, PAR-3).
func TestHandshakeLogs(t *testing.T) {
	buf := &syncBuf{}
	m := New(nil, []config.Partner{{Name: "hostb"}}, echoExporter{}, metrics.New(),
		slog.New(slog.NewTextHandler(buf, nil)))
	remote := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5}
	m.logHandshakeFailed(remote, errors.New("not a switchyard peer"))
	m.logHandshakeFailed(remote, &wire.PeerError{Partner: 0, Err: errors.New("partner expected a different own key")})
	m.logDialFailed("hostb", errors.New("connection refused"))
	m.logDialFailed("hostb", fmt.Errorf("%w: ours 1-1, theirs 2-2", wire.ErrNoVersion))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatal(buf.String())
	}
	want := []string{"level=WARN", "level=ERROR", "level=WARN", "level=ERROR"}
	for i, l := range lines {
		if !strings.Contains(l, want[i]) {
			t.Fatalf("line %d: %s", i, l)
		}
		if i != 0 && !strings.Contains(l, "peer=hostb") {
			t.Fatalf("line %d without peer: %s", i, l)
		}
	}
	if strings.Contains(lines[0], "peer=") {
		t.Fatal(lines[0])
	}
}
