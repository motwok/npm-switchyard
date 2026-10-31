package importer

import (
	"net/netip"
	"testing"
	"time"
)

func pool(t *testing.T) (*Pool, netip.Addr) {
	t.Helper()
	own := netip.MustParseAddr("172.30.0.2")
	p, err := NewPool(netip.MustParsePrefix("172.30.0.0/24"), netip.MustParsePrefix("172.30.0.128/25"),
		[]netip.Addr{own, netip.MustParseAddr("172.30.0.1"), netip.MustParseAddr("172.30.0.5")})
	if err != nil {
		t.Fatal(err)
	}
	return p, own
}

func TestPool(t *testing.T) {
	p, _ := pool(t)
	// .0 network, .1 gateway, .2 own, .5 member excluded; .128-.255 is ip_range.
	if got := p.Free(); got != 128-4 {
		t.Fatalf("free = %d", got)
	}
	a, _ := p.Take()
	if a != netip.MustParseAddr("172.30.0.3") {
		t.Fatalf("first = %s", a)
	}
	b, _ := p.Take()
	if b != netip.MustParseAddr("172.30.0.4") {
		t.Fatalf("second = %s", b)
	}
	c, _ := p.Take()
	if c != netip.MustParseAddr("172.30.0.6") {
		t.Fatalf("third = %s", c)
	}
	if p.Contains(netip.MustParseAddr("172.30.0.200")) {
		t.Fatal("ip_range address in pool")
	}
}

func TestLongSharesIP(t *testing.T) {
	p, own := pool(t)
	tb := NewTable(p, own, []int{53, 9481, 9480})
	a, newA, _ := tb.Long("a.80.hb.switchyard", 80)
	b, newB, _ := tb.Long("b.443.hb.switchyard", 443)
	if a.Addr != own || b.Addr != own || newA || newB {
		t.Fatalf("expected own IP: %s %s", a.Addr, b.Addr)
	}
	c, newC, _ := tb.Long("c.80.hb.switchyard", 80)
	if c.Addr == own || !newC {
		t.Fatal("port collision must get a new IP")
	}
	d, newD, _ := tb.Long("d.443.hb.switchyard", 443)
	if d.Addr != c.Addr || newD {
		t.Fatal("second collision IP should be reused for a free port")
	}
	again, _, _ := tb.Long("a.80.hb.switchyard", 80)
	if again != a {
		t.Fatal("stable mapping")
	}
	dns, _, _ := tb.Long("x.53.hb.switchyard", 53)
	if dns.Addr == own {
		t.Fatal("agent's own port must not be used on its own IP")
	}
}

func TestShortOwnIPAndQuarantine(t *testing.T) {
	p, own := pool(t)
	tb := NewTable(p, own, nil)
	e, newAddr, added, err := tb.Short("n.hb.switchyard", []int{443})
	if err != nil || !newAddr || e.Addr == own || len(added) != 1 {
		t.Fatal("short name needs its own address")
	}
	_, _, added, _ = tb.Short("n.hb.switchyard", []int{443, 8443})
	if len(added) != 1 || added[0] != 8443 {
		t.Fatalf("added = %v", added)
	}
	now := time.Now()
	if _, ok := tb.FreeShort("n.hb.switchyard", now); !ok {
		t.Fatal("free")
	}
	if _, ok := tb.Lookup("n.hb.switchyard"); ok {
		t.Fatal("freed name still active")
	}
	other, _, _, _ := tb.Short("m.hb.switchyard", []int{80})
	if other.Addr == e.Addr {
		t.Fatal("quarantined address reused")
	}
	back, newAddr, added, _ := tb.Short("n.hb.switchyard", []int{443})
	if back.Addr != e.Addr || newAddr || len(added) != 2 {
		t.Fatalf("restore within quarantine: %s new=%v added=%v", back.Addr, newAddr, added)
	}
	tb.FreeShort("n.hb.switchyard", now)
	free := tb.Free()
	tb.Expire(now.Add(Quarantine))
	if tb.Free() != free+1 {
		t.Fatal("address not released after quarantine")
	}
}

func TestJoined(t *testing.T) {
	p, own := pool(t)
	tb := NewTable(p, own, nil)
	e, _, _, _ := tb.Short("n.hb.switchyard", []int{443})
	if name, conflict := tb.Joined(e.Addr); !conflict || name != "n.hb.switchyard" {
		t.Fatalf("conflict not found: %q %v", name, conflict)
	}
	free := tb.Free()
	late := netip.MustParseAddr("172.30.0.100")
	if _, conflict := tb.Joined(late); conflict || tb.Free() != free-1 {
		t.Fatal("free address of a late member not reserved")
	}
	if x, _, _, _ := tb.Short("m.hb.switchyard", []int{80}); x.Addr == late {
		t.Fatal("late member's address handed out")
	}
	if _, conflict := tb.Joined(netip.MustParseAddr("172.30.0.200")); conflict {
		t.Fatal("ip_range address reported")
	}
}

func TestFull(t *testing.T) {
	own := netip.MustParseAddr("10.0.0.2")
	p, _ := NewPool(netip.MustParsePrefix("10.0.0.0/29"), netip.MustParsePrefix("10.0.0.4/30"),
		[]netip.Addr{own, netip.MustParseAddr("10.0.0.1")})
	tb := NewTable(p, own, nil)
	if _, _, _, err := tb.Short("a.p.switchyard", []int{1}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := tb.Short("b.p.switchyard", []int{1}); err != ErrFull {
		t.Fatalf("err = %v", err)
	}
}

func TestShortNames(t *testing.T) {
	p, own := pool(t)
	tb := NewTable(p, own, nil)
	tb.Short("a.hb.switchyard", []int{1})
	tb.Long("b.2.hb.switchyard", 2)
	if n := tb.ShortNames(); len(n) != 1 || n[0] != "a.hb.switchyard" {
		t.Fatal(n)
	}
	if _, ok := tb.FreeShort("b.2.hb.switchyard", time.Now()); ok {
		t.Fatal("long name freed")
	}
	if _, ok := tb.FreeShort("missing", time.Now()); ok {
		t.Fatal("unknown name freed")
	}
	if name, conflict := tb.Joined(own); conflict || name != "" {
		t.Fatal("own address is no conflict")
	}
	c, _, _ := tb.Long("c.2.hb.switchyard", 2) // collision IP
	if _, conflict := tb.Joined(c.Addr); !conflict {
		t.Fatal("collision IP conflict")
	}
	if _, err := NewPool(netip.MustParsePrefix("fd00::/64"), netip.Prefix{}, nil); err == nil {
		t.Fatal("IPv6 subnet accepted")
	}
}
