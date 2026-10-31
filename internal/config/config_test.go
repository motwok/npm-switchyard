package config

import (
	"strings"
	"testing"
)

const psk1 = "q83vEi4DQ+X0g7yW1o0f3o8Yv2s6s7b3H3r3nqk6c4k="
const psk2 = "Zm9vYmFyYmF6cXV4cXV1eHF1dXhxdXV4cXV1eHF1dXg="

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte("partners:\n  - name: HostB\n    psk: " + psk1 + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != 9481 || c.Metrics != 9480 || c.NPMData != "/npm" || c.LogLevel != "info" {
		t.Fatalf("defaults: %+v", c)
	}
	if c.Partners[0].Name != "hostb" || c.Partners[0].SpareCount() != 2 || len(c.Partners[0].Key) != 32 {
		t.Fatalf("partner: %+v", c.Partners[0])
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]string{
		"unknown key":   "bogus: 1\n",
		"reserved name": "partners:\n  - name: local\n    psk: " + psk1 + "\n",
		"port name":     "partners:\n  - name: \"8080\"\n    psk: " + psk1 + "\n",
		"bad label":     "partners:\n  - name: a_b\n    psk: " + psk1 + "\n",
		"twice":         "partners:\n  - name: a\n    psk: " + psk1 + "\n  - name: a\n    psk: " + psk2 + "\n",
		"same psk":      "partners:\n  - name: a\n    psk: " + psk1 + "\n  - name: b\n    psk: " + psk1 + "\n",
		"short psk":     "partners:\n  - name: a\n    psk: c2hvcnQ=\n",
		"not base64":    "partners:\n  - name: a\n    psk: \"%%%\"\n",
		"same ports":    "listen: 9000\nmetrics: 9000\n",
		"port range":    "listen: 70000\n",
		"no port":       "partners:\n  - name: a\n    address: hostb\n    psk: " + psk1 + "\n",
		"log level":     "log_level: loud\n",
		"neg spares":    "partners:\n  - name: a\n    psk: " + psk1 + "\n    spares: -1\n",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: no error", name)
		}
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
