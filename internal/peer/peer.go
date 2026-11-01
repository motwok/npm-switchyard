// Package peer keeps control channels, spare connections and streams to partners (spec 5, 11).
package peer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/metrics"
	"github.com/motwok/npm-switchyard/internal/wire"
)

const (
	deadAfter   = 30 * time.Second
	backoffMin  = time.Second
	backoffMax  = 30 * time.Second
	dialTimeout = 10 * time.Second
)

// requestWait bounds how long a listener waits for the dialer to attach a stream.
var requestWait = 15 * time.Second

// pingEvery is the keepalive interval on the control channel (PAR-5).
var pingEvery = 10 * time.Second

// acceptRetry is the pause after a failed accept on the partner port.
var acceptRetry = time.Second

// statusTimeout bounds the wait for the exporting side's stream status.
var statusTimeout = 15 * time.Second

var ErrNotConnected = errors.New("partner not connected")

// Exporter resolves and connects a stream target on this host (spec 10).
type Exporter interface {
	Connect(ctx context.Context, t wire.Target, allow func(network string) bool) (net.Conn, metrics.Target, byte)
	Track(c net.Conn, mt metrics.Target) func()
}

type Manager struct {
	local    *wire.Local
	partners []config.Partner
	exp      Exporter
	met      *metrics.Registry
	log      *slog.Logger

	// wrap, if set, wraps every new raw partner connection (tests inject faults with it);
	// guarded by the partner state lock.
	wrap func(net.Conn) net.Conn

	// Copied from the package defaults at New, so tests can shorten them.
	requestWait, statusTimeout, pingEvery, acceptRetry time.Duration

	mu    sync.Mutex
	state []*partnerState
}

type partnerState struct {
	idx  int
	name string
	addr string // dial target; guarded by mu

	mu      sync.Mutex
	ctrl    *wire.Conn
	ctrlW   sync.Mutex
	spares  chan *wire.Conn
	pending map[uint32]chan *wire.Conn
	nextID  uint32
	gen     int
}

func New(local *wire.Local, partners []config.Partner, exp Exporter, met *metrics.Registry, log *slog.Logger) *Manager {
	m := &Manager{local: local, partners: partners, exp: exp, met: met, log: log,
		requestWait: requestWait, statusTimeout: statusTimeout, pingEvery: pingEvery, acceptRetry: acceptRetry}
	for i, p := range partners {
		m.state = append(m.state, &partnerState{idx: i, name: p.Name, addr: p.Address, pending: map[uint32]chan *wire.Conn{}})
		met.SetPeerUp(p.Name, false)
	}
	return m
}

// Index returns a partner's index by name.
func (m *Manager) Index(name string) (int, bool) {
	for i, p := range m.partners {
		if p.Name == name {
			return i, true
		}
	}
	return 0, false
}

func (m *Manager) Up(idx int) bool {
	s := m.state[idx]
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctrl != nil
}

// Run starts dialers and the accept loops on the given listeners.
func (m *Manager) Run(ctx context.Context, listeners []net.Listener) {
	for _, l := range listeners {
		go m.acceptLoop(ctx, l)
	}
	for i, p := range m.partners {
		if p.Address != "" {
			go m.dialLoop(ctx, i)
		}
	}
}

func (m *Manager) acceptLoop(ctx context.Context, l net.Listener) {
	for {
		raw, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.Warn("accept", "err", err)
			time.Sleep(m.acceptRetry)
			continue
		}
		go m.handleIncoming(ctx, raw)
	}
}

func (m *Manager) handleIncoming(ctx context.Context, raw net.Conn) {
	c, err := m.local.Accept(raw)
	if err != nil {
		m.logHandshakeFailed(raw.RemoteAddr(), err)
		raw.Close()
		return
	}
	s := m.state[c.Partner]
	switch c.Kind {
	case wire.KindControl:
		m.setControl(ctx, s, c)
	case wire.KindStream:
		m.serveStream(ctx, s, c)
	default:
		c.Close()
	}
}

