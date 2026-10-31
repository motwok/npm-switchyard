//go:build linux

package importer

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/docker/dockertest"
	"github.com/motwok/npm-switchyard/internal/metrics"
	"github.com/motwok/npm-switchyard/internal/names"
	"github.com/motwok/npm-switchyard/internal/npmconf"
	"github.com/motwok/npm-switchyard/internal/wire"
)

// echoOpener connects every stream to a local TCP or UDP echo server.
type echoOpener struct{ tcp, udp string }

func (o echoOpener) Known(p string) bool { return p == "b" || p == "local" }

func (o echoOpener) Open(_ context.Context, p string, t wire.Target) (net.Conn, byte, error) {
	if t.Name != "web" {
		return nil, wire.StatusNoTarget, nil
	}
	if t.Proto == wire.UDP && p == "local" {
		c, err := net.Dial("udp", o.udp)
		return c, wire.StatusOK, err
	}
	// A partner stream carries framed datagrams; a TCP echo returns the frames unchanged.
	c, err := net.Dial("tcp", o.tcp)
	return c, wire.StatusOK, err
}

func echoes(t *testing.T) (string, string) {
	t.Helper()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	u, _ := net.ListenPacket("udp", "127.0.0.1:0")
	t.Cleanup(func() { l.Close(); u.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	go func() {
		b := make([]byte, 2048)
		for {
			n, a, err := u.ReadFrom(b)
			if err != nil {
				return
			}
			_, _ = u.WriteTo(b[:n], a)
		}
	}()
	return l.Addr().String(), u.LocalAddr().String()
}

type writer struct{ msg *dns.Msg }

func (w *writer) LocalAddr() net.Addr         { return &net.UDPAddr{} }
func (w *writer) RemoteAddr() net.Addr        { return &net.UDPAddr{} }
func (w *writer) WriteMsg(m *dns.Msg) error   { w.msg = m; return nil }
func (w *writer) Write(b []byte) (int, error) { return len(b), nil }
func (w *writer) Close() error                { return nil }
func (w *writer) TsigStatus() error           { return nil }
func (w *writer) TsigTimersOnly(bool)         {}
func (w *writer) Hijack()                     {}

func query(im *Importer, name string, qt uint16) *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), qt)
	w := &writer{}
	im.handle(w, q)
	return w.msg
}

// freePort finds a port that is free on 127.0.0.1 for TCP and UDP.
func freePort(t *testing.T) int {
	t.Helper()
	for range 20 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if u, err := net.ListenPacket("udp", l.Addr().String()); err == nil {
			u.Close()
			return p
		}
	}
	t.Fatal("no free port")
	return 0
}

func newImporter(t *testing.T) *Importer {
	t.Helper()
	tcp, udp := echoes(t)
	own := netip.MustParseAddr("127.0.0.1")
	pool, _ := NewPool(netip.MustParsePrefix("127.0.0.0/24"), netip.MustParsePrefix("127.0.0.128/25"),
		[]netip.Addr{own})
	im := &Importer{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		met:       metrics.New(),
		open:      echoOpener{tcp, udp},
		npm:       npmconf.NewStore(t.TempDir()),
		fwd:       NewForwarder([]string{server(t, dns.RcodeSuccess)}),
		ownNet:    map[string]bool{"db": true},
		Own:       own,
		bits:      24,
		table:     NewTable(pool, own, []int{53}),
		listeners: map[netip.AddrPort]*listenerPair{},
		tune:      defaultTuning(),
	}
	t.Cleanup(func() {
		im.mu.Lock()
		for ap := range im.listeners {
			im.closeListener(ap)
		}
		im.mu.Unlock()
	})
	return im
}

func TestDNSAnswers(t *testing.T) {
	im := newImporter(t)
	port := freePort(t)
	name := "web." + strconv.Itoa(port) + ".b.switchyard"

	r := query(im, name, dns.TypeA)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "127.0.0.1" ||
		r.Answer[0].Header().Ttl != 60 {
		t.Fatalf("A: %v", r)
	}
	if r := query(im, "WEB."+strconv.Itoa(port)+".B.switchyard", dns.TypeA); r.Answer[0].(*dns.A).A.String() != "127.0.0.1" {
		t.Fatal("case")
	}
	if r := query(im, name, dns.TypeAAAA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Fatalf("AAAA: %v", r)
	}
	if r := query(im, "web.80.unknown.switchyard", dns.TypeA); r.Rcode != dns.RcodeNameError || r.Ns[0].Header().Ttl != 5 {
		t.Fatalf("unknown partner: %v", r)
	}
	if r := query(im, "web.b.switchyard", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("short name not in NPM: %v", r)
	}
	if r := query(im, "example.org", dns.TypeA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Fatalf("forwarded: %v", r)
	}
	if r := query(im, "db", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("own container name: %v", r)
	}
	two := new(dns.Msg)
	two.Question = []dns.Question{{Name: "a.", Qtype: dns.TypeA}, {Name: "b.", Qtype: dns.TypeA}}
	w := &writer{}
	im.handle(w, two)
	if w.msg.Rcode != dns.RcodeFormatError {
		t.Fatal("two questions")
	}
}

