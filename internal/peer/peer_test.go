package peer

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/metrics"
	"github.com/motwok/npm-switchyard/internal/wire"
)

// echoExporter connects every target named "echo" to a local echo server.
type echoExporter struct{ addr string }

func (e echoExporter) Connect(ctx context.Context, t wire.Target) (net.Conn, metrics.Target, byte) {
	if t.Name != "echo" {
		return nil, metrics.Target{}, wire.StatusNoTarget
	}
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		return nil, metrics.Target{}, wire.StatusUnreachable
	}
	return c, metrics.Target{Container: "echo"}, wire.StatusOK
}

func (echoExporter) Track(net.Conn, metrics.Target) func() { return func() {} }

func echoServer(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	return l.Addr().String()
}

func pair(t *testing.T, aDials, bDials bool) (*Manager, *Manager) {
	t.Helper()
	key := bytes.Repeat([]byte{7}, 32)
	la, _ := net.Listen("tcp", "127.0.0.1:0")
	lb, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { la.Close(); lb.Close() })
	pa := config.Partner{Name: "b", Key: key}
	pb := config.Partner{Name: "a", Key: key}
	if aDials {
		pa.Address = lb.Addr().String()
	}
	if bDials {
		pb.Address = la.Addr().String()
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	exp := echoExporter{echoServer(t)}
	loA, _ := wire.NewLocal()
	loB, _ := wire.NewLocal()
	a := New(loA, []config.Partner{pa}, exp, metrics.New(), log)
	b := New(loB, []config.Partner{pb}, exp, metrics.New(), log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Run(ctx, []net.Listener{la})
	b.Run(ctx, []net.Listener{lb})
	deadline := time.Now().Add(10 * time.Second)
	for !(a.Up(0) && b.Up(0)) {
		if time.Now().After(deadline) {
			t.Fatal("control channel did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return a, b
}

func roundTrip(t *testing.T, m *Manager) {
	t.Helper()
	c, st, err := m.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"})
	if err != nil || st != wire.StatusOK {
		t.Fatalf("open: %v status %d", err, st)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 5)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "hello" {
		t.Fatalf("echo: %q %v", b, err)
	}
}

func TestStreamsBothDirections(t *testing.T) {
	a, b := pair(t, true, false)
	roundTrip(t, a) // dialer opens directly
	roundTrip(t, b) // listener requests, dialer attaches
}

func TestRefused(t *testing.T) {
	a, _ := pair(t, true, false)
	c, st, err := a.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if st != wire.StatusNoTarget {
		t.Fatalf("status %d", st)
	}
}

func TestBothDial(t *testing.T) {
	a, b := pair(t, true, true)
	time.Sleep(500 * time.Millisecond)
	if !a.Up(0) || !b.Up(0) {
		t.Fatal("channel lost after tie-break")
	}
	a.state[0].mu.Lock()
	ca := a.dialerID(a.state[0].ctrl)
	a.state[0].mu.Unlock()
	b.state[0].mu.Lock()
	cb := b.dialerID(b.state[0].ctrl)
	b.state[0].mu.Unlock()
	if ca != cb {
		t.Fatal("sides kept different control channels")
	}
	roundTrip(t, a)
	roundTrip(t, b)
}