func (m *Manager) dialLoop(ctx context.Context, idx int) {
	s := m.state[idx]
	p := m.partners[idx]
	wait := backoffMin
	for ctx.Err() == nil {
		s.mu.Lock()
		up := s.ctrl != nil
		s.mu.Unlock()
		if up {
			time.Sleep(time.Second)
			continue
		}
		c, err := m.dial(ctx, idx, wire.KindControl)
		if err != nil {
			m.logDialFailed(p.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(wait*2, backoffMax)
			continue
		}
		wait = backoffMin
		m.setControl(ctx, s, c)
	}
}

// Handshake failures are warnings, except a version mismatch (PAR-3) and failures after the
// partner was identified (LOG-2), which are errors with the partner's name.
func (m *Manager) logDialFailed(name string, err error) {
	lvl := slog.LevelWarn
	if errors.Is(err, wire.ErrNoVersion) {
		lvl = slog.LevelError
	}
	m.log.Log(context.Background(), lvl, "partner dial failed", "peer", name, "err", err)
}

func (m *Manager) logHandshakeFailed(remote net.Addr, err error) {
	var pe *wire.PeerError
	if errors.As(err, &pe) {
		m.log.Error("partner handshake failed", "peer", m.partners[pe.Partner].Name, "remote", remote, "err", err)
		return
	}
	m.log.Warn("partner handshake failed", "remote", remote, "err", err)
}

func (m *Manager) dial(ctx context.Context, idx int, kind byte) (*wire.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout}
	s := m.state[idx]
	s.mu.Lock()
	addr, wrap := s.addr, m.wrap
	s.mu.Unlock()
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if wrap != nil {
		raw = wrap(raw)
	}
	c, err := m.local.Dial(raw, idx, kind)
	if err != nil {
		raw.Close()
		return nil, err
	}
	c.Partner = idx
	return c, nil
}

// dialerID is the instance ID of the side that dialed c.
func (m *Manager) dialerID(c *wire.Conn) wire.InstanceID {
	if c.Dialer {
		return m.local.ID
	}
	return c.PeerID
}

// setControl installs a control channel; with two channels the one dialed by the lower
// instance ID wins (PAR-4). A new instance ID of the partner means it restarted: the old
// channel is dead, whatever its dialer ID.
func (m *Manager) setControl(ctx context.Context, s *partnerState, c *wire.Conn) {
	s.mu.Lock()
	if old := s.ctrl; old != nil {
		if old.PeerID == c.PeerID && m.dialerID(old).Less(m.dialerID(c)) {
			s.mu.Unlock()
			c.Close()
			return
		}
		old.Close()
		closeSpares(s)
	}
	s.ctrl = c
	s.gen++
	gen := s.gen
	if c.Dialer {
		s.spares = make(chan *wire.Conn, max(m.partners[s.idx].SpareCount(), 1))
	} else {
		s.spares = nil
	}
	s.mu.Unlock()

	m.met.SetPeerUp(s.name, true)
	m.log.Info("partner connected", "peer", s.name, "dialer", c.Dialer, "version", c.Version)
	if c.Dialer {
		go m.keepSpares(ctx, s, gen)
	}
	last, err := m.controlLoop(ctx, s, c)

	s.mu.Lock()
	if s.ctrl == c {
		s.ctrl = nil
		s.gen++
		closeSpares(s)
		s.mu.Unlock()
		m.met.SetPeerUp(s.name, false)
		m.log.Warn("partner disconnected", "peer", s.name, "err", err,
			"silent_for", time.Since(last).Round(time.Millisecond))
		return
	}
	s.mu.Unlock()
}

// closeSpares drops the spare pool with its idle connections. Caller holds s.mu.
func closeSpares(s *partnerState) {
	if s.spares == nil {
		return
	}
	close(s.spares)
	for sp := range s.spares {
		sp.Close()
	}
	s.spares = nil
}