func TestForwardFailure(t *testing.T) {
	im := newImporter(t)
	im.fwd = NewForwarder([]string{server(t, dns.RcodeServerFailure)})
	if r := query(im, "example.org", dns.TypeA); r.Rcode != dns.RcodeServerFailure {
		t.Fatalf("%v", r)
	}
}

func TestTCPAndUDPStreams(t *testing.T) {
	im := newImporter(t)
	for _, p := range []string{"b", "local"} { // partner stream and own host
		port := freePort(t)
		query(im, "web."+strconv.Itoa(port)+"."+p+".switchyard", dns.TypeA)
		addr := "127.0.0.1:" + strconv.Itoa(port)

		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(p, err)
		}
		_, _ = c.Write([]byte("tcp"))
		b := make([]byte, 3)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, b); err != nil || string(b) != "tcp" {
			t.Fatal(p, err, string(b))
		}
		c.Close()

		u, err := net.Dial("udp", addr)
		if err != nil {
			t.Fatal(p, err)
		}
		_, _ = u.Write([]byte("udp"))
		_ = u.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := u.Read(b)
		u.Close()
		if err != nil || string(b[:n]) != "udp" {
			t.Fatal(p, err, string(b[:n]))
		}
	}
}

func TestRefusedStreamResets(t *testing.T) {
	im := newImporter(t)
	port := freePort(t)
	query(im, "nope."+strconv.Itoa(port)+".b.switchyard", dns.TypeA)
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return // reset before the connect completed
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("refused stream not closed")
	}
}

func TestSyncShortFreesGoneNames(t *testing.T) {
	im := newImporter(t)
	// A short name already assigned (on the own address, no alias needed in the test).
	port := freePort(t)
	im.mu.Lock()
	e := &Entry{Name: "n.b.switchyard", Addr: im.Own, Ports: map[int]bool{port: true}, Short: true}
	im.table.names[e.Name] = e
	im.listenPorts(e, []int{port})
	im.mu.Unlock()
	im.syncShort() // NPM store is empty: the name goes to quarantine, its listeners close
	im.mu.Lock()
	_, ok := im.table.Lookup("n.b.switchyard")
	n := len(im.listeners)
	im.mu.Unlock()
	if ok || n != 0 {
		t.Fatal("gone short name still active", ok, n)
	}
}

// A full range gives NXDOMAIN; with free addresses but no NET_ADMIN the alias fails and the
// name is not answered either (as root in CI images it succeeds).
func TestRangeFullAndAlias(t *testing.T) {
	im := newImporter(t)
	port := freePort(t)
	query(im, "a."+strconv.Itoa(port)+".b.switchyard", dns.TypeA)
	r := query(im, "c."+strconv.Itoa(port)+".b.switchyard", dns.TypeA) // collision: needs an alias
	if r.Rcode != dns.RcodeNameError && r.Rcode != dns.RcodeSuccess {
		t.Fatalf("collision: %v", r)
	}

	full := newImporter(t)
	empty, _ := NewPool(netip.MustParsePrefix("127.0.1.0/30"), netip.MustParsePrefix("127.0.1.0/30"), nil)
	full.table = NewTable(empty, full.Own, nil)
	p2 := freePort(t)
	if r := query(full, "a."+strconv.Itoa(p2)+".b.switchyard", dns.TypeA); r.Rcode != dns.RcodeSuccess {
		t.Fatalf("own address: %v", r)
	}
	if r := query(full, "c."+strconv.Itoa(p2)+".b.switchyard", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("full: %v", r)
	}
}

