// Package export resolves stream targets among containers that share a network with the
// agent and connects them, never via DNS (spec 10).
package export

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/metrics"
	"github.com/motwok/npm-switchyard/internal/wire"
)

// connectTimeout bounds the Docker re-check and the dial of a target; a variable for tests.
var connectTimeout = 10 * time.Second

type ref struct {
	containerID string
	network     string // network name
	ip          string
	labels      metrics.Target
}

type Exporter struct {
	dc            *docker.Client
	selfID        string
	importNetwork string // excluded (EXP-1)
	log           *slog.Logger

	// Scopes are the partners and "local" with their network filters, for the log (LOG-3).
	Scopes []Scope

	mu      sync.Mutex
	targets map[string][]ref // lowercase name -> candidates
	allowed map[[2]string]bool
	members map[[2]string]string // container ID, network -> container name; for the log
	nets    []string             // exported networks of the last refresh
	streams map[*tracked]struct{}

	// Event stream reconnect backoff (EXP-6); fields so tests can shorten them.
	backoffMin, backoffMax time.Duration
}

// Scope is one partner (or "local") and the networks it may reach.
type Scope struct {
	Peer   string
	Allows func(network string) bool
}

type tracked struct {
	conn        net.Conn
	containerID string
	network     string
}

func New(dc *docker.Client, selfID, importNetwork string, log *slog.Logger) *Exporter {
	return &Exporter{
		dc: dc, selfID: selfID, importNetwork: importNetwork, log: log,
		targets: map[string][]ref{}, allowed: map[[2]string]bool{}, streams: map[*tracked]struct{}{},
		backoffMin: time.Second, backoffMax: 60 * time.Second,
	}
}

// Refresh rebuilds the name table from the Docker API and closes streams no longer allowed.
func (e *Exporter) Refresh(ctx context.Context) error {
	self, err := e.dc.Inspect(ctx, e.selfID)
	if err != nil {
		return err
	}
	shared := map[string]string{} // network ID -> name
	var nets []string
	for name, ep := range self.NetworkSettings.Networks {
		if name == e.importNetwork {
			continue
		}
		shared[ep.NetworkID] = name
		nets = append(nets, name)
	}
	slices.Sort(nets)
	cs, err := e.dc.Running(ctx)
	if err != nil {
		return err
	}
	targets := map[string][]ref{}
	allowed := map[[2]string]bool{}
	members := map[[2]string]string{}
	for _, c := range cs {
		if c.ID == self.ID {
			continue
		}
		sharesOne := false
		for _, ep := range c.NetworkSettings.Networks {
			if _, ok := shared[ep.NetworkID]; ok {
				sharesOne = true
			}
		}
		if !sharesOne {
			continue
		}
		// The list omits Aliases and DNSNames; inspect has them.
		if full, err := e.dc.Inspect(ctx, c.ID); err == nil {
			c = full
		} else if !docker.IsNotFound(err) {
			return err
		}
		mt := metrics.Target{
			Container: c.DisplayName(),
			Service:   c.Label("com.docker.compose.service"),
			Stack:     c.Label("com.docker.compose.project"),
		}
		for netName, ep := range c.NetworkSettings.Networks {
			name, ok := shared[ep.NetworkID]
			if !ok || ep.IPAddress == "" {
				continue
			}
			r := ref{containerID: c.ID, network: name, ip: ep.IPAddress, labels: mt}
			allowed[[2]string{c.ID, name}] = true
			members[[2]string{c.ID, name}] = c.DisplayName()
			seen := map[string]bool{}
			for _, n := range dnsNames(c, netName, ep) {
				n = strings.ToLower(n)
				for _, k := range []string{n, n + "." + strings.ToLower(name)} {
					if !seen[k] {
						seen[k] = true
						targets[k] = append(targets[k], r)
					}
				}
			}
		}
	}
	e.mu.Lock()
	e.targets, e.allowed = targets, allowed
	oldMembers, oldNets := e.members, e.nets
	e.members, e.nets = members, nets
	var drop []*tracked
	for t := range e.streams {
		if !allowed[[2]string{t.containerID, t.network}] {
			drop = append(drop, t)
		}
	}
	e.mu.Unlock()
	if !slices.Equal(nets, oldNets) || oldMembers == nil {
		e.logNetworks(nets)
	}
	logMembers(e.log, oldMembers, members)
	for _, t := range drop {
		e.log.Info("closing stream, target no longer shared", "container", t.containerID[:12], "network", t.network)
		t.conn.Close()
	}
	return nil
}

// logNetworks shows which networks are exported, overall and per partner.
func (e *Exporter) logNetworks(nets []string) {
	e.log.Debug("exported networks", "networks", nets, "import_network", e.importNetwork)
	for _, sc := range e.Scopes {
		var in, out []string
		for _, n := range nets {
			if sc.Allows(n) {
				in = append(in, n)
			} else {
				out = append(out, n)
			}
		}
		e.log.Debug("exported networks", "peer", sc.Peer, "networks", in, "excluded", out)
	}
}

func logMembers(log *slog.Logger, old, cur map[[2]string]string) {
	for k, name := range cur {
		if _, ok := old[k]; !ok {
			log.Debug("container joined an exported network", "container", name, "network", k[1])
		}
	}
	for k, name := range old {
		if _, ok := cur[k]; !ok {
			log.Debug("container left an exported network", "container", name, "network", k[1])
		}
	}
}