// controlLoop serves the control channel until it fails; it returns when the partner was last
// heard and the error.
func (m *Manager) controlLoop(ctx context.Context, s *partnerState, c *wire.Conn) (time.Time, error) {
	defer c.Close()
	last := time.Now()
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(m.pingEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				c.Close()
				return
			case <-t.C:
				if m.writeCtrl(s, c, wire.MsgPing, nil) != nil {
					c.Close()
					return
				}
			}
		}
	}()
	for {
		_ = c.SetReadDeadline(time.Now().Add(deadAfter))
		typ, p, err := wire.ReadMsg(c)
		if err != nil {
			return last, err
		}
		last = time.Now()
		switch typ {
		case wire.MsgPing:
			_ = m.writeCtrl(s, c, wire.MsgPong, nil)
		case wire.MsgPong:
		case wire.MsgRequest:
			id, t, err := wire.UnmarshalRequest(p)
			if err == nil && c.Dialer {
				m.log.Debug("stream request received", "peer", s.name, "request", id, "name", t.Name, "port", t.Port)
				go m.attach(ctx, s, id, t)
			}
		}
	}
}

func (m *Manager) writeCtrl(s *partnerState, c *wire.Conn, typ byte, p []byte) error {
	s.ctrlW.Lock()
	defer s.ctrlW.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(deadAfter))
	return wire.WriteMsg(c, typ, p)
}

func (m *Manager) keepSpares(ctx context.Context, s *partnerState, gen int) {
	want := m.partners[s.idx].SpareCount()
	for ctx.Err() == nil {
		s.mu.Lock()
		if s.gen != gen {
			s.mu.Unlock()
			return
		}
		ch := s.spares
		s.mu.Unlock()
		if want == 0 || len(ch) >= want {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		c, err := m.dial(ctx, s.idx, wire.KindStream)
		if err != nil {
			m.log.Debug("spare connection failed", "peer", s.name, "err", err)
			time.Sleep(time.Second)
			continue
		}
		s.mu.Lock()
		if s.gen != gen {
			s.mu.Unlock()
			c.Close()
			return
		}
		s.spares <- c // never blocks: only this goroutine fills it, and it was below want
		n := len(s.spares)
		s.mu.Unlock()
		m.log.Debug("spare connection ready", "peer", s.name, "spares", n)
	}
}

// streamConn takes a spare or dials a new stream connection (TRN-1, PAR-7).
func (m *Manager) streamConn(ctx context.Context, s *partnerState) (*wire.Conn, error) {
	s.mu.Lock()
	ch := s.spares
	s.mu.Unlock()
	for ch != nil {
		select {
		case c, ok := <-ch:
			if !ok {
				ch = nil
				continue
			}
			m.log.Debug("spare connection taken", "peer", s.name)
			return c, nil
		default:
			ch = nil
		}
	}
	m.log.Debug("no spare connection, dialing", "peer", s.name)
	return m.dial(ctx, s.idx, wire.KindStream)
}

// Open opens a stream to target on partner idx. It returns the stream and the exporter's status.
func (m *Manager) Open(ctx context.Context, idx int, t wire.Target) (net.Conn, byte, error) {
	s := m.state[idx]
	s.mu.Lock()
	ctrl := s.ctrl
	s.mu.Unlock()
	if ctrl == nil {
		return nil, 0, ErrNotConnected
	}
	if ctrl.Dialer {
		hdr := append([]byte{wire.StreamOpen}, framed(t.Marshal())...)
		c, err := m.streamConn(ctx, s)
		if err != nil {
			return nil, 0, err
		}
		if _, err := c.Write(hdr); err != nil {
			// A spare may have died meanwhile; one fresh dial.
			c.Close()
			if c, err = m.dial(ctx, s.idx, wire.KindStream); err != nil {
				return nil, 0, err
			}
			if _, err := c.Write(hdr); err != nil {
				c.Close()
				return nil, 0, err
			}
		}
		st, err := readStatus(c, m.statusTimeout)
		if err != nil {
			c.Close()
			return nil, 0, err
		}
		return c, st, nil
	}

	ch := make(chan *wire.Conn, 1)
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.pending[id] = ch
	s.mu.Unlock()
	defer s.forget(id, ch)
	if err := m.writeCtrl(s, ctrl, wire.MsgRequest, wire.MarshalRequest(id, t)); err != nil {
		return nil, 0, err
	}
	m.log.Debug("stream request sent", "peer", s.name, "request", id, "name", t.Name, "port", t.Port)
	select {
	case c := <-ch:
		st, err := readStatus(c, m.statusTimeout)
		if err != nil {
			c.Close()
			return nil, 0, err
		}
		return c, st, nil
	case <-time.After(m.requestWait):
		return nil, 0, errors.New("partner did not attach")
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}

// forget ends a request: a stream attached after the requester stopped waiting is closed.
func (s *partnerState) forget(id uint32, ch chan *wire.Conn) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
	select {
	case c := <-ch:
		c.Close()
	default:
	}
}

// attach serves a listener's request: export the target and hand the stream over (PAR-6).
func (m *Manager) attach(ctx context.Context, s *partnerState, id uint32, t wire.Target) {
	c, err := m.streamConn(ctx, s)
	if err != nil {
		m.log.Warn("attach failed", "peer", s.name, "err", err)
		return
	}
	hdr := binary.BigEndian.AppendUint32([]byte{wire.StreamAttach}, id)
	if _, err := c.Write(hdr); err != nil {
		c.Close()
		return
	}
	m.export(ctx, s, c, t)
}

// serveStream handles a stream connection dialed by the partner.
func (m *Manager) serveStream(ctx context.Context, s *partnerState, c *wire.Conn) {
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		c.Close()
		return
	}
	switch b[0] {
	case wire.StreamOpen:
		p, err := readFramed(c)
		if err != nil {
			c.Close()
			return
		}
		t, err := wire.UnmarshalTarget(p)
		if err != nil {
			c.Close()
			return
		}
		m.export(ctx, s, c, t)
	case wire.StreamAttach:
		var idb [4]byte
		if _, err := io.ReadFull(c, idb[:]); err != nil {
			c.Close()
			return
		}
		id := binary.BigEndian.Uint32(idb[:])
		// Lookup and hand-over under the lock, so forget sees the stream or it is closed here.
		s.mu.Lock()
		delivered := false
		if ch := s.pending[id]; ch != nil {
			select {
			case ch <- c:
				delivered = true
			default:
			}
		}
		s.mu.Unlock()
		if !delivered {
			c.Close()
		}
	default:
		c.Close()
	}
}

