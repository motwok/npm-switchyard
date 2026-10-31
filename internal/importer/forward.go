package importer

import (
	"bufio"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// ExtServers parses Docker's "# ExtServers: [host(1.2.3.4) 5.6.7.8]" line and drops servers
// the container cannot reach: loopback, and IPv6 without IPv6 in the network (DNS-4).
func ExtServers(resolvConf string, ipv6 bool) []string {
	f, err := os.Open(resolvConf)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		rest, ok := strings.CutPrefix(line, "# ExtServers:")
		if !ok {
			continue
		}
		rest = strings.Trim(strings.TrimSpace(rest), "[]")
		for _, f := range strings.Fields(rest) {
			f = strings.TrimSuffix(strings.TrimPrefix(f, "host("), ")")
			a, err := netip.ParseAddr(f)
			if err != nil || a.IsLoopback() || a.Is6() && !a.Is4In6() && !ipv6 {
				continue
			}
			out = append(out, netip.AddrPortFrom(a, 53).String())
		}
	}
	return out
}

// Forwarder sends a query to the servers in order, skipping SERVFAIL and REFUSED as Docker
// does (DNS-4, DNS-6).
type Forwarder struct {
	Servers []string
	client  dns.Client
}

func NewForwarder(servers []string) *Forwarder {
	return &Forwarder{Servers: servers, client: dns.Client{Timeout: 4 * time.Second}}
}

func (f *Forwarder) Exchange(q *dns.Msg, tcp bool) *dns.Msg {
	c := f.client
	if tcp {
		c.Net = "tcp"
	}
	for _, s := range f.Servers {
		r, _, err := c.Exchange(q, s)
		if err != nil || r == nil {
			continue
		}
		if r.Rcode == dns.RcodeServerFailure || r.Rcode == dns.RcodeRefused {
			continue
		}
		return r
	}
	return nil
}
