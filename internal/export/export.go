// Package export resolves stream targets among containers that share a network with the
// agent and connects them, never via DNS (spec 10).
package export

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/metrics"
	"github.com/motwok/npm-switchyard/internal/wire"
)

const (
	connectTimeout = 10 * time.Second
	backoffMax     = 60 * time.Second
)

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

	mu      sync.Mutex
	targets map[string][]ref // lowercase name -> candidates
	allowed map[[2]string]bool
	streams map[*tracked]struct{}
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
	}
}

// Refresh rebuilds the name table from the Docker API and closes streams no longer allowed.
func (e *Exporter) Refresh(ctx context.Context) error {
	self, err := e.dc.Inspect(ctx, e.selfID)
	if err != nil {
		return err
	}
	shared := map[string]string{} // network ID -> name
	for name, ep := range self.NetworkSettings.Networks {
		if name == e.importNetwork {
			continue
		}
		shared[ep.NetworkID] = name
	}
	cs, err := e.dc.Running(ctx)
	if err != nil {
		return err
	}
	targets := map[string][]ref{}
	allowed := map[[2]string]bool{}
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
	var drop []*tracked
	for t := range e.streams {
		if !allowed[[2]string{t.containerID, t.network}] {
			drop = append(drop, t)
		}
	}
	e.mu.Unlock()
	for _, t := range drop {
		e.log.Info("closing stream, target no longer shared", "container", t.containerID[:12], "network", t.network)
		t.conn.Close()
	}
	return nil
}

// dnsNames is the endpoint's DNSNames; older daemons without it fall back to name and aliases.
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

// Run keeps the table current from Docker events (EXP-5, EXP-6).
func (e *Exporter) Run(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		if err := e.Refresh(ctx); err != nil {
			e.log.Warn("docker refresh", "err", err)
		}
		start := time.Now()
		err := e.dc.Events(ctx, func(ev docker.Event) {
			switch ev.Type + "/" + ev.Action {
			case "container/start", "container/die", "container/destroy",
				"network/connect", "network/disconnect", "network/destroy":
				if err := e.Refresh(ctx); err != nil {
					e.log.Warn("docker refresh", "err", err)
				}
			}
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > backoffMax {
			wait = time.Second
		}
		e.log.Warn("docker event stream lost", "err", err, "retry", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, backoffMax)
	}
}

// Connect implements peer.Exporter (EXP-1..4); allow limits the networks for this partner.
func (e *Exporter) Connect(ctx context.Context, t wire.Target, allow func(network string) bool) (net.Conn, metrics.Target, byte) {
	e.mu.Lock()
	var cands []ref
	for _, r := range e.targets[strings.ToLower(t.Name)] {
		if allow(r.network) {
			cands = append(cands, r)
		}
	}
	e.mu.Unlock()
	if len(cands) == 0 {
		return nil, metrics.Target{}, wire.StatusNoTarget
	}
	r := cands[rand.IntN(len(cands))]
	c, err := e.dc.Inspect(ctx, r.containerID)
	if err != nil || !c.State.Running || c.NetworkSettings.Networks[r.network].IPAddress != r.ip {
		return nil, r.labels, wire.StatusNoTarget
	}
	network := "tcp"
	if t.Proto == wire.UDP {
		network = "udp"
	}
	d := net.Dialer{Timeout: connectTimeout}
	conn, err := d.DialContext(ctx, network, net.JoinHostPort(r.ip, strconv.Itoa(int(t.Port))))
	if err != nil {
		return nil, r.labels, wire.StatusUnreachable
	}
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
