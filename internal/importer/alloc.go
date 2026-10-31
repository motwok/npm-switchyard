// Package importer answers DNS for .switchyard names and listens on their addresses (spec 7, 8).
package importer

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"sort"
	"time"
)

// Quarantine before a freed short name's address goes to another name (IMP-7).
const Quarantine = time.Hour

var ErrFull = errors.New("no free address")

// Pool hands out addresses of the subnet outside ip_range (IMP-2).
type Pool struct {
	free []netip.Addr
	used map[netip.Addr]bool
}

// NewPool builds the free list. exclude holds gateway, aux addresses, the agent's own IP and
// every member's IP.
func NewPool(subnet, ipRange netip.Prefix, exclude []netip.Addr) (*Pool, error) {
	if !subnet.Addr().Is4() {
		return nil, errors.New("import network needs an IPv4 subnet")
	}
	ex := map[netip.Addr]bool{}
	for _, a := range exclude {
		ex[a] = true
	}
	subnet = subnet.Masked()
	first := subnet.Addr()
	n := uint32(1) << (32 - subnet.Bits())
	base := binary.BigEndian.Uint32(first.AsSlice())
	p := &Pool{used: map[netip.Addr]bool{}}
	for i := uint32(1); i+1 < n; i++ { // skip network and broadcast
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], base+i)
		a := netip.AddrFrom4(b)
		if ipRange.IsValid() && ipRange.Contains(a) || ex[a] {
			continue
		}
		p.free = append(p.free, a)
	}
	sort.Slice(p.free, func(i, j int) bool { return p.free[i].Less(p.free[j]) })
	return p, nil
}

// Take returns the lowest free address.
func (p *Pool) Take() (netip.Addr, error) {
	for _, a := range p.free {
		if !p.used[a] {
			p.used[a] = true
			return a, nil
		}
	}
	return netip.Addr{}, ErrFull
}

// Reserve marks an address used, e.g. one a container joined with later.
func (p *Pool) Reserve(a netip.Addr) { p.used[a] = true }

func (p *Pool) Release(a netip.Addr) { delete(p.used, a) }

func (p *Pool) Contains(a netip.Addr) bool {
	i := sort.Search(len(p.free), func(i int) bool { return !p.free[i].Less(a) })
	return i < len(p.free) && p.free[i] == a
}

func (p *Pool) Free() int {
	n := 0
	for _, a := range p.free {
		if !p.used[a] {
			n++
		}
	}
	return n
}

// Table maps names to addresses and ports (IMP-3, IMP-7). Not safe for concurrent use.
type Table struct {
	pool   *Pool
	own    netip.Addr
	ownUse map[int]bool // agent's own service ports on its own IP

	longIPs []netip.Addr                  // own IP first, then collision aliases, in order
	taken   map[netip.Addr]map[int]string // ip -> port -> name
	names   map[string]*Entry
}

type Entry struct {
	Name    string
	Addr    netip.Addr
	Ports   map[int]bool
	Short   bool
	freedAt time.Time // set while a short name is in quarantine
}

func (e *Entry) Freed() bool { return !e.freedAt.IsZero() }

func NewTable(pool *Pool, own netip.Addr, ownPorts []int) *Table {
	t := &Table{
		pool:    pool,
		own:     own,
		ownUse:  map[int]bool{},
		longIPs: []netip.Addr{own},
		taken:   map[netip.Addr]map[int]string{own: {}},
		names:   map[string]*Entry{},
	}
	for _, p := range ownPorts {
		t.ownUse[p] = true
	}
	return t
}

func (t *Table) portFree(a netip.Addr, port int) bool {
	if a == t.own && t.ownUse[port] {
		return false
	}
	_, used := t.taken[a][port]
	return !used
}

func (t *Table) mark(e *Entry, port int) {
	if t.taken[e.Addr] == nil {
		t.taken[e.Addr] = map[int]string{}
	}
	t.taken[e.Addr][port] = e.Name
	e.Ports[port] = true
}

// Lookup returns an active entry.
func (t *Table) Lookup(name string) (*Entry, bool) {
	e, ok := t.names[name]
	if !ok || e.Freed() {
		return nil, false
	}
	return e, true
}

// Long assigns (or returns) a long name's address. newAddr is true if an alias must be added.
func (t *Table) Long(name string, port int) (e *Entry, newAddr bool, err error) {
	if e, ok := t.names[name]; ok {
		return e, false, nil
	}
	e = &Entry{Name: name, Ports: map[int]bool{}}
	for _, a := range t.longIPs {
		if t.portFree(a, port) {
			e.Addr = a
			t.mark(e, port)
			t.names[name] = e
			return e, false, nil
		}
	}
	a, err := t.pool.Take()
	if err != nil {
		return nil, false, err
	}
	t.longIPs = append(t.longIPs, a)
	e.Addr = a
	t.mark(e, port)
	t.names[name] = e
	return e, true, nil
}

// Short assigns a short name its own address, or restores it from quarantine. added lists
// ports that need new listeners.
func (t *Table) Short(name string, ports []int) (e *Entry, newAddr bool, added []int, err error) {
	if e, ok := t.names[name]; ok {
		restored := e.Freed()
		e.freedAt = time.Time{}
		if restored {
			for p := range e.Ports {
				added = append(added, p)
			}
		}
		for _, p := range ports {
			if !e.Ports[p] {
				t.mark(e, p)
				added = append(added, p)
			}
		}
		return e, false, added, nil
	}
	a, err := t.pool.Take()
	if err != nil {
		return nil, false, nil, err
	}
	e = &Entry{Name: name, Addr: a, Ports: map[int]bool{}, Short: true}
	t.names[name] = e
	for _, p := range ports {
		t.mark(e, p)
	}
	return e, true, ports, nil
}

// FreeShort puts a short name into quarantine; its listeners are closed by the caller.
func (t *Table) FreeShort(name string, now time.Time) (*Entry, bool) {
	e, ok := t.names[name]
	if !ok || !e.Short || e.Freed() {
		return nil, false
	}
	e.freedAt = now
	return e, true
}

// Expire releases short names whose quarantine is over.
func (t *Table) Expire(now time.Time) {
	for n, e := range t.names {
		if e.Freed() && now.Sub(e.freedAt) >= Quarantine {
			delete(t.names, n)
			delete(t.taken, e.Addr)
			t.pool.Release(e.Addr)
		}
	}
}

// ShortNames lists active short names.
func (t *Table) ShortNames() []string {
	var out []string
	for n, e := range t.names {
		if e.Short && !e.Freed() {
			out = append(out, n)
		}
	}
	return out
}

func (t *Table) Free() int { return t.pool.Free() }

// Joined handles a container that joined the import network with address a later (IMP-8).
// It returns the name using a as an alias, if any; a free pool address is reserved.
func (t *Table) Joined(a netip.Addr) (string, bool) {
	if a == t.own {
		return "", false
	}
	for n, e := range t.names {
		if e.Addr == a {
			return n, true
		}
	}
	if t.pool.Contains(a) {
		t.pool.Reserve(a)
	}
	return "", false
}
