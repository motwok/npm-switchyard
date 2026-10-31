// Package metrics keeps the agent's series and writes the Prometheus text format (spec 12).
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

const prefix = "switchyard_"

// Target labels of the container a stream goes to; zero on the importing side (MET-2).
type Target struct {
	Container, Service, Stack string
}

type key struct {
	peer, extra string
	t           Target
}

type Registry struct {
	mu      sync.Mutex
	peerUp  map[string]bool
	active  map[key]int64
	total   map[key]uint64
	bytes   map[key]uint64
	free    int64
	freeSet bool
}

func New() *Registry {
	return &Registry{
		peerUp: map[string]bool{},
		active: map[key]int64{},
		total:  map[key]uint64{},
		bytes:  map[key]uint64{},
	}
}

func (r *Registry) SetPeerUp(peer string, up bool) {
	r.mu.Lock()
	r.peerUp[peer] = up
	r.mu.Unlock()
}

func (r *Registry) StreamStarted(peer string, t Target) {
	r.mu.Lock()
	r.active[key{peer: peer, t: t}]++
	r.mu.Unlock()
}

func (r *Registry) StreamEnded(peer string, t Target) {
	r.mu.Lock()
	k := key{peer: peer, t: t}
	if r.active[k]--; r.active[k] <= 0 {
		delete(r.active, k)
	}
	r.mu.Unlock()
}

// StreamResult counts a stream outcome: ok, refused or error.
func (r *Registry) StreamResult(peer, result string, t Target) {
	r.mu.Lock()
	r.total[key{peer: peer, extra: result, t: t}]++
	r.mu.Unlock()
}

// AddBytes counts payload bytes; direction is in (towards the client) or out.
func (r *Registry) AddBytes(peer, direction string, t Target, n int64) {
	if n <= 0 {
		return
	}
	r.mu.Lock()
	r.bytes[key{peer: peer, extra: direction, t: t}] += uint64(n)
	r.mu.Unlock()
}

func (r *Registry) SetAddressesFree(n int) {
	r.mu.Lock()
	r.free, r.freeSet = int64(n), true
	r.mu.Unlock()
}

func esc(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func labels(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf(`%s="%s"`, pairs[i], esc(pairs[i+1])))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func targetPairs(t Target) []string {
	return []string{"container", t.Container, "service", t.Service, "stack", t.Stack}
}

func sortedLines(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Write emits all series in text format 0.0.4.
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	head := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s%s %s\n# TYPE %s%s %s\n", prefix, name, help, prefix, name, typ)
	}

	head("peer_up", "gauge", "1 if the control channel to the partner is up.")
	lines := map[string]string{}
	for p, up := range r.peerUp {
		v := 0
		if up {
			v = 1
		}
		lines[p] = fmt.Sprintf("%speer_up%s %d\n", prefix, labels("peer", p), v)
	}
	for _, l := range sortedLines(lines) {
		b.WriteString(l)
	}

	head("streams_active", "gauge", "Streams currently open.")
	lines = map[string]string{}
	for k, v := range r.active {
		l := fmt.Sprintf("%sstreams_active%s %d\n", prefix, labels(append([]string{"peer", k.peer}, targetPairs(k.t)...)...), v)
		lines[l] = l
	}
	for _, l := range sortedLines(lines) {
		b.WriteString(l)
	}

	head("streams_total", "counter", "Streams by result.")
	lines = map[string]string{}
	for k, v := range r.total {
		l := fmt.Sprintf("%sstreams_total%s %d\n", prefix, labels(append([]string{"peer", k.peer, "result", k.extra}, targetPairs(k.t)...)...), v)
		lines[l] = l
	}
	for _, l := range sortedLines(lines) {
		b.WriteString(l)
	}

	head("bytes_total", "counter", "Payload bytes by direction.")
	lines = map[string]string{}
	for k, v := range r.bytes {
		l := fmt.Sprintf("%sbytes_total%s %d\n", prefix, labels(append([]string{"peer", k.peer, "direction", k.extra}, targetPairs(k.t)...)...), v)
		lines[l] = l
	}
	for _, l := range sortedLines(lines) {
		b.WriteString(l)
	}

	if r.freeSet {
		head("addresses_free", "gauge", "Free addresses for import names.")
		fmt.Fprintf(&b, "%saddresses_free %d\n", prefix, r.free)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
