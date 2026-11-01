package export

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/motwok/npm-switchyard/internal/docker/dockertest"
	"github.com/motwok/npm-switchyard/internal/wire"
)

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuf) must(t *testing.T, lines ...string) {
	t.Helper()
	out := l.String()
	for _, s := range lines {
		if !strings.Contains(out, s) {
			t.Errorf("log lacks %q:\n%s", s, out)
		}
	}
}

func debugLog(b *logBuf) *slog.Logger {
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestLogNetworksAndMembers(t *testing.T) {
	f, _ := setup(t)
	var lb logBuf
	e := New(f.Client(), "self0000000000", "imp", debugLog(&lb))
	e.Scopes = []Scope{{Peer: "b", Allows: func(n string) bool { return n != "apps" }}}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	lb.must(t, `msg="exported networks" networks=[apps] import_network=imp`,
		`msg="exported networks" peer=b networks=[] excluded=[apps]`,
		`msg="container joined an exported network" container=stack-web-1 network=apps`)
	f.Set(dockertest.Ctr("web00000000000", "stack-web-1", nil, nil)) // left every network
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	lb.must(t, `msg="container left an exported network" container=stack-web-1 network=apps`)
}

func TestLogConnect(t *testing.T) {
	port := target(t)
	_, e := setup(t)
	var lb logBuf
	e.log = debugLog(&lb)
	ctx := wire.WithStream(context.Background(), wire.StreamInfo{ID: "0a0b0c0d", Peer: "b"})
	c, _, st := e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: port, Name: "web"}, all)
	if st != wire.StatusOK {
		t.Fatal(st)
	}
	c.Close()
	lb.must(t, `msg="export request" stream=0a0b0c0d peer=b name=web port=`,
		`msg="export connected" stream=0a0b0c0d peer=b name=web port=`, "container=stack-web-1 network=apps addr=127.0.0.1")
	e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: port, Name: "nope"}, all)
	e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: port, Name: "web"}, func(string) bool { return false })
	lb.must(t, `level=WARN msg="export refused" stream=0a0b0c0d peer=b name=nope`,
		`reason="no container with this name in an exported network"`,
		`reason="networks not allowed for this partner" networks=[apps]`)
}
