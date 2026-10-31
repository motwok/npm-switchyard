package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/docker/dockertest"
	"github.com/motwok/npm-switchyard/internal/export"
	"github.com/motwok/npm-switchyard/internal/metrics"
	"github.com/motwok/npm-switchyard/internal/peer"
	"github.com/motwok/npm-switchyard/internal/wire"
)

const key = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

func TestAPIAtLeast(t *testing.T) {
	for v, want := range map[string]bool{"1.44": true, "1.47": true, "2.0": true, "1.43": false, "x": false, "1.y": false} {
		if apiAtLeast(v, 1, 44) != want {
			t.Errorf("%s", v)
		}
	}
}

func TestLoad(t *testing.T) {
	f := filepath.Join(t.TempDir(), "c.yml")
	_ = os.WriteFile(f, []byte("private_key: "+key+"\n"), 0o644)
	if c, err := load(f); err != nil || c.Listen != 9481 {
		t.Fatal(c, err)
	}
	_ = os.WriteFile(f, []byte("bogus: 1\n"), 0o644)
	if _, err := load(f); err == nil {
		t.Fatal("invalid file accepted")
	}
	if _, err := load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestPubkey(t *testing.T) {
	r, w, _ := os.Pipe()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	_, _ = w.WriteString(key + "\n")
	w.Close()
	if pubkey() != 0 {
		t.Fatal("pubkey failed")
	}
	r2, w2, _ := os.Pipe()
	os.Stdin = r2
	_, _ = w2.WriteString("nonsense")
	w2.Close()
	if pubkey() != 1 {
		t.Fatal("invalid key accepted")
	}
}

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(metricsHandler(metrics.New()))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	f := filepath.Join(t.TempDir(), "c.yml")
	_ = os.WriteFile(f, []byte("private_key: "+key+"\nmetrics: "+strconv.Itoa(port)+"\n"), 0o644)
	if health(f) != 0 {
		t.Fatal("healthy agent reported unhealthy")
	}
	_ = os.WriteFile(f, []byte("private_key: "+key+"\nmetrics: 1\n"), 0o644)
	if health(f) != 1 {
		t.Fatal("dead port reported healthy")
	}
	if health(filepath.Join(t.TempDir(), "missing")) != 1 {
		t.Fatal("missing config reported healthy")
	}
}

