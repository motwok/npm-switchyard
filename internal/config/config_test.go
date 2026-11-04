package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

var (
	own   = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	peer  = "iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w="
	peer2 = "yFQXL7bB0dkYQWrHRlz1CMYiNQ7S5O5JWbqvnmqUv8U="
	head  = "private_key: " + own + "\n"
)

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(head + "partners:\n  - name: HostB\n    public_key: " + peer + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != 9481 || c.Metrics != 9480 || c.NPMData != "/npm" || c.LogLevel != "info" ||
		c.LogTime || c.LogThrottle != DefaultLogThrottle {
		t.Fatalf("defaults: %+v", c)
	}
	p := c.Partners[0]
	if p.Name != "hostb" || p.SpareCount() != 2 || len(p.Peer) != 32 || !p.Own.Equal(c.Key) {
		t.Fatalf("partner: %+v", p)
	}
}

func TestOverride(t *testing.T) {
	other := GenerateKey()
	c, err := Parse([]byte(head + "partners:\n  - name: b\n    public_key: " + peer + "\n    private_key: " + other + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Partners[0].Own.Equal(c.Key) {
		t.Fatal("per-partner key not used")
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]string{
		"no private key":   "partners: []\n",
		"bad private key":  "private_key: c2hvcnQ=\n",
		"unknown key":      head + "bogus: 1\n",
		"reserved name":    head + "partners:\n  - name: local\n    public_key: " + peer + "\n",
		"port name":        head + "partners:\n  - name: \"8080\"\n    public_key: " + peer + "\n",
		"bad label":        head + "partners:\n  - name: a_b\n    public_key: " + peer + "\n",
		"twice":            head + "partners:\n  - name: a\n    public_key: " + peer + "\n  - name: a\n    public_key: " + peer2 + "\n",
		"same key":         head + "partners:\n  - name: a\n    public_key: " + peer + "\n  - name: b\n    public_key: " + peer + "\n",
		"no public key":    head + "partners:\n  - name: a\n",
		"bad host_address": head + "host_address: nope\n",
		"bad override":     head + "partners:\n  - name: a\n    public_key: " + peer + "\n    private_key: x\n",
		"same ports":       head + "listen: 9000\nmetrics: 9000\n",
		"port range":       head + "listen: 70000\n",
		"no port":          head + "partners:\n  - name: a\n    address: hostb\n    public_key: " + peer + "\n",
		"log level":        head + "log_level: loud\n",
		"bad log_throttle": head + "log_throttle: soon\n",
		"neg log_throttle": head + "log_throttle: -1s\n",
		"neg spares":       head + "partners:\n  - name: a\n    public_key: " + peer + "\n    spares: -1\n",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestKeys(t *testing.T) {
	k, err := ParsePrivateKey(GenerateKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePublicKey(PublicKeyString(k)); err != nil {
		t.Fatal(err)
	}
}

func TestSource(t *testing.T) {
	env := map[string]string{"SWITCHYARD_CONFIG": "listen: 1"}
	b, src, err := Source("", func(k string) string { return env[k] })
	if err != nil || src != "SWITCHYARD_CONFIG" || !strings.Contains(string(b), "listen") {
		t.Fatal(src, err)
	}
	if _, _, err := Source("", func(string) string { return "" }); err == nil {
		t.Fatal("expected error without any source")
	}
}

func TestSpareCount(t *testing.T) {
	n := 0
	if (Partner{Spares: &n}).SpareCount() != 0 || (Partner{}).SpareCount() != DefaultSpares {
		t.Fatal("spares")
	}
	if !ValidLabel("a-b") || ValidLabel("-a") || ValidLabel("") {
		t.Fatal("labels")
	}
}

func TestSourceFiles(t *testing.T) {
	f := t.TempDir() + "/c.yml"
	if err := os.WriteFile(f, []byte("listen: 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, src, err := Source(f, func(string) string { return "" }); err != nil || src != f {
		t.Fatal(src, err)
	}
	env := map[string]string{"SWITCHYARD_CONFIG_FILE": f}
	if _, src, err := Source("", func(k string) string { return env[k] }); err != nil || src != f {
		t.Fatal(src, err)
	}
}

func TestHost(t *testing.T) {
	two := "partners:\n  - name: a\n    public_key: " + peer + "\n  - name: b\n    public_key: " + peer2 + "\n"
	c, err := Parse([]byte(head + two))
	if err != nil {
		t.Fatal(err)
	}
	if c.Partners[0].Allows(HostScope) || c.LocalAllows(HostScope) {
		t.Fatal("host on by default")
	}
	if !c.Partners[0].Allows("any") || !c.LocalAllows("any") {
		t.Fatal("networks")
	}
	c, err = Parse([]byte(head + "host: true\nlocal_host: false\nhost_address: 192.168.1.10\n" + two + "    host: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Partners[0].Allows(HostScope) || c.Partners[1].Allows(HostScope) || c.LocalAllows(HostScope) {
		t.Fatal("overrides")
	}
	c, err = Parse([]byte(head + "local_host: true\n" + two))
	if err != nil {
		t.Fatal(err)
	}
	if !c.LocalAllows(HostScope) || c.Partners[0].Allows(HostScope) {
		t.Fatal("local_host")
	}
}

func TestLogKeys(t *testing.T) {
	c, err := Parse([]byte(head + "log_time: true\nlog_throttle: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.LogTime || c.LogThrottle != 0 {
		t.Fatalf("log keys: %+v", c)
	}
	if c, _ = Parse([]byte(head + "log_throttle: 2m\n")); c.LogThrottle != Duration(2*time.Minute) {
		t.Fatalf("log_throttle: %v", c.LogThrottle)
	}
}
