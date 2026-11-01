//go:build linux

package importer

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/motwok/npm-switchyard/internal/npmconf"
)

func (l *logBuf) waitFor(t *testing.T, lines ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, missing := l.String(), ""
		for _, s := range lines {
			if !strings.Contains(out, s) {
				missing = s
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log lacks %q:\n%s", missing, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func debugImporter(t *testing.T) (*Importer, *logBuf) {
	t.Helper()
	im := newImporter(t)
	lb := &logBuf{}
	im.log = slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return im, lb
}

func TestLogDNSReasons(t *testing.T) {
	im, lb := debugImporter(t)
	port := freePort(t)
	name := "web." + strconv.Itoa(port) + ".b.switchyard."
	query(im, name, dns.TypeA)
	query(im, name, dns.TypeAAAA)
	query(im, "b.switchyard", dns.TypeA)
	query(im, "web.80.x.switchyard", dns.TypeA)
	query(im, "web.b.switchyard", dns.TypeA)
	im.npm = npmconf.NewStore(filepath.Join(t.TempDir(), "missing"))
	query(im, "web.b.switchyard", dns.TypeA)
	query(im, "db", dns.TypeA)
	lb.waitFor(t,
		`msg=dns name=`+name+` type=A client=`, `result=NOERROR addr=127.0.0.1 new=true ports=[`+strconv.Itoa(port)+`]`,
		`msg=dns name=`+name+` type=AAAA client=`, `result=NOERROR addr=127.0.0.1 new=false`,
		`name=b.switchyard. type=A client=:0 result=NXDOMAIN reason="not a valid switchyard name"`,
		`reason="unknown partner"`,
		`reason="short name not used in NPM"`,
		`reason="short name, but NPM files are not mounted"`,
		`msg=dns name=db. result=NXDOMAIN reason="container in the agent's networks, not reachable for the client"`)
}

func TestLogNPMChanges(t *testing.T) {
	im, lb := debugImporter(t)
	dir := t.TempDir()
	im.npm = npmconf.NewStore(dir)
	ph := filepath.Join(dir, "nginx", "proxy_host")
	_ = os.MkdirAll(ph, 0o755)
	write := func(port int) {
		conf := "set $server \"s.b.switchyard\";\nset $port " + strconv.Itoa(port) + ";\n"
		_ = os.WriteFile(filepath.Join(ph, "1.conf"), []byte(conf), 0o644)
	}
	write(81)
	im.reloadNPM()
	write(82)
	im.reloadNPM()
	_ = os.Remove(filepath.Join(ph, "1.conf"))
	im.reloadNPM()
	lb.waitFor(t, `msg="NPM short name added" name=s.b.switchyard ports=[81]`,
		`msg="NPM short name ports changed" name=s.b.switchyard ports=[82] before=[81]`,
		`msg="NPM short name removed" name=s.b.switchyard`)
}

func TestLogStream(t *testing.T) {
	im, lb := debugImporter(t)
	port := freePort(t)
	name := "web." + strconv.Itoa(port) + ".b.switchyard"
	query(im, name, dns.TypeA)
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("hi"))
	b := make([]byte, 2)
	_, _ = c.Read(b)
	c.Close()
	lb.waitFor(t, `msg="stream opened" name=`+name+` port=`+strconv.Itoa(port)+` proto=tcp client=127.0.0.1:`,
		`msg="stream closed" name=`+name, "bytes_in=2 bytes_out=2")
}

type idConn struct{ net.Conn }

func (idConn) StreamID() string { return "0a0b0c0d" }

func TestStreamID(t *testing.T) {
	if streamID(idConn{}) != "0a0b0c0d" || streamID(&net.TCPConn{}) != "" {
		t.Fatal("streamID")
	}
}
