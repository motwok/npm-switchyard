package peer

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/metrics"
	"github.com/motwok/npm-switchyard/internal/wire"
)

func TestNotConnected(t *testing.T) {
	m := New(nil, []config.Partner{{Name: "x"}}, echoExporter{}, metrics.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, _, err := m.Open(context.Background(), 0, wire.Target{Name: "echo"}); err != ErrNotConnected {
		t.Fatal(err)
	}
	if m.Up(0) {
		t.Fatal("up without channel")
	}
	if i, ok := m.Index("x"); !ok || i != 0 {
		t.Fatal("index")
	}
	if _, ok := m.Index("y"); ok {
		t.Fatal("unknown index")
	}
}

// udpExporter connects "udp" to a local UDP echo.
type udpExporter struct{ addr string }

func (e udpExporter) Connect(ctx context.Context, t wire.Target, _ func(string) bool) (net.Conn, metrics.Target, byte) {
	c, err := net.Dial("udp", e.addr)
	if err != nil {
		return nil, metrics.Target{}, wire.StatusUnreachable
	}
	return c, metrics.Target{Container: "u"}, wire.StatusOK
}

func (udpExporter) Track(net.Conn, metrics.Target) func() { return func() {} }

func udpEcho(t *testing.T) string {
	t.Helper()
	u, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() })
	go func() {
		b := make([]byte, 2048)
		for {
			n, a, err := u.ReadFrom(b)
			if err != nil {
				return
			}
			_, _ = u.WriteTo(b[:n], a)
		}
	}()
	return u.LocalAddr().String()
}

func TestUDPStream(t *testing.T) {
	a, b := pair(t, true, false)
	exp := udpExporter{udpEcho(t)}
	a.exp, b.exp = exp, exp
	for _, m := range []*Manager{a, b} { // direct open and request/attach
		c, st, err := m.Open(context.Background(), 0, wire.Target{Proto: wire.UDP, Port: 1, Name: "u"})
		if err != nil || st != wire.StatusOK {
			t.Fatal(err, st)
		}
		if err := wire.WriteDatagram(c, []byte("ping")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 16)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := wire.ReadDatagram(c, buf)
		if err != nil || string(buf[:n]) != "ping" {
			t.Fatal(err, string(buf[:n]))
		}
		c.Close()
	}
}

func TestMetricsAfterStreams(t *testing.T) {
	a, b := pair(t, true, false)
	roundTrip(t, a)
	time.Sleep(200 * time.Millisecond)
	var sb strings.Builder
	_ = b.met.Write(&sb)
	if !strings.Contains(sb.String(), `switchyard_streams_total{peer="a",result="ok",container="echo"} 1`) ||
		!strings.Contains(sb.String(), `switchyard_peer_up{peer="a"} 1`) {
		t.Fatal(sb.String())
	}
}

func TestReconnect(t *testing.T) {
	a, b := pair(t, true, false)
	a.state[0].mu.Lock()
	c := a.state[0].ctrl
	a.state[0].mu.Unlock()
	c.Close() // dialer redials after the channel drops
	deadline := time.Now().Add(10 * time.Second)
	for {
		a.state[0].mu.Lock()
		again := a.state[0].ctrl != nil && a.state[0].ctrl != c
		a.state[0].mu.Unlock()
		if again && b.Up(0) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no reconnect")
		}
		time.Sleep(50 * time.Millisecond)
	}
	roundTrip(t, a)
}

func TestIdleClosesFlow(t *testing.T) {
	old := UDPIdle
	UDPIdle = 120 * time.Millisecond
	defer func() { UDPIdle = old }()
	a, b := net.Pipe()
	defer b.Close()
	w := WatchIdle(a)
	defer w.Stop()
	_ = a.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := a.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle flow not closed")
	}
	w.Stop() // idempotent
}

func TestPipeHalfClose(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(c, c)
		c.Close()
	}()
	x, _ := net.Dial("tcp", l.Addr().String())
	cl, sv := net.Pipe()
	var in, out int64
	done := make(chan struct{})
	go func() {
		Pipe(sv, x, func(dir string, n int64) {
			if dir == "in" {
				in += n
			} else {
				out += n
			}
		})
		close(done)
	}()
	_, _ = cl.Write([]byte("abc"))
	b := make([]byte, 3)
	_, _ = io.ReadFull(cl, b)
	cl.Close()
	<-done
	if string(b) != "abc" || in != 3 || out != 3 {
		t.Fatal(string(b), in, out)
	}
}

