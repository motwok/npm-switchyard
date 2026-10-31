package config

import (
	"fmt"
	"path"
)

// Networks limits which shared networks a partner (or local) may reach. Without Include
// every shared network counts; Exclude always wins. Patterns use * as wildcard.
type Networks struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

func (n Networks) validate() error {
	for _, p := range append(append([]string{}, n.Include...), n.Exclude...) {
		if p == "" {
			return fmt.Errorf("empty network pattern")
		}
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("network pattern %q: %w", p, err)
		}
	}
	return nil
}

func match(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// Allows reports whether containers in the named network may be reached.
func (n Networks) Allows(name string) bool {
	if match(n.Exclude, name) {
		return false
	}
	return len(n.Include) == 0 || match(n.Include, name)
}
