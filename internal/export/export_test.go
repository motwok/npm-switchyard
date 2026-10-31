package export

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/docker/dockertest"
	"github.com/motwok/npm-switchyard/internal/wire"
)

// target starts a TCP echo server and returns its port.
func target(t *testing.T) uint16 {
	t.Helper()
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
	return uint16(l.Addr().(*net.TCPAddr).Port)
}

func all(string) bool { return true }

func setup(t *testing.T) (*dockertest.Fake, *Exporter) {
	t.Helper()
	f := dockertest.NewFake()
	t.Cleanup(f.Close)
	f.Set(dockertest.Ctr("self0000000000", "agent", nil, map[string]docker.Endpoint{
		"apps": {NetworkID: "n-apps", IPAddress: "127.0.0.2"},
		"imp":  {NetworkID: "n-imp", IPAddress: "127.0.0.3"},
	}))
	f.Set(dockertest.Ctr("web00000000000", "stack-web-1",
		map[string]string{"com.docker.compose.service": "web", "com.docker.compose.project": "stack"},
		map[string]docker.Endpoint{"apps": {NetworkID: "n-apps", IPAddress: "127.0.0.1", DNSNames: []string{"stack-web-1", "web"}}}))
	f.Set(dockertest.Ctr("npm00000000000", "npm", nil,
		map[string]docker.Endpoint{"imp": {NetworkID: "n-imp", IPAddress: "127.0.0.1", DNSNames: []string{"npm"}}}))
	e := New(f.Client(), "self0000000000", "imp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f, e
}

func TestConnect(t *testing.T) {
	port := target(t)
	_, e := setup(t)
	ctx := context.Background()
	for _, name := range []string{"web", "WEB", "stack-web-1", "web.apps"} {
		c, mt, st := e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: port, Name: name}, all)
		if st != wire.StatusOK {
			t.Fatalf("%s: status %d", name, st)
		}
		if mt.Container != "stack-web-1" || mt.Service != "web" || mt.Stack != "stack" {
			t.Fatalf("labels %+v", mt)
		}
		done := e.Track(c, mt)
		_, _ = c.Write([]byte("hi"))
		b := make([]byte, 2)
		_, _ = io.ReadFull(c, b)
		if string(b) != "hi" {
			t.Fatal("echo")
		}
		_ = c.(interface{ CloseWrite() error }).CloseWrite()
		done()
		c.Close()
	}
}

func TestRefusals(t *testing.T) {
	port := target(t)
	_, e := setup(t)
	ctx := context.Background()
	cases := map[string]struct {
		name  string
		allow func(string) bool
		want  byte
	}{
		"import network": {"npm", all, wire.StatusNoTarget},
		"unknown":        {"nope", all, wire.StatusNoTarget},
		"excluded":       {"web", func(n string) bool { return n != "apps" }, wire.StatusNoTarget},
	}
	for k, c := range cases {
		if _, _, st := e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: port, Name: c.name}, c.allow); st != c.want {
			t.Errorf("%s: status %d", k, st)
		}
	}
	// Port with nothing listening.
	if _, _, st := e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: 1, Name: "web"}, all); st != wire.StatusUnreachable {
		t.Errorf("closed port: status %d", st)
	}
}

func TestReplacedContainer(t *testing.T) {
	port := target(t)
	f, e := setup(t)
	// Same name, but the IP now belongs to another container (re-check before connect).
	c := f.Containers["web00000000000"]
	ep := c.NetworkSettings.Networks["apps"]
	ep.IPAddress = "127.0.0.9"
	c.NetworkSettings.Networks["apps"] = ep
	f.Set(c)
	if _, _, st := e.Connect(context.Background(), wire.Target{Proto: wire.TCP, Port: port, Name: "web"}, all); st != wire.StatusNoTarget {
		t.Fatalf("status %d", st)
	}
}

func TestOptOutClosesStreams(t *testing.T) {
	port := target(t)
	f, e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	c, mt, st := e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: port, Name: "web"}, all)
	if st != wire.StatusOK {
		t.Fatal(st)
	}
	defer e.Track(c, mt)()
	time.Sleep(200 * time.Millisecond)
	f.Remove("web00000000000")
	f.Emit(docker.Event{Type: "container", Action: "die"})
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("stream not closed after opt-out")
	}
	if _, _, st := e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: port, Name: "web"}, all); st != wire.StatusNoTarget {
		t.Fatal("removed container still reachable")
	}
}

func TestUDP(t *testing.T) {
	u, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	_, e := setup(t)
	c, _, st := e.Connect(context.Background(), wire.Target{Proto: wire.UDP, Port: uint16(u.LocalAddr().(*net.UDPAddr).Port), Name: "web"}, all)
	if st != wire.StatusOK {
		t.Fatal(st)
	}
	c.Close()
}

func TestRunStopsOnLostStream(t *testing.T) {
	f, e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	f.CloseEvents() // stream ends; Run retries with backoff
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// Older daemons without DNSNames: container name, aliases and short ID still match.
func TestFallbackNames(t *testing.T) {
	port := target(t)
	f, e := setup(t)
	f.Set(dockertest.Ctr("old0123456789ab", "legacy", nil,
		map[string]docker.Endpoint{"apps": {NetworkID: "n-apps", IPAddress: "127.0.0.1", Aliases: []string{"alias1"}}}))
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"legacy", "alias1", "old012345678"} {
		c, _, st := e.Connect(context.Background(), wire.Target{Proto: wire.TCP, Port: port, Name: n}, all)
		if st != wire.StatusOK {
			t.Fatalf("%s: %d", n, st)
		}
		c.Close()
	}
}
