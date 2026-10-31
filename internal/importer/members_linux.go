//go:build linux

package importer

import (
	"context"
	"net/netip"
	"time"

	"github.com/motwok/npm-switchyard/internal/docker"
)

// watchMembers follows containers joining the import network: a fixed address outside
// ip_range is taken out of the pool, or logged if the agent already uses it (IMP-8).
func (im *Importer) watchMembers(ctx context.Context) {
	lo, hi := im.tune.backoffMin, im.tune.backoffMax
	wait := lo
	for ctx.Err() == nil {
		start := time.Now()
		err := im.dc.Events(ctx, func(ev docker.Event) {
			if ev.Type != "network" || ev.Action != "connect" || ev.Actor.ID != im.networkID {
				return
			}
			im.joined(ctx, ev.Actor.Attributes["container"])
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

func (im *Importer) joined(ctx context.Context, id string) {
	c, err := im.dc.Inspect(ctx, id)
	if err != nil {
		return
	}
	a, err := netip.ParseAddr(c.NetworkSettings.Networks[im.network].IPAddress)
	if err != nil {
		return
	}
	im.mu.Lock()
	name, conflict := im.table.Joined(a)
	im.met.SetAddressesFree(im.table.Free())
	im.mu.Unlock()
	if conflict {
		im.log.Error("container joined the import network with an address the agent uses",
			"container", c.DisplayName(), "addr", a, "name", name)
	}
}