func TestMetricsHandler(t *testing.T) {
	m := metrics.New()
	m.SetPeerUp("b", true)
	rec := httptest.NewRecorder()
	metricsHandler(m).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `switchyard_peer_up{peer="b"} 1`) ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestOpenerLocal(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			_, _ = io.Copy(c, c)
			c.Close()
		}
	}()
	f := dockertest.NewFake()
	defer f.Close()
	f.Set(dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"apps": {NetworkID: "n1", IPAddress: "10.0.0.2"}}))
	f.Set(dockertest.Ctr("web1", "web", nil, map[string]docker.Endpoint{"apps": {NetworkID: "n1", IPAddress: "127.0.0.1", DNSNames: []string{"web"}}}))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	exp := export.New(f.Client(), "self", "", log)
	if err := exp.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	met := metrics.New()
	o := &opener{mgr: peer.New(nil, []config.Partner{{Name: "b"}}, exp, met, log), exp: exp, met: met}
	if !o.Known("local") || !o.Known("b") || o.Known("x") {
		t.Fatal("Known")
	}
	if _, _, err := o.Open(context.Background(), "x", wire.Target{}); err == nil {
		t.Fatal("unknown partner opened")
	}
	if _, _, err := o.Open(context.Background(), "b", wire.Target{}); err == nil {
		t.Fatal("unconnected partner opened")
	}
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	c, st, err := o.Open(context.Background(), "local", wire.Target{Proto: wire.TCP, Port: port, Name: "web"})
	if err != nil || st != wire.StatusOK {
		t.Fatal(err, st)
	}
	_, _ = c.Write([]byte("hi"))
	b := make([]byte, 2)
	_, _ = io.ReadFull(c, b)
	_ = c.(interface{ CloseWrite() error }).CloseWrite()
	c.Close()
	c.Close()
	if _, st, _ := o.Open(context.Background(), "local", wire.Target{Proto: wire.TCP, Port: port, Name: "nope"}); st == wire.StatusOK {
		t.Fatal("unknown local target opened")
	}
	var sb strings.Builder
	_ = met.Write(&sb)
	if !strings.Contains(sb.String(), `switchyard_bytes_total{peer="local",direction="in",container="web"} 2`) ||
		!strings.Contains(sb.String(), `switchyard_streams_total{peer="local",result="refused"} 1`) {
		t.Fatal(sb.String())
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestServe(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	f.Set(dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"apps": {NetworkID: "n1", IPAddress: "127.0.0.2"}}))
	cfg, err := config.Parse([]byte("private_key: " + key + "\nlisten: " + strconv.Itoa(freePort(t)) +
		"\nmetrics: " + strconv.Itoa(freePort(t)) + "\npartners:\n  - name: b\n    address: 127.0.0.1:1\n" +
		"    public_key: iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w=\n"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, cfg, log, f.Client(), "self") }()
	url := "http://127.0.0.1:" + strconv.Itoa(cfg.Metrics) + "/metrics"
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(b), `switchyard_peer_up{peer="b"} 0`) {
				t.Fatal(string(b))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("metrics not served")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestServeErrors(t *testing.T) {
	cfg, _ := config.Parse([]byte("private_key: " + key + "\n"))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	old := dockertest.NewFake()
	old.VersionAPI = "1.40"
	defer old.Close()
	if serve(ctx, cfg, log, old.Client(), "self") != 1 {
		t.Fatal("old API accepted")
	}

	f := dockertest.NewFake()
	defer f.Close()
	if serve(ctx, cfg, log, f.Client(), "self") != 1 {
		t.Fatal("missing own container accepted")
	}
	f.Set(dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"apps": {NetworkID: "n1"}}))
	if serve(ctx, cfg, log, f.Client(), "self") != 1 {
		t.Fatal("own container without IPv4 accepted")
	}

	dead := docker.NewWithDialer(func(context.Context) (net.Conn, error) { return nil, io.EOF })
	if serve(ctx, cfg, log, dead, "self") != 1 {
		t.Fatal("unreachable Docker accepted")
	}
}

func TestLocalConnCloseWritePlain(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := &localConn{Conn: a, met: metrics.New(), untrack: func() {}}
	if c.CloseWrite() != nil {
		t.Fatal("plain conn CloseWrite")
	}
	c.Close()
}

func TestCLI(t *testing.T) {
	startTimeout = 2 * time.Second
	f := filepath.Join(t.TempDir(), "c.yml")
	_ = os.WriteFile(f, []byte("private_key: "+key+"\nmetrics: 1\n"), 0o644)
	cases := map[string]struct {
		args []string
		want int
	}{
		"genkey":       {[]string{"genkey"}, 0},
		"unknown":      {[]string{"bogus"}, 2},
		"bad flag":     {[]string{"-nope"}, 2},
		"health":       {[]string{"-config", f, "health"}, 1},
		"run bad conf": {[]string{"-config", filepath.Join(t.TempDir(), "missing")}, 1},
		// No Docker socket here: the agent stops with an error.
		"run no docker": {[]string{"-config", f}, 1},
	}
	for name, c := range cases {
		if got := cli(c.args); got != c.want {
			t.Errorf("%s: exit %d, want %d", name, got, c.want)
		}
	}
	r, w, _ := os.Pipe()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	_, _ = w.WriteString(key)
	w.Close()
	if cli([]string{"pubkey"}) != 0 {
		t.Fatal("pubkey")
	}
}

