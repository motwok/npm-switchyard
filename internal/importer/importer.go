//go:build linux

package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/metrics"
	"github.com/motwok/npm-switchyard/internal/names"
	"github.com/motwok/npm-switchyard/internal/npmconf"
	"github.com/motwok/npm-switchyard/internal/peer"
	"github.com/motwok/npm-switchyard/internal/wire"
)

const (
	ttlPositive = 60
	ttlNegative = 5
)

// expireEvery is how often quarantined short names are released.
var expireEvery = time.Minute

// Importer is the import side on Linux.
type Importer struct {
	log    *slog.Logger
	met    *metrics.Registry
	open   Opener
	npm    *npmconf.Store
	fwd    *Forwarder
	ownNet map[string]bool // names answered NXDOMAIN when falling back to 127.0.0.11 (DNS-5)

	Own     netip.Addr
	bits    int
	ifIndex int

	dc        *docker.Client
	networkID string
	network   string

	mu        sync.Mutex
	table     *Table
	listeners map[netip.AddrPort]*listenerPair
}

type listenerPair struct {
	tcp net.Listener
	udp *net.UDPConn
}

// Setup reads the import network from the Docker API (IMP-1) and prepares the address table.
func Setup(ctx context.Context, dc *docker.Client, selfID string, cfg *config.Config, open Opener,
	met *metrics.Registry, log *slog.Logger) (*Importer, error) {
	self, err := dc.Inspect(ctx, selfID)
	if err != nil {
		return nil, fmt.Errorf("inspect own container: %w", err)
	}
	ep, ok := self.NetworkSettings.Networks[cfg.ImportNetwork]
	if !ok {
		return nil, fmt.Errorf("agent is not a member of import network %q", cfg.ImportNetwork)
	}
	nw, err := dc.Network(ctx, ep.NetworkID)
	if err != nil {
		return nil, fmt.Errorf("inspect import network: %w", err)
	}
	var ic *docker.IPAMConfig
	for i := range nw.IPAM.Config {
		if p, err := netip.ParsePrefix(nw.IPAM.Config[i].Subnet); err == nil && p.Addr().Is4() {
			ic = &nw.IPAM.Config[i]
			break
		}
	}
	if ic == nil {
		return nil, errors.New("import network has no IPv4 subnet")
	}
	if ic.IPRange == "" {
		return nil, errors.New("import network has no ip_range")
	}
	subnet, _ := netip.ParsePrefix(ic.Subnet)
	ipRange, err := netip.ParsePrefix(ic.IPRange)
	if err != nil {
		return nil, fmt.Errorf("ip_range %q: %w", ic.IPRange, err)
	}
	own, err := netip.ParseAddr(ep.IPAddress)
	if err != nil {
		return nil, fmt.Errorf("own address %q: %w", ep.IPAddress, err)
	}
	var exclude []netip.Addr
	exclude = append(exclude, own)
	if g, err := netip.ParseAddr(ic.Gateway); err == nil {
		exclude = append(exclude, g)
	}
	for _, a := range ic.AuxAddress {
		if x, err := netip.ParseAddr(a); err == nil {
			exclude = append(exclude, x)
		}
	}
	for _, c := range nw.Containers {
		if p, err := netip.ParsePrefix(c.IPv4Address); err == nil {
			exclude = append(exclude, p.Addr())
		}
	}
	pool, _ := NewPool(subnet, ipRange, exclude) // cannot fail: the subnet is IPv4, checked above
	idx, err := ifaceFor(own)
	if err != nil {
		return nil, err
	}

	im := &Importer{
		log: log, met: met, open: open,
		npm:       npmconf.NewStore(cfg.NPMData),
		Own:       own,
		bits:      subnet.Bits(),
		ifIndex:   idx,
		table:     NewTable(pool, own, []int{53, cfg.Listen, cfg.Metrics}),
		listeners: map[netip.AddrPort]*listenerPair{},
		ownNet:    map[string]bool{},
		dc:        dc,
		networkID: ep.NetworkID,
		network:   cfg.ImportNetwork,
	}
	servers := ExtServers("/etc/resolv.conf", nw.EnableIPv6)
	if len(servers) == 0 {
		servers = []string{"127.0.0.11:53"}
		im.fillOwnNames(ctx, dc, self)
		log.Info("no reachable upstream DNS server, using 127.0.0.11")
	}
	im.fwd = NewForwarder(servers)
	met.SetAddressesFree(pool.Free())
	return im, nil
}

