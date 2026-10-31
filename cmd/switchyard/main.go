// Command switchyard is the npm-switchyard agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/export"
	"github.com/motwok/npm-switchyard/internal/importer"
	"github.com/motwok/npm-switchyard/internal/metrics"
	"github.com/motwok/npm-switchyard/internal/peer"
	"github.com/motwok/npm-switchyard/internal/wire"
)

var version = "dev"

func main() {
	fs := flag.NewFlagSet("switchyard", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "configuration file")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	switch fs.Arg(0) {
	case "":
		os.Exit(run(*cfgPath))
	case "health":
		os.Exit(health(*cfgPath))
	case "genkey":
		fmt.Println(config.GenerateKey())
	case "pubkey":
		os.Exit(pubkey())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", fs.Arg(0))
		os.Exit(2)
	}
}

// pubkey prints the public key of the private key on stdin (CLI-4).
func pubkey() int {
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	k, err := config.ParsePrivateKey(string(b))
	if err != nil {
		fmt.Fprintln(os.Stderr, "private key:", err)
		return 1
	}
	fmt.Println(config.PublicKeyString(k))
	return 0
}

func load(path string) (*config.Config, error) {
	data, src, err := config.Source(path, os.Getenv)
	if err != nil {
		return nil, err
	}
	c, err := config.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	if c.ImportNetwork != "" && runtime.GOOS != "linux" {
		return nil, errors.New("import_network: import is only supported on Linux")
	}
	return c, nil
}

// health implements HLT-1.
func health(path string) int {
	c, err := load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cl := http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get("http://127.0.0.1:" + strconv.Itoa(c.Metrics) + "/metrics")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run(path string) int {
	cfg, err := load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	log.Info("starting", "version", version, "partners", len(cfg.Partners), "import", cfg.ImportNetwork != "",
		"public_key", config.PublicKeyString(cfg.Key))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg, log, docker.New(), docker.SelfID())
}

// serve runs the agent until ctx ends.
func serve(ctx context.Context, cfg *config.Config, log *slog.Logger, dc *docker.Client, selfID string) int {
	v, err := dc.Version(ctx)
	if err != nil {
		log.Error("docker api", "err", err)
		return 1
	}
	if !apiAtLeast(v.APIVersion, 1, 44) {
		log.Error("docker api too old", "found", v.APIVersion, "need", "1.44")
		return 1
	}
	self, err := dc.Inspect(ctx, selfID)
	if err != nil {
		log.Error("inspect own container", "id", selfID, "err", err)
		return 1
	}
	var ownIPs []string
	for _, ep := range self.NetworkSettings.Networks {
		if ep.IPAddress != "" {
			ownIPs = append(ownIPs, ep.IPAddress)
		}
	}
	if len(ownIPs) == 0 {
		log.Error("own container has no IPv4 address")
		return 1
	}

	met := metrics.New()
	exp := export.New(dc, self.ID, cfg.ImportNetwork, log)
	go exp.Run(ctx)

	var wp []wire.Partner
	for _, p := range cfg.Partners {
		wp = append(wp, wire.Partner{Own: p.Own, Peer: p.Peer})
	}
	local, err := wire.NewLocal(cfg.Key, wp)
	if err != nil {
		log.Error("tls", "err", err)
		return 1
	}
	mgr := peer.New(local, cfg.Partners, exp, met, log)
	var pls []net.Listener
	for _, ip := range ownIPs {
		l, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(cfg.Listen)))
		if err != nil {
			log.Error("listen", "addr", ip, "err", err)
			return 1
		}
		pls = append(pls, l)
	}
	mgr.Run(ctx, pls)

	ready := make(chan struct{})
	errc := make(chan error, 2)
	if cfg.ImportNetwork != "" {
		im, err := importer.Setup(ctx, dc, self.ID, cfg, &opener{mgr: mgr, exp: exp, met: met, local: cfg.LocalNetworks}, met, log)
		if err != nil {
			log.Error("import", "err", err)
			return 1
		}
		go func() { errc <- im.Serve(ctx, ready) }()
	} else {
		close(ready)
	}

	select {
	case <-ready:
	case err := <-errc:
		log.Error("import", "err", err)
		return 1
	}
	srv := &http.Server{Handler: metricsHandler(met), ReadHeaderTimeout: 10 * time.Second}
	for _, ip := range append(ownIPs, "127.0.0.1") {
		l, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(cfg.Metrics)))
		if err != nil {
			log.Error("listen metrics", "addr", ip, "err", err)
			return 1
		}
		go func() { errc <- srv.Serve(l) }()
	}
	log.Info("ready", "addresses", strings.Join(ownIPs, ","), "listen", cfg.Listen, "metrics", cfg.Metrics)

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("stopped", "err", err)
			return 1
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	for _, l := range pls {
		l.Close()
	}
	log.Info("stopped")
	return 0
}

func metricsHandler(met *metrics.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = met.Write(w)
	})
	return mux
}

func apiAtLeast(v string, major, minor int) bool {
	a, b, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	x, err1 := strconv.Atoi(a)
	y, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return x > major || x == major && y >= minor
}

// opener routes import streams to partners or, for "local", to the own exporter (NAM-7).
type opener struct {
	mgr   *peer.Manager
	exp   *export.Exporter
	met   *metrics.Registry
	local config.Networks
}

func (o *opener) Known(p string) bool {
	if p == config.LocalPartner {
		return true
	}
	_, ok := o.mgr.Index(p)
	return ok
}

func (o *opener) Open(ctx context.Context, p string, t wire.Target) (net.Conn, byte, error) {
	if p != config.LocalPartner {
		idx, ok := o.mgr.Index(p)
		if !ok {
			return nil, 0, errors.New("unknown partner")
		}
		return o.mgr.Open(ctx, idx, t)
	}
	c, mt, st := o.exp.Connect(ctx, t, o.local.Allows)
	if st != wire.StatusOK {
		o.met.StreamResult(p, "refused", mt)
		return nil, st, nil
	}
	o.met.StreamResult(p, "ok", mt)
	o.met.StreamStarted(p, mt)
	return &localConn{Conn: c, met: o.met, mt: mt, untrack: o.exp.Track(c, mt)}, st, nil
}

// localConn counts a local stream on the exporting side's labels (MET-1).
type localConn struct {
	net.Conn
	met     *metrics.Registry
	mt      metrics.Target
	untrack func()
	once    sync.Once
}

func (c *localConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.met.AddBytes(config.LocalPartner, "in", c.mt, int64(n))
	return n, err
}

func (c *localConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.met.AddBytes(config.LocalPartner, "out", c.mt, int64(n))
	return n, err
}

func (c *localConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *localConn) Close() error {
	c.once.Do(func() {
		c.met.StreamEnded(config.LocalPartner, c.mt)
		c.untrack()
	})
	return c.Conn.Close()
}