// export connects the local target and pipes it to the partner stream c.
func (m *Manager) export(ctx context.Context, s *partnerState, c *wire.Conn, t wire.Target) {
	id := c.StreamID()
	ctx = wire.WithStream(ctx, wire.StreamInfo{ID: id, Peer: s.name})
	target, mt, st := m.exp.Connect(ctx, t, m.partners[s.idx].Networks.Allows)
	_ = c.SetWriteDeadline(time.Now().Add(m.statusTimeout))
	if _, err := c.Write([]byte{st}); err != nil || st != wire.StatusOK {
		result := "refused"
		if err != nil {
			result = "error"
		}
		m.met.StreamResult(s.name, result, mt)
		if target != nil {
			target.Close()
		}
		c.Close()
		return
	}
	_ = c.SetWriteDeadline(time.Time{})
	m.met.StreamResult(s.name, "ok", mt)
	m.met.StreamStarted(s.name, mt)
	defer m.met.StreamEnded(s.name, mt)
	untrack := m.exp.Track(target, mt)
	defer untrack()
	var bytes Bytes
	count := func(dir string, n int64) {
		m.met.AddBytes(s.name, dir, mt, n)
		bytes.Add(dir, n)
	}
	start := time.Now()
	if t.Proto == wire.UDP {
		PipeUDPExport(c, target, count)
	} else {
		Pipe(c, target, count)
	}
	m.log.Debug("export closed", "stream", id, "peer", s.name, "name", t.Name, "port", t.Port,
		"duration", time.Since(start).Round(time.Millisecond), "bytes_in", bytes.In.Load(), "bytes_out", bytes.Out.Load())
}

// Bytes sums a stream's traffic per direction for its closing log line.
type Bytes struct{ In, Out atomic.Int64 }

func (b *Bytes) Add(dir string, n int64) {
	if dir == "in" {
		b.In.Add(n)
	} else {
		b.Out.Add(n)
	}
}

func framed(p []byte) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(p))), p...)
}

func readFramed(r io.Reader) ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	p := make([]byte, binary.BigEndian.Uint16(h[:]))
	_, err := io.ReadFull(r, p)
	return p, err
}

func readStatus(c *wire.Conn, timeout time.Duration) (byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	defer c.SetReadDeadline(time.Time{})
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return 0, fmt.Errorf("stream status: %w", err)
	}
	return b[0], nil
}
