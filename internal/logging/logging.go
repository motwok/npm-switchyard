// Package logging builds the agent's logger (spec LOG-1, LOG-6).
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// threshold is how often the same message is written before throttling starts (LOG-6).
const threshold = 3

// volatile fields differ per stream and do not make a message different (LOG-6).
var volatile = map[string]bool{"stream": true, "client": true, "duration": true, "bytes_in": true, "bytes_out": true}

// afterFunc is time.AfterFunc; a variable for tests.
var afterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }

// New returns a text logger; time only if withTime, throttling unless every is 0.
func New(w io.Writer, level slog.Level, withTime bool, every time.Duration) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if !withTime {
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{}
			}
			return a
		}
	}
	h := slog.Handler(slog.NewTextHandler(w, opts))
	if every > 0 {
		h = &handler{s: &state{out: h, every: every}, inner: h}
	}
	return slog.New(h)
}

// state is shared by all handlers derived from one logger: one last message for the whole log.
type state struct {
	mu        sync.Mutex
	out       slog.Handler
	every     time.Duration
	last      string
	count     int
	throttled bool
	repeated  int
	total     int
	stop      func() bool
	gen       int
}

type handler struct {
	s      *state
	inner  slog.Handler
	prefix string
	attrs  string
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	return &handler{s: h.s, inner: h.inner.WithAttrs(as), prefix: h.prefix, attrs: h.attrs + key(h.prefix, as)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{s: h.s, inner: h.inner.WithGroup(name), prefix: h.prefix + name + ".", attrs: h.attrs}
}

func key(prefix string, as []slog.Attr) string {
	var b strings.Builder
	for _, a := range as {
		if prefix == "" && volatile[a.Key] {
			continue
		}
		b.WriteString(" " + prefix + a.Key + "=" + a.Value.Resolve().String())
	}
	return b.String()
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	var as []slog.Attr
	r.Attrs(func(a slog.Attr) bool { as = append(as, a); return true })
	k := r.Level.String() + " " + r.Message + h.attrs + key(h.prefix, as)
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if k == s.last {
		s.count++
		if s.throttled {
			s.repeated++
			s.total++
			return nil
		}
		if s.count > threshold {
			s.throttled, s.repeated, s.total = true, 1, 1
			s.arm()
			return s.write("log throttled")
		}
		return h.inner.Handle(ctx, r)
	}
	if s.throttled {
		s.end()
	}
	s.last, s.count = k, 1
	return h.inner.Handle(ctx, r)
}

// arm starts the next summary interval.
func (s *state) arm() {
	s.gen++
	gen := s.gen
	s.stop = afterFunc(s.every, func() { s.tick(gen) })
}

// tick writes the summary, or ends the throttling after an interval without repeats.
func (s *state) tick(gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen || !s.throttled {
		return
	}
	if s.repeated == 0 {
		s.end()
		s.last, s.count = "", 0
		return
	}
	_ = s.write("log throttled", "repeated", s.repeated)
	s.repeated = 0
	s.arm()
}

func (s *state) end() {
	s.stop()
	s.gen++
	s.throttled = false
	_ = s.write("log throttle ended", "repeated", s.repeated, "total", s.total)
}

func (s *state) write(msg string, args ...any) error {
	r := slog.NewRecord(time.Now(), slog.LevelWarn, msg, 0)
	r.Add(args...)
	return s.out.Handle(context.Background(), r)
}
