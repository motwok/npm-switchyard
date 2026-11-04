package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeTimer replaces afterFunc; fire runs the pending callback.
type fakeTimer struct {
	f       func()
	stopped int
}

func (ft *fakeTimer) install(t *testing.T) {
	old := afterFunc
	afterFunc = func(_ time.Duration, f func()) func() bool {
		ft.f = f
		return func() bool { ft.stopped++; return true }
	}
	t.Cleanup(func() { afterFunc = old })
}

func (ft *fakeTimer) fire() {
	f := ft.f
	ft.f = nil
	f()
}

func lines(b *bytes.Buffer) []string {
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}

func expect(t *testing.T, b *bytes.Buffer, want ...string) {
	t.Helper()
	got := lines(b)
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(want), b.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTime(t *testing.T) {
	var b bytes.Buffer
	New(&b, slog.LevelInfo, false, 0).Info("x", slog.Group("g", "time", 1))
	expect(t, &b, `level=INFO msg=x g.time=1`)
	b.Reset()
	New(&b, slog.LevelInfo, true, 0).Info("x")
	if !strings.HasPrefix(b.String(), "time=") {
		t.Fatalf("no time: %s", b.String())
	}
}

func TestLevel(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, slog.LevelWarn, false, time.Minute)
	l.Info("hidden")
	l.Warn("shown")
	expect(t, &b, `level=WARN msg=shown`)
}

func TestThrottle(t *testing.T) {
	var ft fakeTimer
	ft.install(t)
	var b bytes.Buffer
	l := New(&b, slog.LevelDebug, false, time.Minute).With("dns", "a.1.b.switchyard")
	for i := range 6 {
		l.Warn("stream refused", "client", i, "stream", i, "status", "no_target")
	}
	ft.fire()
	l.Warn("stream refused", "client", 9, "status", "no_target")
	l.Info("other")
	expect(t, &b,
		`level=WARN msg="stream refused" dns=a.1.b.switchyard client=0 stream=0 status=no_target`,
		`level=WARN msg="stream refused" dns=a.1.b.switchyard client=1 stream=1 status=no_target`,
		`level=WARN msg="stream refused" dns=a.1.b.switchyard client=2 stream=2 status=no_target`,
		`level=WARN msg="log throttled"`,
		`level=WARN msg="log throttled" repeated=3`,
		`level=WARN msg="log throttle ended" repeated=1 total=4`,
		`level=INFO msg=other dns=a.1.b.switchyard`)
	if ft.stopped != 1 {
		t.Fatalf("timer not stopped: %d", ft.stopped)
	}
}

func TestThrottleQuietEnds(t *testing.T) {
	var ft fakeTimer
	ft.install(t)
	var b bytes.Buffer
	l := New(&b, slog.LevelInfo, false, time.Minute)
	for range 4 {
		l.Info("same")
	}
	ft.fire() // repeated=1 since start: summary
	ft.fire() // no repeats: ended
	l.Info("same")
	expect(t, &b,
		`level=INFO msg=same`, `level=INFO msg=same`, `level=INFO msg=same`,
		`level=WARN msg="log throttled"`,
		`level=WARN msg="log throttled" repeated=1`,
		`level=WARN msg="log throttle ended" repeated=0 total=1`,
		`level=INFO msg=same`)
	if ft.f != nil {
		t.Fatal("timer still armed")
	}
}

func TestStaleTick(t *testing.T) {
	var ft fakeTimer
	ft.install(t)
	var b bytes.Buffer
	l := New(&b, slog.LevelInfo, false, time.Minute)
	for range 4 {
		l.Info("same")
	}
	stale := ft.f
	l.Info("other") // ends the throttling
	stale()
	h := l.Handler().(*handler)
	h.s.tick(h.s.gen) // not throttled
	expect(t, &b,
		`level=INFO msg=same`, `level=INFO msg=same`, `level=INFO msg=same`,
		`level=WARN msg="log throttled"`,
		`level=WARN msg="log throttle ended" repeated=1 total=1`,
		`level=INFO msg=other`)
}

func TestGroupsDiffer(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, slog.LevelInfo, false, time.Minute)
	g := l.WithGroup("g").With("stream", 1)
	for i := range 4 {
		g.Info("m", "client", i)
	}
	expect(t, &b,
		`level=INFO msg=m g.stream=1 g.client=0`,
		`level=INFO msg=m g.stream=1 g.client=1`,
		`level=INFO msg=m g.stream=1 g.client=2`,
		`level=INFO msg=m g.stream=1 g.client=3`)
}

func TestRealTimer(t *testing.T) {
	stop := afterFunc(time.Hour, func() {})
	if !stop() {
		t.Fatal("stop")
	}
}
