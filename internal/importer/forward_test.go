package importer

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

var quiet = slog.New(slog.DiscardHandler)

func TestExtServers(t *testing.T) {
	f := filepath.Join(t.TempDir(), "resolv.conf")
	_ = os.WriteFile(f, []byte("nameserver 127.0.0.11\n"+
		"# Based on host file: '/etc/resolv.conf' (internal resolver)\n"+
		"# ExtServers: [host(192.168.1.1) host(127.0.0.53) host(fd00::1) 9.9.9.9]\n"), 0o644)
	got, skipped := ExtServers(f, false)
	if len(got) != 2 || got[0] != "192.168.1.1:53" || got[1] != "9.9.9.9:53" {
		t.Fatalf("v4 only: %v", got)
	}
	if len(skipped) != 2 || skipped[0] != "127.0.0.53 (loopback)" || !strings.HasPrefix(skipped[1], "fd00::1 (IPv6") {
		t.Fatalf("skipped: %v", skipped)
	}
	if got, _ := ExtServers(f, true); len(got) != 3 || got[1] != "[fd00::1]:53" {
		t.Fatalf("with v6: %v", got)
	}
	if got, _ := ExtServers(filepath.Join(t.TempDir(), "missing"), false); got != nil {
		t.Fatal("missing file")
	}
	_ = os.WriteFile(f, []byte("# ExtServers: [bogus]\n"), 0o644)
	if got, skipped := ExtServers(f, false); got != nil || skipped[0] != "bogus (not an address)" {
		t.Fatalf("bogus: %v %v", got, skipped)
	}
}

// server answers every query with rcode, or with an A record when rcode is success.
func server(t *testing.T, rcode int) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(q, rcode)
		if rcode == dns.RcodeSuccess {
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA,
				Class: dns.ClassINET, Ttl: 5}, A: net.IPv4(1, 2, 3, 4)}}
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = s.ActivateAndServe() }()
	t.Cleanup(func() { _ = s.Shutdown() })
	return pc.LocalAddr().String()
}

func TestForwarder(t *testing.T) {
	q := new(dns.Msg)
	q.SetQuestion("example.org.", dns.TypeA)
	f := NewForwarder([]string{server(t, dns.RcodeServerFailure), server(t, dns.RcodeRefused), server(t, dns.RcodeSuccess)}, quiet)
	r := f.Exchange(q, false)
	if r == nil || len(r.Answer) != 1 {
		t.Fatalf("SERVFAIL and REFUSED must be skipped: %v", r)
	}
	nx := NewForwarder([]string{server(t, dns.RcodeNameError)}, quiet)
	if r := nx.Exchange(q, false); r == nil || r.Rcode != dns.RcodeNameError {
		t.Fatal("NXDOMAIN is a valid answer")
	}
	if NewForwarder([]string{server(t, dns.RcodeServerFailure)}, quiet).Exchange(q, false) != nil {
		t.Fatal("all failed must give nil")
	}
}

func TestForwarderTCP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &dns.Server{Listener: l, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		_ = w.WriteMsg(m)
	})}
	go func() { _ = s.ActivateAndServe() }()
	defer s.Shutdown()
	q := new(dns.Msg)
	q.SetQuestion("example.org.", dns.TypeA)
	if r := NewForwarder([]string{l.Addr().String()}, quiet).Exchange(q, true); r == nil || r.Rcode != dns.RcodeSuccess {
		t.Fatalf("tcp: %v", r)
	}
}

func TestForwarderUnreachable(t *testing.T) {
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	dead := pc.LocalAddr().String()
	pc.Close() // nothing listens there any more
	q := new(dns.Msg)
	q.SetQuestion("example.org.", dns.TypeA)
	if NewForwarder([]string{dead, server(t, dns.RcodeSuccess)}, quiet).Exchange(q, false) == nil {
		t.Fatal("next server not tried")
	}
}
