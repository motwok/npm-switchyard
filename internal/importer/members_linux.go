//go:build linux

package importer

import (
	"context"
	"net/netip"
	"time"

	"github.com/motwok/npm-switchyard/internal/docker"
)

// watchMembers follows containers joining the import network: a fixed address outside
// ip_range is taken out of the pool, or logged if the agent already uses it (IMP-8). Before
// each (re)connect of the event stream the members are read again, so nothing that joined
// while no stream was open is missed; in fallback mode the DNS-5 names follow the same events.
func (im *Importer) watchMembers(ctx context.Context) {
	lo, hi := im.tune.backoffMin, im.tune.backoffMax
	wait := lo
	for ctx.Err() == nil {
		im.refreshMembers(ctx)
		start := time.Now()
		err := im.dc.Events(ctx, func(ev docker.Event) {
			if ev.Type == "network" && ev.Action == "connect" && ev.Actor.ID == im.networkID {
				im.joined(ctx, ev.Actor.Attributes["container"])
			}
			switch ev.Type + "/" + ev.Action {
			case "container/start", "container/die", "container/destroy",
				"network/connect", "network/disconnect", "network/destroy":
				if im.fallback {
					im.refreshOwnNames(ctx)
				}
			}
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > hi {
			wait = lo
		}
		im.log.Warn("docker event stream lost (import network)", "err", err, "retry", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, hi)
	}
}

// refreshMembers reserves the addresses of all current members of the import network.
func (im *Importer) refreshMembers(ctx context.Context) {
	nw, err := im.dc.Network(ctx, im.networkID)
	if err != nil {
		return
	}
	for name, c := range nw.Containers {
		if p, err := netip.ParsePrefix(c.IPv4Address); err == nil {
			im.reserve(p.Addr(), name)
		}
	}
	if im.fallback {
		im.refreshOwnNames(ctx)
	}
}

func (im *Importer) joined(ctx context.Context, id string) {
	c, err := im.dc.Inspect(ctx, id)
	if err != nil {
		return
	}
	a, err := netip.ParseAddr(c.NetworkSettings.Networks[im.network].IPAddress)
	if err != nil {
		return
	}
	im.log.Debug("container joined the import network", "container", c.DisplayName(), "addr", a)
	im.reserve(a, c.DisplayName())
}

// reserve takes a member's address out of the pool and logs a conflict with a name (IMP-8).
func (im *Importer) reserve(a netip.Addr, container string) {
	im.mu.Lock()
	name, conflict := im.table.Joined(a)
	im.met.SetAddressesFree(im.table.Free())
	im.mu.Unlock()
	if conflict {
		im.log.Error("container joined the import network with an address the agent uses",
			"container", container, "addr", a, "name", name)
	}
}
