package peer

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

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

// waitFor waits until the log holds every line.
func (l *logBuf) waitFor(t *testing.T, lines ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, missing := l.String(), ""
		for _, s := range lines {
			if !strings.Contains(out, s) {
				missing = s
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log lacks %q:\n%s", missing, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// logPair is a pair where a only listens, so its streams go through a request (PAR-6).
func logPair(t *testing.T) (a *Manager, la, lb *logBuf) {
	t.Helper()
	la, lb = &logBuf{}, &logBuf{}
	pairSetup = func(x, y *Manager) {
		x.log = slog.New(slog.NewTextHandler(la, &slog.HandlerOptions{Level: slog.LevelDebug}))
		y.log = slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	defer func() { pairSetup = nil }()
	a, _ = pair(t, false, true)
	return a, la, lb
}

func TestLogStreamIDBothSides(t *testing.T) {
	a, la, lb := logPair(t)
	c, st, err := a.Open(context.Background(), 0, wire.Target{Proto: wire.TCP, Port: 1, Name: "echo"})
	if err != nil || st != wire.StatusOK {
		t.Fatal(err, st)
	}
	id := c.(*wire.Conn).StreamID()
	_, _ = c.Write([]byte("hi"))
	b := make([]byte, 2)
	_, _ = c.Read(b)
	c.Close()
	la.waitFor(t, `msg="stream request sent" peer=b request=1 name=echo port=1`)
	lb.waitFor(t, `msg="stream request received" peer=a request=1 name=echo port=1`,
		`msg="export closed" stream=`+id+` peer=a name=echo port=1`, "bytes_in=2 bytes_out=2")
}

func TestLogDisconnect(t *testing.T) {
	a, la, _ := logPair(t)
	a.cancel()
	la.waitFor(t, `msg="partner disconnected" peer=b err=`, "silent_for=")
}

func TestBytes(t *testing.T) {
	var b Bytes
	b.Add("in", 3)
	b.Add("out", 5)
	if b.In.Load() != 3 || b.Out.Load() != 5 {
		t.Fatal(b.In.Load(), b.Out.Load())
	}
}
