//go:build linux

package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
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

// tuning holds timers and hooks per importer, so tests can change them without races.
type tuning struct {
	expireEvery            time.Duration // release of quarantined short names
	npmPoll                time.Duration // retry for NPM directories that do not exist yet
	backoffMin, backoffMax time.Duration // event stream reconnect
	inotifyInit            func(int) (int, error)
}

func defaultTuning() tuning {
	return tuning{expireEvery: time.Minute, npmPoll: 5 * time.Second, backoffMin: time.Second,
		backoffMax: time.Minute, inotifyInit: inotifyInit1}
}

// Importer is the import side on Linux.
type Importer struct {
	log  *slog.Logger
	met  *metrics.Registry
	open Opener
	npm  *npmconf.Store
	fwd  *Forwarder

	// fallback is set when the forwarder uses 127.0.0.11; ownNet then holds the names Docker
	// would answer from the agent's networks, answered NXDOMAIN instead (DNS-5). Guarded by mu.
	fallback bool
	ownNet   map[string]bool

	Own     netip.Addr
	bits    int
	ifIndex int

	tune tuning

	dc        *docker.Client
	selfID    string
	networkID string
	network   string

	mu        sync.Mutex
	table     *Table
	listeners map[netip.AddrPort]*listenerPair

	npmMu   sync.Mutex
	npmSeen map[string][]int // short names of the last NPM read, for the log
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
		tune:      defaultTuning(),
		dc:        dc,
		selfID:    self.ID,
		networkID: ep.NetworkID,
		network:   cfg.ImportNetwork,
	}
	servers, skipped := ExtServers("/etc/resolv.conf", nw.EnableIPv6)
	log.Debug("upstream dns", "servers", servers, "skipped", skipped)
	if len(servers) == 0 {
		servers = []string{"127.0.0.11:53"}
		im.fallback = true
		im.ownNet = ownNames(ctx, dc, self)
		log.Info("no reachable upstream DNS server, using 127.0.0.11")
	}
	im.fwd = NewForwarder(servers, log)
	met.SetAddressesFree(pool.Free())
	log.Debug("import network", "network", cfg.ImportNetwork, "subnet", subnet, "ip_range", ipRange,
		"dns", own, "addresses_free", pool.Free())
	return im, nil
}

// ownNames collects the names Docker's resolver would answer from the agent's networks (DNS-5).
func ownNames(ctx context.Context, dc *docker.Client, self docker.Container) map[string]bool {
	out := map[string]bool{}
	cs, err := dc.Running(ctx)
	if err != nil {
		return out
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
				out[n] = true
				out[n+"."+nw] = true
			}
		}
	}
	return out
}

// refreshOwnNames re-reads the DNS-5 names after containers came or went.
func (im *Importer) refreshOwnNames(ctx context.Context) {
	self, err := im.dc.Inspect(ctx, im.selfID)
	if err != nil {
		return
	}
	m := ownNames(ctx, im.dc, self)
	im.mu.Lock()
	im.ownNet = m
	im.mu.Unlock()
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
		im.reloadNPM()
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
	t := time.NewTicker(im.tune.expireEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			im.expire(time.Now())
		}
	}
}

// expire releases quarantined short names whose hour is over and drops their aliases (IMP-7).
func (im *Importer) expire(now time.Time) {
	im.mu.Lock()
	defer im.mu.Unlock()
	for _, a := range im.table.Expire(now) {
		im.unalias(a)
		im.log.Info("quarantine over, address released", "addr", a)
	}
	im.met.SetAddressesFree(im.table.Free())
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
			if failed := im.listenPorts(e, added); len(failed) > 0 {
				im.table.Undo(e, false, failed)
			}
		}
	}
}