func TestServeListenErrors(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	f.Set(dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"apps": {NetworkID: "n1", IPAddress: "127.0.0.2"}}))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	busy, _ := net.Listen("tcp", "127.0.0.2:0")
	defer busy.Close()
	bp := strconv.Itoa(busy.Addr().(*net.TCPAddr).Port)

	cfg, _ := config.Parse([]byte("private_key: " + key + "\nlisten: " + bp + "\n"))
	if serve(context.Background(), cfg, log, f.Client(), "self") != 1 {
		t.Fatal("busy partner port accepted")
	}
	cfg, _ = config.Parse([]byte("private_key: " + key + "\nlisten: " + strconv.Itoa(freePort(t)) + "\nmetrics: " + bp + "\n"))
	if serve(context.Background(), cfg, log, f.Client(), "self") != 1 {
		t.Fatal("busy metrics port accepted")
	}
	cfg, _ = config.Parse([]byte("private_key: " + key + "\nlisten: " + strconv.Itoa(freePort(t)) +
		"\nmetrics: " + strconv.Itoa(freePort(t)) + "\nimport_network: missing\n"))
	if serve(context.Background(), cfg, log, f.Client(), "self") != 1 {
		t.Fatal("import setup error accepted")
	}
}

func TestHealthNot200(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	f := filepath.Join(t.TempDir(), "c.yml")
	_ = os.WriteFile(f, []byte("private_key: "+key+"\nmetrics: "+strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)+"\n"), 0o644)
	if health(f) != 1 {
		t.Fatal("404 reported healthy")
	}
}

func TestLoadImportPlatform(t *testing.T) {
	f := filepath.Join(t.TempDir(), "c.yml")
	_ = os.WriteFile(f, []byte("private_key: "+key+"\nimport_network: imp\n"), 0o644)
	_, err := load(f)
	if (err == nil) != (runtime.GOOS == "linux") {
		t.Fatalf("%s: %v", runtime.GOOS, err)
	}
}

func TestPubkeyReadError(t *testing.T) {
	r, w, _ := os.Pipe()
	w.Close()
	r.Close() // reading a closed file fails
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	if pubkey() != 1 {
		t.Fatal("read error ignored")
	}
}

// main itself, in a child process (os.Exit); its coverage goes to the same directory.
func TestMainProcess(t *testing.T) {
	if os.Getenv("SY_MAIN") == "1" {
		os.Args = []string{"switchyard", "bogus"}
		main()
		return
	}
	args := []string{"-test.run=^TestMainProcess$"}
	if f := flag.Lookup("test.gocoverdir"); f != nil && f.Value.String() != "" {
		args = append(args, "-test.gocoverdir="+f.Value.String())
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "SY_MAIN=1")
	if f := flag.Lookup("test.gocoverdir"); f != nil && f.Value.String() != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+f.Value.String()) // written by os.Exit
	}
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("exit: %v", err)
	}
}

func TestServeTLSError(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	f.Set(dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"apps": {NetworkID: "n1", IPAddress: "127.0.0.2"}}))
	old := newLocal
	defer func() { newLocal = old }()
	newLocal = func(ed25519.PrivateKey, []wire.Partner) (*wire.Local, error) {
		return nil, errors.New("no certificate")
	}
	cfg, _ := config.Parse([]byte("private_key: " + key + "\n"))
	if serve(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), f.Client(), "self") != 1 {
		t.Fatal("certificate error ignored")
	}
}

// The metrics listener breaks while the agent runs: the agent stops with an error.
func TestServeMetricsBreaks(t *testing.T) {
	f := dockertest.NewFake()
	defer f.Close()
	f.Set(dockertest.Ctr("self", "agent", nil, map[string]docker.Endpoint{"apps": {NetworkID: "n1", IPAddress: "127.0.0.2"}}))
	mport := freePort(t)
	got := make(chan net.Listener, 4)
	old := listen
	defer func() { listen = old }()
	listen = func(n, a string) (net.Listener, error) {
		l, err := old(n, a)
		if err == nil && strings.HasSuffix(a, ":"+strconv.Itoa(mport)) {
			got <- l
		}
		return l, err
	}
	cfg, _ := config.Parse([]byte("private_key: " + key + "\nlisten: " + strconv.Itoa(freePort(t)) +
		"\nmetrics: " + strconv.Itoa(mport) + "\n"))
	done := make(chan int, 1)
	go func() {
		done <- serve(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), f.Client(), "self")
	}()
	(<-got).Close()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent kept running")
	}
}
