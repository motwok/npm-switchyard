package importer

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/miekg/dns"
)

func TestExtServers(t *testing.T) {
	f := filepath.Join(t.TempDir(), "resolv.conf")
	_ = os.WriteFile(f, []byte("nameserver 127.0.0.11\n"+
		"# Based on host file: '/etc/resolv.conf' (internal resolver)\n"+
		"# ExtServers: [host(192.168.1.1) host(127.0.0.53) host(fd00::1) 9.9.9.9]\n"), 0o644)
	got := ExtServers(f, false)
	if len(got) != 2 || got[0] != "192.168.1.1:53" || got[1] != "9.9.9.9:53" {
		t.Fatalf("v4 only: %v", got)
	}
	if got := ExtServers(f, true); len(got) != 3 || got[1] != "[fd00::1]:53" {
		t.Fatalf("with v6: %v", got)
	}
	if ExtServers(filepath.Join(t.TempDir(), "missing"), false) != nil {
		t.Fatal("missing file")
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
	f := NewForwarder([]string{server(t, dns.RcodeServerFailure), server(t, dns.RcodeRefused), server(t, dns.RcodeSuccess)})
	r := f.Exchange(q, false)
	if r == nil || len(r.Answer) != 1 {
		t.Fatalf("SERVFAIL and REFUSED must be skipped: %v", r)
	}
	nx := NewForwarder([]string{server(t, dns.RcodeNameError)})
	if r := nx.Exchange(q, false); r == nil || r.Rcode != dns.RcodeNameError {
		t.Fatal("NXDOMAIN is a valid answer")
	}
	if NewForwarder([]string{server(t, dns.RcodeServerFailure)}).Exchange(q, false) != nil {
		t.Fatal("all failed must give nil")
	}
}