// reloadNPM re-reads NPM's files and logs the short names that came, went or changed ports.
func (im *Importer) reloadNPM() {
	im.npm.Reload()
	cur := im.npm.Names()
	im.npmMu.Lock()
	defer im.npmMu.Unlock()
	for _, n := range slices.Sorted(maps.Keys(cur)) {
		old, ok := im.npmSeen[n]
		switch {
		case !ok:
			im.log.Debug("NPM short name added", "name", n, "ports", cur[n])
		case !slices.Equal(old, cur[n]):
			im.log.Debug("NPM short name ports changed", "name", n, "ports", cur[n], "before", old)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(im.npmSeen)) {
		if _, ok := cur[n]; !ok {
			im.log.Debug("NPM short name removed", "name", n)
		}
	}
	im.npmSeen = cur
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
	log := im.log.With("name", qn, "type", dns.TypeToString[q.Question[0].Qtype], "client", w.RemoteAddr())
	e, isNew, reason := im.resolve(qn)
	if e == nil {
		log.Debug("dns", "result", "NXDOMAIN", "reason", reason)
		m.Rcode = dns.RcodeNameError
		m.Ns = []dns.RR{soa(ttlNegative)}
		_ = w.WriteMsg(m)
		return
	}
	log.Debug("dns", "result", "NOERROR", "addr", e.Addr, "new", isNew, "ports", slices.Sorted(maps.Keys(e.Ports)))
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
	n := strings.ToLower(strings.TrimSuffix(q.Question[0].Name, "."))
	im.mu.Lock()
	own := im.ownNet[n]
	im.mu.Unlock()
	if own {
		im.log.Debug("dns", "name", q.Question[0].Name, "result", "NXDOMAIN",
			"reason", "container in the agent's networks, not reachable for the client")
		m := new(dns.Msg)
		m.SetRcode(q, dns.RcodeNameError)
		_ = w.WriteMsg(m)
		return
	}
	_, tcp := w.RemoteAddr().(*net.TCPAddr)
	r := im.fwd.Exchange(q, tcp)
	if r == nil {
		im.log.Debug("dns", "name", q.Question[0].Name, "result", "SERVFAIL", "reason", "no upstream server answered")
		m := new(dns.Msg)
		m.SetRcode(q, dns.RcodeServerFailure)
		_ = w.WriteMsg(m)
		return
	}
	r.Id = q.Id
	_ = w.WriteMsg(r)
}

// resolve validates a name and assigns its address (NAM-*, IMP-3, IMP-6). isNew means the name
// got its address or ports just now; reason says why a name is refused (e is nil).
func (im *Importer) resolve(qn string) (e *Entry, isNew bool, reason string) {
	n, ok := names.Parse(qn)
	if !ok {
		return nil, false, "not a valid switchyard name"
	}
	if !im.open.Known(n.Partner) {
		return nil, false, "unknown partner"
	}
	key := n.String()
	if n.Short() {
		if !im.npm.Mounted() {
			return nil, false, "short name, but NPM files are not mounted"
		}
		ports := im.npm.Ports(key)
		if ports == nil {
			im.reloadNPM() // NAM-6: nginx -t may be ahead of inotify
			ports = im.npm.Ports(key)
		}
		if ports == nil {
			return nil, false, "short name not used in NPM"
		}
		im.mu.Lock()
		defer im.mu.Unlock()
		e, newAddr, added, err := im.table.Short(key, ports)
		if err != nil {
			im.full(key)
			return nil, false, "no free address"
		}
		if !im.setUp(e, newAddr, added) {
			return nil, false, "address setup failed"
		}
		im.met.SetAddressesFree(im.table.Free())
		return e, newAddr || len(added) > 0, ""
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	if e, ok := im.table.Lookup(key); ok {
		return e, false, ""
	}
	e, newAddr, err := im.table.Long(key, n.Port)
	if err != nil {
		im.full(key)
		return nil, false, "no free address"
	}
	if !im.setUp(e, newAddr, []int{n.Port}) {
		return nil, false, "address setup failed"
	}
	im.met.SetAddressesFree(im.table.Free())
	return e, true, ""
}

// setUp adds a new entry's alias and the listeners of ports. On failure the entry (or only the
// failed ports) is taken back, so the name is not answered and the next query tries again.
// Caller holds im.mu.
func (im *Importer) setUp(e *Entry, newAddr bool, ports []int) bool {
	if newAddr && !im.alias(e) {
		im.table.Undo(e, true, nil)
		return false
	}
	failed := im.listenPorts(e, ports)
	if len(failed) == 0 {
		return true
	}
	if !newAddr {
		im.table.Undo(e, false, failed)
		return false
	}
	for _, p := range ports {
		im.closeListener(netip.AddrPortFrom(e.Addr, uint16(p)))
	}
	im.unalias(e.Addr)
	im.table.Undo(e, true, nil)
	return false
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

func (im *Importer) unalias(a netip.Addr) {
	if err := delAlias(im.ifIndex, a.AsSlice(), im.bits); err != nil {
		im.log.Error("remove address", "addr", a, "err", err)
	}
}

// listenPorts opens TCP and UDP listeners for an entry's ports (IMP-5) and returns the ports
// that could not be opened. Caller holds im.mu.
func (im *Importer) listenPorts(e *Entry, ports []int) (failed []int) {
	for _, p := range ports {
		ap := netip.AddrPortFrom(e.Addr, uint16(p))
		if im.listeners[ap] != nil {
			continue
		}
		tl, err := net.Listen("tcp", ap.String())
		if err != nil {
			im.log.Error("listen tcp", "addr", ap, "name", e.Name, "err", err)
			failed = append(failed, p)
			continue
		}
		ul, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(ap))
		if err != nil {
			tl.Close()
			im.log.Error("listen udp", "addr", ap, "name", e.Name, "err", err)
			failed = append(failed, p)
			continue
		}
		im.listeners[ap] = &listenerPair{tcp: tl, udp: ul}
		n, _ := names.Parse(e.Name)
		go im.acceptTCP(tl, n, p)
		go im.serveUDP(ul, n, p)
		im.log.Debug("listening", "addr", ap, "name", e.Name)
	}
	return failed
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

// streamID is the ID both agents log for a stream (LOG-3).
func streamID(s net.Conn) string {
	if x, ok := s.(interface{ StreamID() string }); ok {
		return x.StreamID()
	}
	return ""
}

func (im *Importer) serveTCP(c net.Conn, n names.Name, port int) {
	ctx := context.Background()
	start := time.Now()
	log := im.log.With("name", n.String(), "port", port, "proto", "tcp", "client", c.RemoteAddr())
	s, st, err := im.open.Open(ctx, n.Partner, im.target(n, port, wire.TCP))
	if err != nil || st != wire.StatusOK {
		if s != nil {
			log = log.With("stream", streamID(s))
			s.Close()
		}
		im.reset(c)
		im.countRefused(n, err)
		log.Debug("stream refused", "status", wire.StatusReason(st), "err", err)
		return
	}
	log = log.With("stream", streamID(s))
	log.Debug("stream opened")
	var bytes peer.Bytes
	defer func() {
		log.Debug("stream closed", "duration", time.Since(start).Round(time.Millisecond),
			"bytes_in", bytes.In.Load(), "bytes_out", bytes.Out.Load())
	}()
	if n.Partner == config.LocalPartner {
		peer.Pipe(c, s, bytes.Add)
		return
	}
	im.met.StreamResult(n.Partner, "ok", metrics.Target{})
	im.met.StreamStarted(n.Partner, metrics.Target{})
	defer im.met.StreamEnded(n.Partner, metrics.Target{})
	peer.Pipe(c, s, func(dir string, b int64) {
		im.met.AddBytes(n.Partner, dir, metrics.Target{}, b)
		bytes.Add(dir, b)
	})
}

// countRefused counts a refused or failed stream on the importing side (MET-2); local
// streams are counted by the exporter.
func (im *Importer) countRefused(n names.Name, err error) {
	if n.Partner == config.LocalPartner {
		return
	}
	res := "refused"
	if err != nil {
		res = "error"
	}
	im.met.StreamResult(n.Partner, res, metrics.Target{})
}

func (im *Importer) reset(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	c.Close()
}

// udpFlow is one client's flow (TRN-3). Datagrams that arrive while its stream is being
// opened wait in queue, so one slow open does not stall the other flows of the port.
type udpFlow struct {
	mu    sync.Mutex
	s     net.Conn // nil until the stream is open
	idle  *peer.Idle
	bytes *peer.Bytes // set with s
	queue [][]byte
	dead  bool
}

// udpQueue bounds the datagrams kept per flow while its stream opens.
const udpQueue = 16

// serveUDP demultiplexes datagrams into one flow per client address (TRN-3).
func (im *Importer) serveUDP(u *net.UDPConn, n names.Name, port int) {
	var mu sync.Mutex
	flows := map[netip.AddrPort]*udpFlow{}
	buf := make([]byte, 65535)
	for {
		k, from, err := u.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		pkt := append([]byte(nil), buf[:k]...)
		mu.Lock()
		f := flows[from]
		if f == nil {
			f = &udpFlow{}
			flows[from] = f
			go im.runFlow(u, n, port, from, f, func() {
				mu.Lock()
				delete(flows, from)
				mu.Unlock()
			})
		}
		mu.Unlock()
		im.flowSend(f, n, pkt)
	}
}

// flowSend writes a datagram to the flow's stream, or queues it until the stream is open.
func (im *Importer) flowSend(f *udpFlow, n names.Name, pkt []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead {
		return
	}
	if f.s == nil {
		if len(f.queue) < udpQueue {
			f.queue = append(f.queue, pkt)
		}
		return
	}
	im.flowWrite(f, n, pkt)
}

// flowWrite sends one datagram on an open flow; a failed send ends the flow. Caller holds f.mu.
func (im *Importer) flowWrite(f *udpFlow, n names.Name, pkt []byte) {
	f.idle.Touch()
	if err := im.writeUDP(f.s, n.Partner == config.LocalPartner, pkt); err != nil {
		f.s.Close() // the reader ends and removes the flow
		f.dead = true
		return
	}
	f.bytes.Add("out", int64(len(pkt)))
	if n.Partner != config.LocalPartner {
		im.met.AddBytes(n.Partner, "out", metrics.Target{}, int64(len(pkt)))
	}
}

// runFlow opens the flow's stream, sends what waited and relays replies until the flow ends.
func (im *Importer) runFlow(u *net.UDPConn, n names.Name, port int, from netip.AddrPort, f *udpFlow, remove func()) {
	local := n.Partner == config.LocalPartner
	start := time.Now()
	log := im.log.With("name", n.String(), "port", port, "proto", "udp", "client", from)
	s, st, err := im.open.Open(context.Background(), n.Partner, im.target(n, port, wire.UDP))
	if err != nil || st != wire.StatusOK {
		if s != nil {
			log = log.With("stream", streamID(s))
			s.Close()
		}
		f.mu.Lock()
		f.dead, f.queue = true, nil
		f.mu.Unlock()
		remove()
		im.countRefused(n, err)
		log.Debug("stream refused", "status", wire.StatusReason(st), "err", err)
		return
	}
	log = log.With("stream", streamID(s))
	log.Debug("stream opened")
	idle := peer.WatchIdle(s)
	if !local {
		im.met.StreamResult(n.Partner, "ok", metrics.Target{})
		im.met.StreamStarted(n.Partner, metrics.Target{})
	}
	f.mu.Lock()
	f.s, f.idle, f.bytes = s, idle, &peer.Bytes{}
	for _, pkt := range f.queue {
		if f.dead {
			break
		}
		im.flowWrite(f, n, pkt)
	}
	f.queue = nil
	f.mu.Unlock()
	defer func() {
		idle.Stop()
		s.Close()
		f.mu.Lock()
		f.dead = true
		f.mu.Unlock()
		remove()
		if !local {
			im.met.StreamEnded(n.Partner, metrics.Target{})
		}
		log.Debug("stream closed", "duration", time.Since(start).Round(time.Millisecond),
			"bytes_in", f.bytes.In.Load(), "bytes_out", f.bytes.Out.Load())
	}()
	rb := make([]byte, 65535)
	for {
		m, err := im.readUDP(s, local, rb)
		if err != nil {
			return
		}
		idle.Touch()
		if _, err := u.WriteToUDPAddrPort(rb[:m], from); err != nil {
			return
		}
		f.bytes.Add("in", int64(m))
		if !local {
			im.met.AddBytes(n.Partner, "in", metrics.Target{}, int64(m))
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
