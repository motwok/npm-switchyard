// Package config loads and validates the agent configuration (spec 4).
package config

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultListen  = 9481
	DefaultMetrics = 9480
	DefaultNPMData = "/npm"
	DefaultSpares  = 2
	LocalPartner   = "local"
)

type Partner struct {
	Name       string `yaml:"name"`
	Address    string `yaml:"address"`
	PublicKey  string `yaml:"public_key"`
	PrivateKey string `yaml:"private_key"` // overrides the global key for this partner
	Spares     *int   `yaml:"spares"`

	Own  ed25519.PrivateKey `yaml:"-"` // own key towards this partner
	Peer ed25519.PublicKey  `yaml:"-"`
}

func (p Partner) SpareCount() int {
	if p.Spares == nil {
		return DefaultSpares
	}
	return *p.Spares
}

type Config struct {
	ImportNetwork string    `yaml:"import_network"`
	Listen        int       `yaml:"listen"`
	Metrics       int       `yaml:"metrics"`
	NPMData       string    `yaml:"npm_data"`
	LogLevel      string    `yaml:"log_level"`
	PrivateKey    string    `yaml:"private_key"`
	Partners      []Partner `yaml:"partners"`

	Key ed25519.PrivateKey `yaml:"-"`
}

// Source resolves CFG-1: flag, then SWITCHYARD_CONFIG_FILE, then SWITCHYARD_CONFIG.
func Source(flagPath string, getenv func(string) string) ([]byte, string, error) {
	if flagPath != "" {
		b, err := os.ReadFile(flagPath)
		return b, flagPath, err
	}
	if p := getenv("SWITCHYARD_CONFIG_FILE"); p != "" {
		b, err := os.ReadFile(p)
		return b, p, err
	}
	if c := getenv("SWITCHYARD_CONFIG"); c != "" {
		return []byte(c), "SWITCHYARD_CONFIG", nil
	}
	return nil, "", errors.New("no configuration: set -config, SWITCHYARD_CONFIG_FILE or SWITCHYARD_CONFIG")
}

func Parse(data []byte) (*Config, error) {
	c := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if c.Listen == 0 {
		c.Listen = DefaultListen
	}
	if c.Metrics == 0 {
		c.Metrics = DefaultMetrics
	}
	if c.NPMData == "" {
		c.NPMData = DefaultNPMData
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level %q: want debug, info, warn or error", c.LogLevel)
	}
	for _, p := range []struct {
		name string
		v    int
	}{{"listen", c.Listen}, {"metrics", c.Metrics}} {
		if p.v < 1 || p.v > 65535 {
			return fmt.Errorf("%s %d: port out of range", p.name, p.v)
		}
	}
	if c.Listen == c.Metrics {
		return errors.New("listen and metrics use the same port")
	}
	key, err := ParsePrivateKey(c.PrivateKey)
	if err != nil {
		return fmt.Errorf("private_key: %w", err)
	}
	c.Key = key
	names := map[string]bool{}
	peers := map[string]string{}
	for i := range c.Partners {
		p := &c.Partners[i]
		p.Name = strings.ToLower(p.Name)
		if !ValidPartnerName(p.Name) {
			return fmt.Errorf("partner %q: not a DNS label, a port number or reserved", p.Name)
		}
		if names[p.Name] {
			return fmt.Errorf("partner %q: used twice", p.Name)
		}
		names[p.Name] = true
		if p.Peer, err = ParsePublicKey(p.PublicKey); err != nil {
			return fmt.Errorf("partner %q: public_key: %w", p.Name, err)
		}
		if other, ok := peers[string(p.Peer)]; ok {
			return fmt.Errorf("partners %q and %q: same public_key", other, p.Name)
		}
		peers[string(p.Peer)] = p.Name
		p.Own = c.Key
		if p.PrivateKey != "" {
			if p.Own, err = ParsePrivateKey(p.PrivateKey); err != nil {
				return fmt.Errorf("partner %q: private_key: %w", p.Name, err)
			}
		}
		if p.Spares != nil && *p.Spares < 0 {
			return fmt.Errorf("partner %q: spares < 0", p.Name)
		}
		if p.Address != "" {
			if _, port, ok := strings.Cut(p.Address, ":"); !ok || port == "" {
				return fmt.Errorf("partner %q: address %q needs host:port", p.Name, p.Address)
			}
		}
	}
	return nil
}

// ValidPartnerName: DNS label, not a port number, not "local".
func ValidPartnerName(s string) bool {
	if s == LocalPartner || !ValidLabel(s) {
		return false
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= 65535 {
		return false
	}
	return true
}

func ValidLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
