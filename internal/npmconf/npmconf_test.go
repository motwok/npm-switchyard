package npmconf

import (
	"os"
	"path/filepath"
	"testing"
)

const proxyHost = `server {
  set $forward_scheme http;
  set $server         "NextCloud.HostB.switchyard";
  set $port           443;

  listen 80;
  location /api {
    proxy_pass       http://api.9000.hostb.switchyard:9000/v1;
  }
  location / {
    include conf.d/include/proxy.conf;
  }
}
`

const stream = `server {
  listen 5432 reuseport;
  proxy_pass db.hostb.switchyard:5432;
}
server {
  listen 5353 udp reuseport;
  proxy_pass dns.hostb.switchyard:53;
}
server {
  listen 22;
  proxy_pass 192.168.1.10:22;
}
`

func TestParse(t *testing.T) {
	got := Parse(proxyHost)
	if !got["nextcloud.hostb.switchyard"][443] || !got["api.9000.hostb.switchyard"][9000] || len(got) != 2 {
		t.Fatalf("proxy host: %v", got)
	}
	got = Parse(stream)
	if !got["db.hostb.switchyard"][5432] || !got["dns.hostb.switchyard"][53] || len(got) != 2 {
		t.Fatalf("stream: %v", got)
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	ph := filepath.Join(dir, "nginx", "proxy_host")
	st := filepath.Join(dir, "nginx", "stream")
	_ = os.MkdirAll(ph, 0o755)
	_ = os.MkdirAll(st, 0o755)
	_ = os.WriteFile(filepath.Join(ph, "1.conf"), []byte(proxyHost), 0o644)
	_ = os.WriteFile(filepath.Join(st, "2.conf"), []byte(stream), 0o644)
	_ = os.WriteFile(filepath.Join(st, "3.conf.err"), []byte("proxy_pass bad.hostb.switchyard:1;\n"), 0o644)
	s := NewStore(dir)
	if !s.Available() {
		t.Fatal("not available")
	}
	s.Reload()
	if p := s.Ports("NEXTCLOUD.hostb.switchyard"); len(p) != 1 || p[0] != 443 {
		t.Fatalf("ports %v", p)
	}
	if s.Ports("bad.hostb.switchyard") != nil {
		t.Fatal(".err file used")
	}
	n := s.Names()
	if _, ok := n["api.9000.hostb.switchyard"]; ok {
		t.Fatal("long name listed as short")
	}
	if len(n) != 3 {
		t.Fatalf("short names %v", n)
	}
}

func TestMounted(t *testing.T) {
	d := t.TempDir()
	if !NewStore(d).Mounted() || NewStore(filepath.Join(d, "missing")).Mounted() {
		t.Fatal("Mounted")
	}
	s := NewStore(d) // mounted but NPM has not created its directories yet
	if s.Available() || s.Ports("x.b.switchyard") != nil {
		t.Fatal("empty store")
	}
}
