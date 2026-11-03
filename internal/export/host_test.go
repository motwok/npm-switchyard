package export

import (
	"context"
	"io"
	"testing"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/docker/dockertest"
	"github.com/motwok/npm-switchyard/internal/wire"
)

func hostOn(n string) bool { return true }

func gateways(f *dockertest.Fake, apps, imp string) {
	net := func(gw string) docker.Network {
		var n docker.Network
		n.IPAM.Config = []docker.IPAMConfig{{Gateway: gw}}
		return n
	}
	f.Networks["n-apps"], f.Networks["n-imp"] = net(apps), net(imp)
}

func TestHostViaGateway(t *testing.T) {
	port := target(t)
	f, e := setup(t)
	// apps comes first by name; its IPv6 gateway is skipped.
	gateways(f, "::1", "127.0.0.1")
	// A container named host does not hide the Docker host.
	f.Set(dockertest.Ctr("host0000000000", "host", nil,
		map[string]docker.Endpoint{"apps": {NetworkID: "n-apps", IPAddress: "127.0.0.9", DNSNames: []string{"host"}}}))
	ctx := context.Background()
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	c, mt, st := e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: port, Name: "HOST"}, hostOn)
	if st != wire.StatusOK || mt.Container != config.HostName || mt.Service != "" {
		t.Fatalf("status %d, labels %+v", st, mt)
	}
	done := e.Track(c, mt)
	defer done()
	if err := e.Refresh(ctx); err != nil { // must not drop the host stream
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("hi"))
	b := make([]byte, 2)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "hi" {
		t.Fatalf("echo %q %v", b, err)
	}
	c.Close()
}

func TestHostAddress(t *testing.T) {
	port := target(t)
	_, e := setup(t)
	e.HostAddress = "127.0.0.1"
	ctx := context.Background()
	if _, _, st := e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: port, Name: "host"}, hostOn); st != wire.StatusOK {
		t.Fatalf("status %d", st)
	}
	if _, _, st := e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: 1, Name: "host"}, hostOn); st != wire.StatusUnreachable {
		t.Fatalf("closed port: status %d", st)
	}
}

func TestHostRefused(t *testing.T) {
	port := target(t)
	ctx := context.Background()
	try := func(e *Exporter, allow func(string) bool) byte {
		_, _, st := e.Connect(ctx, wire.Target{Proto: wire.TCP, Port: port, Name: "host"}, allow)
		return st
	}
	// Off: host is an ordinary container name, and there is none.
	_, e := setup(t)
	if st := try(e, func(n string) bool { return n != config.HostScope }); st != wire.StatusNoTarget {
		t.Errorf("off: status %d", st)
	}
	// No IPv4 gateway.
	f, e := setup(t)
	gateways(f, "", "::1")
	if st := try(e, hostOn); st != wire.StatusNoTarget {
		t.Errorf("no gateway: status %d", st)
	}
	// Network unknown to the daemon.
	_, e = setup(t)
	if st := try(e, hostOn); st != wire.StatusNoTarget {
		t.Errorf("network error: status %d", st)
	}
	// Own container cannot be inspected.
	f, e = setup(t)
	f.FailInspect["self0000000000"] = true
	if st := try(e, hostOn); st != wire.StatusNoTarget {
		t.Errorf("inspect error: status %d", st)
	}
}