// A port that cannot be listened on is not answered; once it is free the name works.
func TestPortInUse(t *testing.T) {
	im := newImporter(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	name := "x." + strconv.Itoa(port) + ".b.switchyard"
	if r := query(im, name, dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("%v", r)
	}
	l.Close()
	if r := query(im, name, dns.TypeA); r.Rcode != dns.RcodeSuccess {
		t.Fatalf("free port not answered: %v", r)
	}
	u, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if r := query(im, "y."+strconv.Itoa(u.LocalAddr().(*net.UDPAddr).Port)+".b.switchyard", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("udp port in use answered: %v", r)
	}
}

func TestExpireLoop(t *testing.T) {
	im := newImporter(t)
	im.tune.expireEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { im.expireLoop(ctx); close(done) }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done
}

func TestWatchNPMAndShortNames(t *testing.T) {
	im := newImporter(t)
	dir := t.TempDir()
	im.npm = npmconf.NewStore(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go im.watchNPM(ctx) // directories do not exist yet
	ph := filepath.Join(dir, "nginx", "proxy_host")
	if err := os.MkdirAll(ph, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "nginx", "stream"), 0o755)
	port := freePort(t)
	conf := "set $server \"web.b.switchyard\";\nset $port " + strconv.Itoa(port) + ";\n"
	_ = os.WriteFile(filepath.Join(ph, "1.conf"), []byte(conf), 0o644)
	// The short name needs its own address (alias); without NET_ADMIN it is not answered, but
	// the store must know it after the directories appear.
	deadline := time.Now().Add(10 * time.Second)
	for im.npm.Ports("web.b.switchyard") == nil {
		if time.Now().After(deadline) {
			t.Fatal("NPM file not picked up")
		}
		time.Sleep(100 * time.Millisecond)
	}
	query(im, "web.b.switchyard", dns.TypeA)
	_ = os.Remove(filepath.Join(ph, "1.conf"))
	time.Sleep(500 * time.Millisecond)
}

func TestWatchMembers(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	im := newImporter(t)
	im.dc, im.networkID, im.network, im.selfID = f.Client(), "n-imp", "imp", "self"
	im.fallback = true // container events refresh the DNS-5 names too
	f.Set(dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"imp": {NetworkID: "n-imp", IPAddress: "127.0.0.1"}}))
	f.Set(dockertest.Ctr("late", "late", nil, map[string]docker.Endpoint{"imp": {NetworkID: "n-imp", IPAddress: "127.0.0.1"}}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { im.watchMembers(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	f.Emit(docker.Event{Type: "network", Action: "connect", Actor: struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	}{ID: "n-imp", Attributes: map[string]string{"container": "late"}}})
	f.Emit(docker.Event{Type: "container", Action: "start"})
	time.Sleep(200 * time.Millisecond)
	im.mu.Lock()
	own := im.ownNet["late.imp"]
	im.mu.Unlock()
	if !own {
		t.Fatal("DNS-5 names not refreshed on the event")
	}
	f.CloseEvents()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	im.joined(context.Background(), "missing")
}

func TestSetup(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{ImportNetwork: "imp", Listen: 9481, Metrics: 9480, NPMData: t.TempDir()}
	op := echoOpener{}
	ctx := context.Background()
	setNet := func(ipam docker.IPAMConfig) {
		n := docker.Network{ID: "n-imp", Name: "imp"}
		n.IPAM.Config = []docker.IPAMConfig{ipam}
		n.Containers = map[string]struct {
			Name        string `json:"Name"`
			IPv4Address string `json:"IPv4Address"`
		}{"other": {Name: "npm", IPv4Address: "127.0.0.5/8"}}
		f.Networks["n-imp"] = n
	}
	self := func(ip string) {
		f.Set(dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"imp": {NetworkID: "n-imp", IPAddress: ip}}))
	}

	if _, err := Setup(ctx, f.Client(), "self", cfg, op, metrics.New(), log); err == nil {
		t.Fatal("missing own container accepted")
	}
	f.Set(dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"other": {NetworkID: "x"}}))
	if _, err := Setup(ctx, f.Client(), "self", cfg, op, metrics.New(), log); err == nil {
		t.Fatal("agent outside the import network accepted")
	}
	self("127.0.0.1")
	if _, err := Setup(ctx, f.Client(), "self", cfg, op, metrics.New(), log); err == nil {
		t.Fatal("missing network accepted")
	}
	for _, bad := range []docker.IPAMConfig{
		{Subnet: "fd00::/64"},
		{Subnet: "127.0.0.0/8"},
		{Subnet: "127.0.0.0/8", IPRange: "nonsense"},
	} {
		setNet(bad)
		if _, err := Setup(ctx, f.Client(), "self", cfg, op, metrics.New(), log); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	setNet(docker.IPAMConfig{Subnet: "127.0.0.0/24", IPRange: "127.0.0.128/25", Gateway: "127.0.0.129",
		AuxAddress: map[string]string{"dev": "127.0.0.9"}})
	self("bad-ip")
	if _, err := Setup(ctx, f.Client(), "self", cfg, op, metrics.New(), log); err == nil {
		t.Fatal("invalid own IP accepted")
	}
	self("127.0.0.77") // on no interface
	if _, err := Setup(ctx, f.Client(), "self", cfg, op, metrics.New(), log); err == nil {
		t.Fatal("own IP on no interface accepted")
	}
	self("127.0.0.1")
	im, err := Setup(ctx, f.Client(), "self", cfg, op, metrics.New(), log)
	if err != nil {
		t.Fatal(err)
	}
	if im.Own.String() != "127.0.0.1" || im.network != "imp" {
		t.Fatal(im.Own, im.network)
	}
}