// fillOwnNames collects names Docker's resolver would answer from the agent's networks (DNS-5).
func (im *Importer) fillOwnNames(ctx context.Context, dc *docker.Client, self docker.Container) {
	cs, err := dc.Running(ctx)
	if err != nil {
		return
	}
	mine := map[string]string{}
	for name, ep := range self.NetworkSettings.Networks {
		mine[ep.NetworkID] = strings.ToLower(name)
	}
	for _, c := range cs {
		if full, err := dc.Inspect(ctx, c.ID); err == nil {
			c = full // the list omits Aliases and DNSNames
		}
		for _, ep := range c.NetworkSettings.Networks {
			nw, ok := mine[ep.NetworkID]
			if !ok {
				continue
			}
			ns := append([]string{c.DisplayName()}, ep.Aliases...)
			ns = append(ns, ep.DNSNames...)
			for _, n := range ns {
				n = strings.ToLower(n)
				im.ownNet[n] = true
				im.ownNet[n+"."+nw] = true
			}
		}
	}
}

// interfaces is replaceable in tests.
var interfaces = net.Interfaces

func ifaceFor(a netip.Addr) (int, error) {
	ifs, err := interfaces()
	if err != nil {
		return 0, err
	}
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		for _, x := range addrs {
			if n, ok := x.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == a {
					return i.Index, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("no interface with %s", a)
}

// Serve starts the DNS server on the agent's IP (DNS-1) and the NPM watcher. ready is
// closed once DNS listens (IMP-11).
func (im *Importer) Serve(ctx context.Context, ready chan<- struct{}) error {
	addr := netip.AddrPortFrom(im.Own, 53).String()
	h := dns.HandlerFunc(im.handle)
	udp := &dns.Server{Addr: addr, Net: "udp", Handler: h}
	tcp := &dns.Server{Addr: addr, Net: "tcp", Handler: h}
	errc := make(chan error, 2)
	var started sync.WaitGroup
	started.Add(2)
	udp.NotifyStartedFunc = started.Done
	tcp.NotifyStartedFunc = started.Done
	go func() { errc <- udp.ListenAndServe() }()
	go func() { errc <- tcp.ListenAndServe() }()
	go func() { started.Wait(); close(ready) }()

	if im.npm.Mounted() {
		im.npm.Reload()
		go im.watchNPM(ctx)
	} else {
		im.log.Info("NPM files not mounted, short names off")
	}
	go im.expireLoop(ctx)
	go im.watchMembers(ctx)

	select {
	case <-ctx.Done():
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
		return nil
	case err := <-errc:
		return fmt.Errorf("dns: %w", err)
	}
}

func (im *Importer) expireLoop(ctx context.Context) {
	t := time.NewTicker(expireEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			im.mu.Lock()
			im.table.Expire(time.Now())
			im.met.SetAddressesFree(im.table.Free())
			im.mu.Unlock()
		}
	}
}

// syncShort applies NPM changes to assigned short names (IMP-7).
func (im *Importer) syncShort() {
	current := im.npm.Names()
	im.mu.Lock()
	defer im.mu.Unlock()
	for _, n := range im.table.ShortNames() {
		ports, ok := current[n]
		if !ok {
			if e, ok := im.table.FreeShort(n, time.Now()); ok {
				for p := range e.Ports {
					im.closeListener(netip.AddrPortFrom(e.Addr, uint16(p)))
				}
				im.log.Info("short name gone from NPM, in quarantine", "name", n)
			}
			continue
		}
		if e, _, added, err := im.table.Short(n, ports); err == nil {
			im.listenPorts(e, added)
		}
	}
}

func (im *Importer) handle(w dns.ResponseWriter, q *dns.Msg) {
	if len(q.Question) != 1 {
		m := new(dns.Msg)
		m.SetRcode(q, dns.RcodeFormatError)
		_ = w.WriteMsg(m)
		return
	}
	qn := q.Question[0].Name
	if !names.HasSuffix(qn) {
		im.forward(w, q)
		return
	}
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	e, ok := im.resolve(qn)
	if !ok {
		m.Rcode = dns.RcodeNameError
		m.Ns = []dns.RR{soa(ttlNegative)}
		_ = w.WriteMsg(m)
		return
	}
	if q.Question[0].Qtype == dns.TypeA || q.Question[0].Qtype == dns.TypeANY {
		m.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: qn, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttlPositive},
			A:   e.Addr.AsSlice(),
		}}
	} else {
		m.Ns = []dns.RR{soa(ttlNegative)}
	}
	_ = w.WriteMsg(m)
}

