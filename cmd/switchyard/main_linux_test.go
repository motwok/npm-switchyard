//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/docker/dockertest"
)

// The importer is set up, but its DNS server cannot bind port 53 as a normal user: the
// agent stops with an error before it reports ready.
func TestServeImportFails(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	f.Set(dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"imp": {NetworkID: "n-imp", IPAddress: "127.0.0.1"}}))
	n := docker.Network{ID: "n-imp", Name: "imp"}
	n.IPAM.Config = []docker.IPAMConfig{{Subnet: "127.0.0.0/24", IPRange: "127.0.0.128/25"}}
	f.Networks["n-imp"] = n
	cfg, err := config.Parse([]byte("private_key: " + key + "\nimport_network: imp\nnpm_data: " + t.TempDir() +
		"\nlisten: " + strconv.Itoa(freePort(t)) + "\nmetrics: " + strconv.Itoa(freePort(t)) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), f.Client(), "self") }()
	// As root (port 53 allowed) it runs until cancelled.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
	}
	cancel()
}