// errOpener fails every stream with an error.
type errOpener struct{}

func (errOpener) Known(string) bool { return true }
func (errOpener) Open(context.Context, string, wire.Target) (net.Conn, byte, error) {
	return nil, 0, io.ErrClosedPipe
}

func TestOpenErrorsAndRefusedUDP(t *testing.T) {
	im := newImporter(t)
	im.open = errOpener{}
	port := freePort(t)
	query(im, "x."+strconv.Itoa(port)+".b.switchyard", dns.TypeA)
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err == nil {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Read(make([]byte, 1))
		c.Close()
	}
	u, _ := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
	_, _ = u.Write([]byte("x"))
	u.Close()

	im2 := newImporter(t) // refused (no error) for UDP
	p2 := freePort(t)
	query(im2, "nope."+strconv.Itoa(p2)+".b.switchyard", dns.TypeA)
	u2, _ := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(p2))
	_, _ = u2.Write([]byte("x"))
	time.Sleep(100 * time.Millisecond)
	u2.Close()
}

func TestShortNameRangeFull(t *testing.T) {
	im := newImporter(t)
	dir := t.TempDir()
	ph := filepath.Join(dir, "nginx", "proxy_host")
	_ = os.MkdirAll(ph, 0o755)
	_ = os.WriteFile(filepath.Join(ph, "1.conf"), []byte("set $server \"s.b.switchyard\";\nset $port 1;\n"), 0o644)
	im.npm = npmconf.NewStore(dir)
	empty, _ := NewPool(netip.MustParsePrefix("127.0.1.0/30"), netip.MustParsePrefix("127.0.1.0/30"), nil)
	im.table = NewTable(empty, im.Own, nil)
	if r := query(im, "s.b.switchyard", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("%v", r)
	}
}

func TestListenTwice(t *testing.T) {
	im := newImporter(t)
	port := freePort(t)
	e := &Entry{Name: "x." + strconv.Itoa(port) + ".b.switchyard", Addr: im.Own, Ports: map[int]bool{}}
	im.mu.Lock()
	im.listenPorts(e, []int{port})
	im.listenPorts(e, []int{port}) // already listening: no second listener
	n := len(im.listeners)
	im.mu.Unlock()
	if n != 1 {
		t.Fatal(n)
	}
}

func TestServeAndOwnNames(t *testing.T) {
	im := newImporter(t)
	f := dockertest.NewFake()
	defer f.Close()
	im.dc, im.networkID, im.network = f.Client(), "n-imp", "imp"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ready := make(chan struct{})
	im.npm = npmconf.NewStore(filepath.Join(t.TempDir(), "missing")) // short names off
	// Port 53 needs root; as a normal user Serve fails, as root it serves until ctx ends.
	_ = im.Serve(ctx, ready)
	cancel()

	f.Set(dockertest.Ctr("c1", "db", nil, map[string]docker.Endpoint{"n": {NetworkID: "n1", DNSNames: []string{"db"}}}))
	f.Set(dockertest.Ctr("c2", "elsewhere", nil, map[string]docker.Endpoint{"x": {NetworkID: "nx", DNSNames: []string{"elsewhere"}}}))
	self := dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"n": {NetworkID: "n1"}})
	f.Set(self)
	own := ownNames(context.Background(), f.Client(), self)
	if !own["db"] || !own["db.n"] {
		t.Fatal(own)
	}
	dead := docker.NewWithDialer(func(context.Context) (net.Conn, error) { return nil, io.EOF })
	if len(ownNames(context.Background(), dead, self)) != 0 {
		t.Fatal("names from a dead daemon")
	}

	// The list follows containers that come and go (DNS-5), read again on request.
	im.fallback, im.selfID = true, "self"
	im.ownNet = map[string]bool{}
	im.refreshOwnNames(context.Background())
	if r := query(im, "db.n", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("new container not NXDOMAIN: %v", r)
	}
	f.Remove("c1")
	im.refreshOwnNames(context.Background())
	if r := query(im, "db.n", dns.TypeA); r.Rcode != dns.RcodeSuccess {
		t.Fatalf("gone container still NXDOMAIN: %v", r)
	}
	im.dc = dead
	im.refreshOwnNames(context.Background()) // unreachable daemon: list kept
}