// dnsNames is the endpoint's DNSNames. Docker's default bridge network has none; there the
// container name, its aliases and the short ID count (decided 2026-10-31).
func dnsNames(c docker.Container, _ string, ep docker.Endpoint) []string {
	if len(ep.DNSNames) > 0 {
		return ep.DNSNames
	}
	out := append([]string{c.DisplayName()}, ep.Aliases...)
	if len(c.ID) >= 12 {
		out = append(out, c.ID[:12])
	}
	return out
}

// Run keeps the table current from Docker events (EXP-5, EXP-6). Refreshes run in their own
// goroutine and are repeated with backoff until one succeeds, so a failed Docker call does not
// leave the table stale until the next event.
func (e *Exporter) Run(ctx context.Context) {
	kick := make(chan struct{}, 1)
	ask := func() { nudge(kick) }
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.refresher(ctx, kick)
	}()
	defer wg.Wait() // Run returns only once the refresher is gone
	wait := e.backoffMin
	for ctx.Err() == nil {
		ask()
		start := time.Now()
		err := e.dc.Events(ctx, func(ev docker.Event) {
			switch ev.Type + "/" + ev.Action {
			case "container/start", "container/die", "container/destroy",
				"network/connect", "network/disconnect", "network/destroy":
				e.log.Debug("docker event", "event", ev.Type+"/"+ev.Action, "actor", ev.Actor.ID)
				ask()
			}
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > e.backoffMax {
			wait = e.backoffMin
		}
		e.log.Warn("docker event stream lost", "err", err, "retry", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, e.backoffMax)
	}
}

// nudge asks for a refresh; one pending request covers any number of events.
func nudge(kick chan<- struct{}) {
	select {
	case kick <- struct{}{}:
	default:
	}
}

// refresher rebuilds the table on every kick, retrying with backoff while Docker fails.
func (e *Exporter) refresher(ctx context.Context, kick <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		}
		wait := e.backoffMin
		for ctx.Err() == nil {
			err := e.Refresh(ctx)
			if err == nil {
				break
			}
			e.log.Warn("docker refresh", "err", err, "retry", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(wait*2, e.backoffMax)
		}
	}
}

// Connect implements peer.Exporter (EXP-1..4); allow limits the networks for this partner.
// The stream's ID and partner for the log come from ctx (wire.WithStream).
func (e *Exporter) Connect(ctx context.Context, t wire.Target, allow func(network string) bool) (net.Conn, metrics.Target, byte) {
	network := "tcp"
	if t.Proto == wire.UDP {
		network = "udp"
	}
	si := wire.StreamFrom(ctx)
	log := e.log.With("stream", si.ID, "peer", si.Peer, "name", t.Name, "port", t.Port, "proto", network)
	log.Debug("export request")
	e.mu.Lock()
	var cands []ref
	var denied []string
	all := e.targets[strings.ToLower(t.Name)]
	for _, r := range all {
		if allow(r.network) {
			cands = append(cands, r)
		} else {
			denied = append(denied, r.network)
		}
	}
	e.mu.Unlock()
	if len(all) == 0 {
		log.Warn("export refused", "reason", "no container with this name in an exported network")
		return nil, metrics.Target{}, wire.StatusNoTarget
	}
	if len(cands) == 0 {
		log.Warn("export refused", "reason", "networks not allowed for this partner", "networks", denied)
		return nil, metrics.Target{}, wire.StatusNoTarget
	}
	r := cands[rand.IntN(len(cands))]
	log = log.With("container", r.labels.Container, "network", r.network, "addr", r.ip)
	ictx, cancel := context.WithTimeout(ctx, connectTimeout) // a stalled daemon must not hold the stream
	defer cancel()
	c, err := e.dc.Inspect(ictx, r.containerID)
	if err != nil || !c.State.Running || c.NetworkSettings.Networks[r.network].IPAddress != r.ip {
		log.Warn("export refused", "reason", "container stopped or changed its address", "err", err)
		return nil, r.labels, wire.StatusNoTarget
	}
	d := net.Dialer{Timeout: connectTimeout}
	conn, err := d.DialContext(ctx, network, net.JoinHostPort(r.ip, strconv.Itoa(int(t.Port))))
	if err != nil {
		log.Warn("export refused", "reason", "connect failed", "err", err)
		return nil, r.labels, wire.StatusUnreachable
	}
	log.Debug("export connected")
	return &refConn{Conn: conn, ref: r}, r.labels, wire.StatusOK
}

type refConn struct {
	net.Conn
	ref ref
}

func (c *refConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// NetConn exposes the socket, so a reset can be passed on (TRN-2).
func (c *refConn) NetConn() net.Conn { return c.Conn }

// Track registers a connected target for opt-out (EXP-5); call the result when done.
func (e *Exporter) Track(c net.Conn, _ metrics.Target) func() {
	rc, ok := c.(*refConn)
	if !ok {
		return func() {}
	}
	t := &tracked{conn: c, containerID: rc.ref.containerID, network: rc.ref.network}
	e.mu.Lock()
	e.streams[t] = struct{}{}
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		delete(e.streams, t)
		e.mu.Unlock()
	}
}
