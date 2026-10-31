//go:build linux

package importer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/docker/dockertest"
	"github.com/motwok/npm-switchyard/internal/metrics"
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
	im.mu.Lock()
	e := &Entry{Name: "n.b.switchyard", Addr: im.Own, Ports: map[int]bool{}, Short: true}
	im.table.names[e.Name] = e
	im.mu.Unlock()
	im.syncShort() // NPM store is empty: the name goes to quarantine
	im.mu.Lock()
	_, ok := im.table.Lookup("n.b.switchyard")
	im.mu.Unlock()
	if ok {
		t.Fatal("gone short name still active")
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
	query(full, "a."+strconv.Itoa(port)+".b.switchyard", dns.TypeA)
	if r := query(full, "c."+strconv.Itoa(port)+".b.switchyard", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Fatalf("full: %v", r)
	}
}

func TestPortInUse(t *testing.T) {
	im := newImporter(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	// Answered, but the listener cannot be opened; logged, no crash.
	if r := query(im, "x."+strconv.Itoa(port)+".b.switchyard", dns.TypeA); r.Rcode != dns.RcodeSuccess {
		t.Fatalf("%v", r)
	}
	u, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	query(im, "y."+strconv.Itoa(u.LocalAddr().(*net.UDPAddr).Port)+".b.switchyard", dns.TypeA)
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
	im.dc, im.networkID, im.network = f.Client(), "n-imp", "imp"
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
	// Port 53 needs root; as a normal user Serve fails, as root it serves until ctx ends.
	_ = im.Serve(ctx, ready)
	cancel()

	f.Set(dockertest.Ctr("c1", "db", nil, map[string]docker.Endpoint{"n": {NetworkID: "n1", DNSNames: []string{"db"}}}))
	self := dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"n": {NetworkID: "n1"}})
	im.ownNet = map[string]bool{}
	im.fillOwnNames(context.Background(), f.Client(), self)
	if !im.ownNet["db"] || !im.ownNet["db.n"] {
		t.Fatal(im.ownNet)
	}
	dead := docker.NewWithDialer(func(context.Context) (net.Conn, error) { return nil, io.EOF })
	im.fillOwnNames(context.Background(), dead, self)
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