// A failed alias takes the assignment back: the name stays NXDOMAIN instead of answering an
// address nothing listens on, and the address returns to the pool.
func TestAliasFailureRollsBack(t *testing.T) {
	im := newImporter(t)
	port := freePort(t)
	query(im, "a."+strconv.Itoa(port)+".b.switchyard", dns.TypeA) // own address
	free := im.table.Free()
	fakeNetlink(t, errors.New("no netlink"), nil, nil, nil, nil)
	name := "c." + strconv.Itoa(port) + ".b.switchyard"
	for range 2 {
		if r := query(im, name, dns.TypeA); r.Rcode != dns.RcodeNameError {
			t.Fatalf("%v", r)
		}
		if im.table.Free() != free || len(im.table.longIPs) != 1 {
			t.Fatal("assignment kept after a failed alias")
		}
	}
	// The same for a short name, which always needs its own address.
	dir := t.TempDir()
	ph := filepath.Join(dir, "nginx", "proxy_host")
	_ = os.MkdirAll(ph, 0o755)
	_ = os.WriteFile(filepath.Join(ph, "1.conf"), []byte("set $server \"s.b.switchyard\";\nset $port "+strconv.Itoa(port)+";\n"), 0o644)
	im.npm = npmconf.NewStore(dir)
	if r := query(im, "s.b.switchyard", dns.TypeA); r.Rcode != dns.RcodeNameError || im.table.Free() != free {
		t.Fatalf("short: %v", r)
	}

	// The alias works now (fake ack): the names get the next addresses, with listeners.
	fakeNetlink(t, nil, nil, nil, nil, nlMsg(syscall.NLMSG_ERROR, 0))
	r := query(im, name, dns.TypeA)
	if r.Rcode != dns.RcodeSuccess || r.Answer[0].(*dns.A).A.String() != "127.0.0.2" {
		t.Fatalf("%v", r)
	}
	c, err := net.Dial("tcp", "127.0.0.2:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if r := query(im, "s.b.switchyard", dns.TypeA); r.Rcode != dns.RcodeSuccess || r.Answer[0].(*dns.A).A.String() != "127.0.0.3" {
		t.Fatalf("short: %v", r)
	}

	// Removing an alias can fail too; it is only logged.
	fakeNetlink(t, nil, nil, nil, nil, nlMsg(syscall.NLMSG_ERROR, -int32(syscall.EPERM)))
	im.unalias(netip.MustParseAddr("127.0.0.3"))
}

// A refused TCP stream that came with a connection closes it.
func TestRefusedTCPWithConn(t *testing.T) {
	im := newImporter(t)
	closed := make(chan struct{})
	a, b := net.Pipe()
	defer b.Close()
	im.open = funcOpener(func() (net.Conn, byte, error) {
		go func() { _, _ = b.Read(make([]byte, 1)); close(closed) }()
		return a, wire.StatusNoTarget, nil
	})
	port := freePort(t)
	query(im, "web."+strconv.Itoa(port)+".b.switchyard", dns.TypeA)
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err == nil {
		defer c.Close()
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("refused stream not closed")
	}
}

// Datagrams that arrive while the flow's stream opens wait, up to a limit, and are sent
// once it is open; the other flows of the port are not held up.
func TestUDPQueueWhileOpening(t *testing.T) {
	im := newImporter(t)
	release := make(chan struct{})
	first := make(chan struct{}, 1)
	a, b := net.Pipe()
	defer b.Close()
	im.open = funcOpener(func() (net.Conn, byte, error) {
		first <- struct{}{}
		<-release
		return a, wire.StatusOK, nil
	})
	port := freePort(t)
	query(im, "web."+strconv.Itoa(port)+".local.switchyard", dns.TypeA)
	u, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	for i := range udpQueue + 4 {
		_, _ = u.Write([]byte{byte(i)})
	}
	<-first
	time.Sleep(100 * time.Millisecond) // all datagrams read and queued
	close(release)
	got := make([]byte, 1)
	for i := range udpQueue {
		_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := b.Read(got); err != nil || got[0] != byte(i) {
			t.Fatalf("datagram %d: %v %v", i, got, err)
		}
	}
	_ = b.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := b.Read(got); err == nil {
		t.Fatal("datagram beyond the queue limit delivered")
	}

	// A dead flow drops what arrives.
	im.flowSend(&udpFlow{dead: true}, names.Name{Partner: "local"}, []byte("x"))
}