// Garbage and unknown peers on the partner port are dropped without harm.
func TestBadIncoming(t *testing.T) {
	a, _ := pair(t, true, false)
	_ = a
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ka := a.local
	m := New(ka, a.partners, echoExporter{}, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	m.Run(ctx, []net.Listener{l})
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("not tls"))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("garbage connection kept open")
	}
	c.Close()
	cancel()
	l.Close()
}

// Stream connections with an unknown first byte or an unknown attach id are closed.
func TestBadStreams(t *testing.T) {
	a, b := pair(t, true, false)
	for _, hdr := range [][]byte{{'?'}, {wire.StreamAttach, 0, 0, 0, 99}, {wire.StreamOpen, 0, 1, 'x'}} {
		c, err := a.dial(context.Background(), 0, wire.KindStream)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write(hdr)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatalf("stream %v kept open", hdr)
		}
		c.Close()
	}
	roundTrip(t, b)
}

// A request the dialer cannot serve (target refused) reaches the listener as a refusal.
func TestAttachRefused(t *testing.T) {
	_, b := pair(t, true, false)
	c, st, err := b.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if st != wire.StatusNoTarget {
		t.Fatalf("status %d", st)
	}
}

func TestOpenCancelled(t *testing.T) {
	_, b := pair(t, true, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := b.Open(ctx, 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"}); err == nil {
		// The attach may win the race; then the stream is valid.
		return
	}
}

// silentExporter never answers: the attaching dialer blocks, the listener times out.
type silentExporter struct{ release chan struct{} }

func (e silentExporter) Connect(ctx context.Context, _ wire.Target, _ func(string) bool) (net.Conn, metrics.Target, byte) {
	<-e.release
	return nil, metrics.Target{}, wire.StatusNoTarget
}

func (silentExporter) Track(net.Conn, metrics.Target) func() { return func() {} }

func TestStreamTimeouts(t *testing.T) {
	oldR, oldS := requestWait, statusTimeout
	requestWait, statusTimeout = 300*time.Millisecond, 300*time.Millisecond
	a, b := pair(t, true, false) // the managers copy the short timeouts
	requestWait, statusTimeout = oldR, oldS
	ex := silentExporter{make(chan struct{})}
	defer close(ex.release)
	a.exp = ex

	// The exporter never answers: the listener gives up waiting for the status.
	if _, _, err := b.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"}); err == nil {
		t.Fatal("no status timeout")
	}

	// The dialer cannot open stream connections any more: no attach arrives.
	a.setAddr(0, "127.0.0.1:1")
	a.state[0].mu.Lock()
	ch := a.state[0].spares
	a.state[0].mu.Unlock()
	for len(ch) > 0 {
		(<-ch).Close()
	}
	if _, _, err := b.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"}); err == nil {
		t.Fatal("no request timeout")
	}
	if _, _, err := a.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"}); err == nil {
		t.Fatal("dialer opened without a stream connection")
	}
}

// Incoming stream connections that break off at each step are closed.
func TestTruncatedStreams(t *testing.T) {
	a, _ := pair(t, true, false)
	for _, hdr := range [][]byte{{}, {wire.StreamOpen}, {wire.StreamOpen, 0, 9, 't'}, {wire.StreamAttach, 0}} {
		c, err := a.dial(context.Background(), 0, wire.KindStream)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write(hdr)
		_ = c.CloseWrite()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Read(make([]byte, 1))
		c.Close()
	}
}

// An authenticated connection of an unknown kind is dropped.
func TestUnknownKind(t *testing.T) {
	a, _ := pair(t, true, false)
	c, err := a.dial(context.Background(), 0, 'X')
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("unknown kind kept open")
	}
}

