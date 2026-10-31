package names

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Name
		ok   bool
	}{
		{"nextcloud.8080.hostb.switchyard.", Name{"nextcloud", 8080, "hostb"}, true},
		{"NextCloud.8080.HostB.switchyard", Name{"nextcloud", 8080, "hostb"}, true},
		{"nextcloud.hostb.switchyard", Name{"nextcloud", 0, "hostb"}, true},
		{"www.quark.de.8080.hostb.switchyard", Name{"www.quark.de", 8080, "hostb"}, true},
		{"app.8080.8080.hostb.switchyard", Name{"app.8080", 8080, "hostb"}, true},
		{"app.8080.hostb.switchyard", Name{"app", 8080, "hostb"}, true},
		{"8080.hostb.switchyard", Name{"8080", 0, "hostb"}, true},
		{"app.70000.hostb.switchyard", Name{"app.70000", 0, "hostb"}, true},
		{"app.080.hostb.switchyard", Name{"app.080", 0, "hostb"}, true},
		{"my_db.5432.local.switchyard", Name{"my_db", 5432, "local"}, true},
		{"hostb.switchyard", Name{}, false},
		{"a..hostb.switchyard", Name{}, false},
		{"a.hostb.example", Name{}, false},
		{strings.Repeat("a", 64) + ".hostb.switchyard", Name{}, false},
	}
	for _, c := range cases {
		got, ok := Parse(c.in)
		if ok != c.ok || ok && got != c.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestTotalLength(t *testing.T) {
	long := strings.Repeat(strings.Repeat("a", 60)+".", 4) + "hostb.switchyard"
	if _, ok := Parse(long); ok {
		t.Fatal("name over 253 characters accepted")
	}
}

func TestString(t *testing.T) {
	n, _ := Parse("App.80.HostB.switchyard.")
	if n.String() != "app.80.hostb.switchyard" {
		t.Fatal(n.String())
	}
	if !HasSuffix("x.switchyard.") || HasSuffix("x.switchyards") {
		t.Fatal("HasSuffix")
	}
}