// A queued datagram that cannot be sent ends the flow; the rest of the queue is dropped.
func TestUDPQueueFlushFails(t *testing.T) {
	im := newImporter(t)
	release := make(chan struct{})
	first := make(chan struct{}, 1)
	a, b := net.Pipe()
	b.Close() // every write on a fails
	im.open = funcOpener(func() (net.Conn, byte, error) {
		first <- struct{}{}
		<-release
		return a, wire.StatusOK, nil
	})
	port := freePort(t)
	query(im, "web."+strconv.Itoa(port)+".local.switchyard", dns.TypeA)
	u, _ := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
	defer u.Close()
	_, _ = u.Write([]byte("1"))
	_, _ = u.Write([]byte("2"))
	<-first
	time.Sleep(100 * time.Millisecond)
	close(release)
	time.Sleep(100 * time.Millisecond) // the flow ends without a panic
}

// A partner flow is counted while open and ended when the stream closes.
func TestUDPPartnerFlowEnds(t *testing.T) {
	im := newImporter(t)
	a, b := net.Pipe()
	im.open = funcOpener(func() (net.Conn, byte, error) { return a, wire.StatusOK, nil })
	port := freePort(t)
	query(im, "web."+strconv.Itoa(port)+".b.switchyard", dns.TypeA)
	u, _ := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
	defer u.Close()
	_, _ = u.Write([]byte("x"))
	buf := make([]byte, 8)
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := wire.ReadDatagram(b, buf); err != nil || string(buf[:n]) != "x" {
		t.Fatal(n, err)
	}
	_ = wire.WriteDatagram(b, []byte("y"))
	_ = u.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := u.Read(buf); err != nil || string(buf[:n]) != "y" {
		t.Fatal(n, err)
	}
	_, _ = u.Write([]byte("z")) // on the open flow, no queue
	if n, err := wire.ReadDatagram(b, buf); err != nil || string(buf[:n]) != "z" {
		t.Fatal(n, err)
	}
	var sb strings.Builder
	_ = im.met.Write(&sb)
	if !strings.Contains(sb.String(), `switchyard_streams_active{peer="b"} 1`) {
		t.Fatal(sb.String())
	}
	b.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sb.Reset()
		_ = im.met.Write(&sb)
		if !strings.Contains(sb.String(), "streams_active{") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(sb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A directory that cannot be watched for another reason than "not there yet" is logged once.
func TestWatchNPMBadDir(t *testing.T) {
	im := newImporter(t)
	buf := &logBuf{}
	im.log = slog.New(slog.NewTextHandler(buf, nil))
	im.tune.npmPoll = 20 * time.Millisecond
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "nginx"), []byte("not a directory"), 0o644)
	im.npm = npmconf.NewStore(dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { im.watchNPM(ctx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	if n := strings.Count(buf.String(), "inotify watch"); n != 2 { // once per directory
		t.Fatal(n, buf.String())
	}
}

type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A new address whose listener cannot be opened is given back, alias included.
func TestNewAddressListenerFails(t *testing.T) {
	im := newImporter(t)
	port := freePort(t)
	query(im, "a."+strconv.Itoa(port)+".b.switchyard", dns.TypeA)
	busy, err := net.Listen("tcp", "127.0.0.2:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	free := im.table.Free()
	fakeNetlink(t, nil, nil, nil, nil, nlMsg(syscall.NLMSG_ERROR, 0))
	if r := query(im, "c."+strconv.Itoa(port)+".b.switchyard", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("%v", r)
	}
	if im.table.Free() != free || len(im.table.longIPs) != 1 || len(im.listeners) != 1 {
		t.Fatal("assignment kept after a failed listener")
	}
}

// A short name's new port that cannot be listened on is forgotten, so it is tried again.
func TestSyncShortListenerFails(t *testing.T) {
	im := newImporter(t)
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	bp := busy.Addr().(*net.TCPAddr).Port
	dir := t.TempDir()
	ph := filepath.Join(dir, "nginx", "proxy_host")
	_ = os.MkdirAll(ph, 0o755)
	_ = os.WriteFile(filepath.Join(ph, "1.conf"), []byte("set $server \"n.b.switchyard\";\nset $port "+strconv.Itoa(bp)+";\n"), 0o644)
	im.npm = npmconf.NewStore(dir)
	im.npm.Reload()
	im.mu.Lock()
	e := &Entry{Name: "n.b.switchyard", Addr: im.Own, Ports: map[int]bool{}, Short: true}
	im.table.names[e.Name] = e
	im.mu.Unlock()
	im.syncShort()
	if e.Ports[bp] {
		t.Fatal("port without listener kept")
	}
}

// An expired quarantine drops the alias and releases the address.
func TestExpireDropsAlias(t *testing.T) {
	im := newImporter(t)
	fakeNetlink(t, nil, nil, nil, nil, nlMsg(syscall.NLMSG_ERROR, 0))
	im.mu.Lock()
	e, _, _, _ := im.table.Short("n.b.switchyard", []int{80})
	im.table.FreeShort("n.b.switchyard", time.Now().Add(-2*Quarantine))
	free := im.table.Free()
	im.mu.Unlock()
	im.expire(time.Now())
	if _, ok := im.table.names[e.Name]; ok || im.table.Free() != free+1 {
		t.Fatal("quarantined name not released")
	}
}

func TestRefreshMembers(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	im := newImporter(t)
	im.dc, im.networkID, im.network, im.selfID = f.Client(), "n-imp", "imp", "self"
	n := docker.Network{ID: "n-imp", Name: "imp"}
	n.Containers = map[string]struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"`
	}{"c1": {Name: "late", IPv4Address: "127.0.0.9/24"}, "c2": {Name: "bad", IPv4Address: "nonsense"}}
	f.Networks["n-imp"] = n
	f.Set(dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"imp": {NetworkID: "n-imp", IPAddress: "127.0.0.1"}}))
	f.Set(dockertest.Ctr("c1", "late", nil, map[string]docker.Endpoint{"imp": {NetworkID: "n-imp", IPAddress: "127.0.0.9", DNSNames: []string{"late"}}}))
	free := im.table.Free()
	im.fallback = true
	im.refreshMembers(context.Background())
	if im.table.Free() != free-1 {
		t.Fatal("member address not reserved")
	}
	// A member with an address a name already uses: logged as a conflict.
	buf := &logBuf{}
	im.log = slog.New(slog.NewTextHandler(buf, nil))
	n.Containers["c3"] = struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"`
	}{Name: "clash", IPv4Address: "127.0.0.2/24"}
	f.Networks["n-imp"] = n
	im.mu.Lock()
	_, _, _, _ = im.table.Short("s.b.switchyard", []int{80}) // takes 127.0.0.2
	im.mu.Unlock()
	im.refreshMembers(context.Background())
	if !strings.Contains(buf.String(), "address the agent uses") {
		t.Fatal(buf.String())
	}
	im.mu.Lock()
	own := im.ownNet["late.imp"]
	im.mu.Unlock()
	if !own {
		t.Fatal("DNS-5 names not refreshed")
	}
	im.networkID = "missing"
	im.refreshMembers(context.Background()) // unknown network: nothing happens
}