func soa(ttl uint32) dns.RR {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: names.Suffix + ".", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: ttl},
		Ns:      "ns." + names.Suffix + ".",
		Mbox:    "hostmaster." + names.Suffix + ".",
		Serial:  1,
		Refresh: 3600, Retry: 600, Expire: 86400,
		Minttl: ttl,
	}
}

func (im *Importer) forward(w dns.ResponseWriter, q *dns.Msg) {
	if len(im.ownNet) > 0 {
		n := strings.ToLower(strings.TrimSuffix(q.Question[0].Name, "."))
		if im.ownNet[n] {
			m := new(dns.Msg)
			m.SetRcode(q, dns.RcodeNameError)
			_ = w.WriteMsg(m)
			return
		}
	}
	_, tcp := w.RemoteAddr().(*net.TCPAddr)
	r := im.fwd.Exchange(q, tcp)
	if r == nil {
		m := new(dns.Msg)
		m.SetRcode(q, dns.RcodeServerFailure)
		_ = w.WriteMsg(m)
		return
	}
	r.Id = q.Id
	_ = w.WriteMsg(r)
}

// resolve validates a name and assigns its address (NAM-*, IMP-3, IMP-6).
func (im *Importer) resolve(qn string) (*Entry, bool) {
	n, ok := names.Parse(qn)
	if !ok || !im.open.Known(n.Partner) {
		return nil, false
	}
	key := n.String()
	if n.Short() {
		ports := im.npm.Ports(key)
		if ports == nil && im.npm.Mounted() {
			im.npm.Reload() // NAM-6: nginx -t may be ahead of inotify
			ports = im.npm.Ports(key)
		}
		if ports == nil {
			return nil, false
		}
		im.mu.Lock()
		defer im.mu.Unlock()
		e, newAddr, added, err := im.table.Short(key, ports)
		if err != nil {
			im.full(key)
			return nil, false
		}
		if newAddr && !im.alias(e) {
			return nil, false
		}
		im.listenPorts(e, added)
		im.met.SetAddressesFree(im.table.Free())
		return e, true
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	if e, ok := im.table.Lookup(key); ok {
		return e, true
	}
	e, newAddr, err := im.table.Long(key, n.Port)
	if err != nil {
		im.full(key)
		return nil, false
	}
	if newAddr && !im.alias(e) {
		return nil, false
	}
	im.listenPorts(e, []int{n.Port})
	im.met.SetAddressesFree(im.table.Free())
	return e, true
}

func (im *Importer) full(name string) {
	im.met.SetAddressesFree(0)
	im.log.Warn("no free address in the import network", "name", name)
}

func (im *Importer) alias(e *Entry) bool {
	if err := addAlias(im.ifIndex, e.Addr.AsSlice(), im.bits); err != nil {
		im.log.Error("add address", "addr", e.Addr, "name", e.Name, "err", err)
		return false
	}
	return true
}

// listenPorts opens TCP and UDP listeners for an entry's ports (IMP-5). Caller holds im.mu.
func (im *Importer) listenPorts(e *Entry, ports []int) {
	for _, p := range ports {
		ap := netip.AddrPortFrom(e.Addr, uint16(p))
		if im.listeners[ap] != nil {
			continue
		}
		tl, err := net.Listen("tcp", ap.String())
		if err != nil {
			im.log.Error("listen tcp", "addr", ap, "name", e.Name, "err", err)
			continue
		}
		ul, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(ap))
		if err != nil {
			tl.Close()
			im.log.Error("listen udp", "addr", ap, "name", e.Name, "err", err)
			continue
		}
		im.listeners[ap] = &listenerPair{tcp: tl, udp: ul}
		n, _ := names.Parse(e.Name)
		go im.acceptTCP(tl, n, p)
		go im.serveUDP(ul, n, p)
		im.log.Debug("listening", "addr", ap, "name", e.Name)
	}
}