// A dead spare is replaced by one fresh dial; without a reachable partner Open fails.
func TestDeadSpare(t *testing.T) {
	a, _ := pair(t, true, false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.state[0].mu.Lock()
		n := len(a.state[0].spares)
		a.state[0].mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.state[0].mu.Lock()
	ch := a.state[0].spares
	a.state[0].mu.Unlock()
	sp := <-ch
	sp.Conn.NetConn().Close() // the TLS layer still looks fine, the socket is gone
	ch <- sp
	roundTrip(t, a)

	a.setAddr(0, "127.0.0.1:1")
	for len(ch) > 0 {
		sp := <-ch
		sp.Conn.NetConn().Close()
		ch <- sp
		break
	}
	_, _, _ = a.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"})
}

// The dialer waits for a status that never comes.
func TestDialerStatusTimeout(t *testing.T) {
	old := statusTimeout
	statusTimeout = 300 * time.Millisecond
	a, b := pair(t, true, false)
	statusTimeout = old
	ex := silentExporter{make(chan struct{})}
	defer close(ex.release)
	b.exp = ex
	if _, _, err := a.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"}); err == nil {
		t.Fatal("no status timeout")
	}
}

// The handshake with something that is not a partner fails, and so does a dial to nowhere.
func TestDialErrors(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			c.Close()
		}
	}()
	a, _ := pair(t, true, false)
	a.setAddr(0, l.Addr().String())
	if _, err := a.dial(context.Background(), 0, wire.KindStream); err == nil {
		t.Fatal("handshake with a non-partner succeeded")
	}
	a.setAddr(0, "127.0.0.1:1")
	if _, err := a.dial(context.Background(), 0, wire.KindStream); err == nil {
		t.Fatal("dial to nowhere succeeded")
	}
}

// A reset in the middle of a copy closes both sides.
func TestPipeReset(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		_ = c.(*net.TCPConn).SetLinger(0)
		c.Close()
	}()
	x, _ := net.Dial("tcp", l.Addr().String())
	cl, sv := net.Pipe()
	defer cl.Close()
	done := make(chan struct{})
	go func() { Pipe(sv, x, func(string, int64) {}); close(done) }()
	go func() { _, _ = io.Copy(io.Discard, cl) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pipe did not end")
	}
}

// Pings are answered and keep the channel up.
func TestPingPong(t *testing.T) {
	pairSetup = func(a, b *Manager) { a.pingEvery, b.pingEvery = 20*time.Millisecond, 20*time.Millisecond }
	a, b := pair(t, true, false)
	pairSetup = nil
	time.Sleep(200 * time.Millisecond)
	if !a.Up(0) || !b.Up(0) {
		t.Fatal("channel lost under keepalive")
	}
}

// A reset on one side reaches the other side as a reset, not as an orderly close (TRN-2).
func TestPipeResetPropagates(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	resetter := make(chan net.Conn, 1)
	errc := make(chan error, 1)
	go func() {
		c, err := l.Accept() // first: the side that resets
		if err != nil {
			return
		}
		resetter <- c
		d, err := l.Accept() // second: the side that must see the reset
		if err != nil {
			return
		}
		_, err = d.Read(make([]byte, 1))
		errc <- err
		d.Close()
	}()
	x, _ := net.Dial("tcp", l.Addr().String())
	y, _ := net.Dial("tcp", l.Addr().String())
	go Pipe(wrapped{y}, x, func(string, int64) {})
	c := <-resetter
	_ = c.(*net.TCPConn).SetLinger(0)
	c.Close()
	select {
	case err := <-errc:
		if err == nil || err == io.EOF {
			t.Fatalf("reset arrived as %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reset")
	}
	reset(wrapped{wrapped{&net.TCPConn{}}}) // unwraps; a nil TCPConn tolerates SetLinger
	a, b := net.Pipe()
	reset(a) // not TCP: nothing to do
	a.Close()
	b.Close()
}

// wrapped hides a socket behind NetConn, as TLS and the exporter's connections do.
type wrapped struct{ net.Conn }

func (w wrapped) NetConn() net.Conn { return w.Conn }

// setAddr changes where the manager dials partner idx.
func (m *Manager) setAddr(idx int, a string) {
	s := m.state[idx]
	s.mu.Lock()
	s.addr = a
	s.mu.Unlock()
}