func TestInotifyEvents(t *testing.T) {
	ev := func(wd int32, mask uint32, name string) []byte {
		b := make([]byte, syscall.SizeofInotifyEvent+len(name))
		binary.NativeEndian.PutUint32(b[0:], uint32(wd))
		binary.NativeEndian.PutUint32(b[4:], mask)
		binary.NativeEndian.PutUint32(b[12:], uint32(len(name)))
		copy(b[syscall.SizeofInotifyEvent:], name)
		return b
	}
	buf := append(ev(1, syscall.IN_CREATE, "1.conf\x00\x00"), ev(2, syscall.IN_IGNORED, "")...)
	buf = append(buf, 1, 2, 3) // trailing garbage is ignored
	got := inotifyEvents(buf)
	if len(got) != 2 || got[0].wd != 1 || got[0].mask != syscall.IN_CREATE || got[1].wd != 2 || got[1].mask != syscall.IN_IGNORED {
		t.Fatalf("%+v", got)
	}
}

// A watched NPM directory that is deleted and created again is watched again.
func TestWatchNPMDirRecreated(t *testing.T) {
	im := newImporter(t)
	im.tune.npmPoll = 20 * time.Millisecond
	dir := t.TempDir()
	im.npm = npmconf.NewStore(dir)
	ph := filepath.Join(dir, "nginx", "proxy_host")
	_ = os.MkdirAll(ph, 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "nginx", "stream"), 0o755)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { im.watchNPM(ctx); close(done) }()
	time.Sleep(200 * time.Millisecond)
	_ = os.RemoveAll(ph)
	time.Sleep(300 * time.Millisecond) // IN_IGNORED processed
	_ = os.MkdirAll(ph, 0o755)
	time.Sleep(200 * time.Millisecond) // watched again by the poll
	port := freePort(t)
	_ = os.WriteFile(filepath.Join(ph, "1.conf"), []byte("set $server \"web.b.switchyard\";\nset $port "+strconv.Itoa(port)+";\n"), 0o644)
	deadline := time.Now().Add(10 * time.Second)
	for im.npm.Ports("web.b.switchyard") == nil {
		if time.Now().After(deadline) {
			t.Fatal("recreated directory not watched")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestSyscallFailures(t *testing.T) {
	im := newImporter(t)
	im.tune.inotifyInit = func(int) (int, error) { return -1, errors.New("no inotify") }
	im.watchNPM(context.Background()) // logs and returns

	oldIf := interfaces
	interfaces = func() ([]net.Interface, error) { return nil, errors.New("no interfaces") }
	if _, err := ifaceFor(netip.MustParseAddr("127.0.0.1")); err == nil {
		t.Fatal("interface error ignored")
	}
	interfaces = oldIf
}

func TestMembersBackoff(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	im := newImporter(t)
	im.dc = f.Client()
	im.tune.backoffMin, im.tune.backoffMax = 10*time.Millisecond, 30*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { im.watchMembers(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond) // the stream outlives the backoff limit
	f.CloseEvents()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	im.tune.backoffMin = time.Hour // cancel while waiting
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { im.watchMembers(ctx2); close(done2) }()
	time.Sleep(100 * time.Millisecond)
	cancel2()
	<-done2
}

func TestNPMPollAndStop(t *testing.T) {
	im := newImporter(t)
	im.tune.npmPoll = 20 * time.Millisecond
	im.npm = npmconf.NewStore(t.TempDir()) // directories never appear
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { im.watchNPM(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel() // closes the inotify descriptor: the read loop ends
	<-done
}

// funcOpener answers every open with f.
type funcOpener func() (net.Conn, byte, error)

func (funcOpener) Known(string) bool { return true }
func (f funcOpener) Open(context.Context, string, wire.Target) (net.Conn, byte, error) {
	return f()
}

// udpTo answers a local UDP name and sends one datagram to it.
func udpTo(t *testing.T, im *Importer) netip.AddrPort {
	t.Helper()
	port := freePort(t)
	query(im, "web."+strconv.Itoa(port)+".local.switchyard", dns.TypeA)
	u, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() })
	_, _ = u.Write([]byte("x"))
	return netip.AddrPortFrom(im.Own, uint16(port))
}

func TestUDPRefusedWithConn(t *testing.T) {
	im := newImporter(t)
	closed := make(chan struct{})
	a, b := net.Pipe()
	defer b.Close()
	im.open = funcOpener(func() (net.Conn, byte, error) {
		go func() { _, _ = b.Read(make([]byte, 1)); close(closed) }()
		return a, wire.StatusNoTarget, nil
	})
	udpTo(t, im)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("refused stream not closed")
	}
}

func TestUDPReplyAfterListenerClosed(t *testing.T) {
	im := newImporter(t)
	a, b := net.Pipe()
	defer b.Close()
	im.open = funcOpener(func() (net.Conn, byte, error) { return a, wire.StatusOK, nil })
	ap := udpTo(t, im)
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := b.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	im.mu.Lock()
	im.closeListener(ap)
	im.mu.Unlock()
	_, _ = b.Write([]byte("y")) // the reply cannot be sent: the flow ends
	if _, err := b.Read(make([]byte, 1)); err == nil {
		t.Fatal("flow not closed")
	}
}

func TestUDPWriteFails(t *testing.T) {
	im := newImporter(t)
	sent := make(chan struct{})
	im.open = funcOpener(func() (net.Conn, byte, error) {
		a, b := net.Pipe()
		b.Close()
		close(sent)
		return a, wire.StatusOK, nil
	})
	udpTo(t, im)
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("no open")
	}
	time.Sleep(100 * time.Millisecond)
}

func TestJoinedWithoutAddress(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	im := newImporter(t)
	im.dc, im.network = f.Client(), "imp"
	f.Set(dockertest.Ctr("noip", "noip", nil, map[string]docker.Endpoint{"imp": {NetworkID: "n-imp"}}))
	free := im.table.Free()
	im.joined(context.Background(), "noip")
	if im.table.Free() != free {
		t.Fatal("address taken without an IP")
	}
}

func TestWatchNPMLateStreamDir(t *testing.T) {
	im := newImporter(t)
	im.tune.npmPoll = 20 * time.Millisecond
	dir := t.TempDir()
	im.npm = npmconf.NewStore(dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { im.watchNPM(ctx); close(done) }()
	_ = os.MkdirAll(filepath.Join(dir, "nginx", "proxy_host"), 0o755)
	time.Sleep(200 * time.Millisecond) // proxy_host is watched, stream not yet
	st := filepath.Join(dir, "nginx", "stream")
	_ = os.MkdirAll(st, 0o755)
	port := freePort(t)
	conf := "set $server \"web.b.switchyard\";\nset $port " + strconv.Itoa(port) + ";\n"
	deadline := time.Now().Add(10 * time.Second)
	for im.npm.Ports("web.b.switchyard") == nil {
		_ = os.WriteFile(filepath.Join(st, "1.conf"), []byte(conf), 0o644)
		if time.Now().After(deadline) {
			t.Fatal("stream file not picked up")
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done
}