func (im *Importer) closeListener(ap netip.AddrPort) {
	if l := im.listeners[ap]; l != nil {
		l.tcp.Close()
		l.udp.Close()
		delete(im.listeners, ap)
	}
}

func (im *Importer) target(n names.Name, port int, proto byte) wire.Target {
	return wire.Target{Proto: proto, Port: uint16(port), Name: n.Container}
}

func (im *Importer) acceptTCP(l net.Listener, n names.Name, port int) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go im.serveTCP(c, n, port)
	}
}

func (im *Importer) serveTCP(c net.Conn, n names.Name, port int) {
	ctx := context.Background()
	s, st, err := im.open.Open(ctx, n.Partner, im.target(n, port, wire.TCP))
	if err != nil || st != wire.StatusOK {
		if s != nil {
			s.Close()
		}
		im.reset(c)
		if n.Partner != config.LocalPartner {
			res := "refused"
			if err != nil {
				res = "error"
			}
			im.met.StreamResult(n.Partner, res, metrics.Target{})
		}
		im.log.Debug("stream refused", "name", n.String(), "port", port, "status", wire.StatusReason(st), "err", err)
		return
	}
	if n.Partner == config.LocalPartner {
		peer.Pipe(c, s, func(string, int64) {})
		return
	}
	im.met.StreamResult(n.Partner, "ok", metrics.Target{})
	im.met.StreamStarted(n.Partner, metrics.Target{})
	defer im.met.StreamEnded(n.Partner, metrics.Target{})
	peer.Pipe(c, s, func(dir string, b int64) { im.met.AddBytes(n.Partner, dir, metrics.Target{}, b) })
}

func (im *Importer) reset(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	c.Close()
}

// serveUDP demultiplexes datagrams into one flow per client address (TRN-3).
func (im *Importer) serveUDP(u *net.UDPConn, n names.Name, port int) {
	type flow struct {
		s    net.Conn
		idle *peer.Idle
	}
	var mu sync.Mutex
	flows := map[netip.AddrPort]*flow{}
	buf := make([]byte, 65535)
	for {
		k, from, err := u.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		pkt := append([]byte(nil), buf[:k]...)
		mu.Lock()
		f := flows[from]
		mu.Unlock()
		if f == nil {
			s, st, err := im.open.Open(context.Background(), n.Partner, im.target(n, port, wire.UDP))
			if err != nil || st != wire.StatusOK {
				if s != nil {
					s.Close()
				}
				continue
			}
			f = &flow{s: s, idle: peer.WatchIdle(s)}
			mu.Lock()
			flows[from] = f
			mu.Unlock()
			if n.Partner != config.LocalPartner {
				im.met.StreamResult(n.Partner, "ok", metrics.Target{})
				im.met.StreamStarted(n.Partner, metrics.Target{})
			}
			go func(from netip.AddrPort, f *flow) {
				defer func() {
					f.idle.Stop()
					f.s.Close()
					mu.Lock()
					delete(flows, from)
					mu.Unlock()
					if n.Partner != config.LocalPartner {
						im.met.StreamEnded(n.Partner, metrics.Target{})
					}
				}()
				rb := make([]byte, 65535)
				for {
					m, err := im.readUDP(f.s, n.Partner == config.LocalPartner, rb)
					if err != nil {
						return
					}
					f.idle.Touch()
					if _, err := u.WriteToUDPAddrPort(rb[:m], from); err != nil {
						return
					}
					if n.Partner != config.LocalPartner {
						im.met.AddBytes(n.Partner, "in", metrics.Target{}, int64(m))
					}
				}
			}(from, f)
		}
		f.idle.Touch()
		if err := im.writeUDP(f.s, n.Partner == config.LocalPartner, pkt); err != nil {
			f.s.Close()
			continue
		}
		if n.Partner != config.LocalPartner {
			im.met.AddBytes(n.Partner, "out", metrics.Target{}, int64(len(pkt)))
		}
	}
}

// A local UDP target is a plain connected socket; a partner stream carries framed datagrams.
func (im *Importer) readUDP(s net.Conn, local bool, b []byte) (int, error) {
	if local {
		return s.Read(b)
	}
	return wire.ReadDatagram(s, b)
}

func (im *Importer) writeUDP(s net.Conn, local bool, p []byte) error {
	if local {
		_, err := s.Write(p)
		return err
	}
	return wire.WriteDatagram(s, p)
}
