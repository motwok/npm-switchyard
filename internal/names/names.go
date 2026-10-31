// Package names parses .switchyard names (spec 6).
package names

import (
	"strconv"
	"strings"
)

const Suffix = "switchyard"

type Name struct {
	Container string
	Port      int // 0 for a short name
	Partner   string
}

func (n Name) Short() bool { return n.Port == 0 }

// String is the canonical lowercase FQDN without trailing dot.
func (n Name) String() string {
	if n.Short() {
		return n.Container + "." + n.Partner + "." + Suffix
	}
	return n.Container + "." + strconv.Itoa(n.Port) + "." + n.Partner + "." + Suffix
}

// HasSuffix reports whether q (with or without trailing dot) ends in .switchyard.
func HasSuffix(q string) bool {
	q = strings.ToLower(strings.TrimSuffix(q, "."))
	return q == Suffix || strings.HasSuffix(q, "."+Suffix)
}

// Parse splits a query name from the right (NAM-2, NAM-4). ok is false for anything not
// of the form <container>[.<port>].<partner>.switchyard within DNS limits.
func Parse(q string) (Name, bool) {
	q = strings.ToLower(strings.TrimSuffix(q, "."))
	if len(q) > 253 {
		return Name{}, false
	}
	labels := strings.Split(q, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return Name{}, false
		}
	}
	if len(labels) < 3 || labels[len(labels)-1] != Suffix {
		return Name{}, false
	}
	n := Name{Partner: labels[len(labels)-2]}
	rest := labels[:len(labels)-2]
	if len(rest) >= 2 {
		if p, ok := port(rest[len(rest)-1]); ok {
			n.Port = p
			rest = rest[:len(rest)-1]
		}
	}
	n.Container = strings.Join(rest, ".")
	return n, true
}

func port(s string) (int, bool) {
	if s == "" || s[0] == '0' {
		return 0, false
	}
	p, err := strconv.Atoi(s)
	return p, err == nil && p >= 1 && p <= 65535
}
