// Package npmconf reads forward hosts from NPM's generated nginx files (spec 9).
package npmconf

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/motwok/npm-switchyard/internal/names"
)

var (
	reServer   = regexp.MustCompile(`(?m)^\s*set\s+\$server\s+"([^"]+)"\s*;`)
	rePort     = regexp.MustCompile(`(?m)^\s*set\s+\$port\s+(\d+)\s*;`)
	reLocation = regexp.MustCompile(`(?m)^\s*proxy_pass\s+[a-z]+://([^:/\s;]+):(\d+)[^;]*;`)
	reStream   = regexp.MustCompile(`(?m)^\s*proxy_pass\s+([^:/\s;]+):(\d+)\s*;`)
)

// Parse returns lowercase .switchyard forward host -> ports found in one file (NPM-3).
func Parse(content string) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	add := func(host, port string) {
		host = strings.ToLower(host)
		if !names.HasSuffix(host) {
			return
		}
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return
		}
		if out[host] == nil {
			out[host] = map[int]bool{}
		}
		out[host][p] = true
	}
	if s := reServer.FindStringSubmatch(content); s != nil {
		if p := rePort.FindStringSubmatch(content); p != nil {
			add(s[1], p[1])
		}
	}
	for _, m := range reLocation.FindAllStringSubmatch(content, -1) {
		add(m[1], m[2])
	}
	for _, m := range reStream.FindAllStringSubmatch(content, -1) {
		add(m[1], m[2])
	}
	return out
}

// Store holds the forward hosts of all active NPM files.
type Store struct {
	dir string

	mu    sync.Mutex
	hosts map[string]map[int]bool
}

func NewStore(npmData string) *Store {
	return &Store{dir: filepath.Join(npmData, "nginx"), hosts: map[string]map[int]bool{}}
}

func (s *Store) Dirs() []string {
	return []string{filepath.Join(s.dir, "proxy_host"), filepath.Join(s.dir, "stream")}
}

// Available reports whether NPM's directories are mounted (NPM-1).
func (s *Store) Available() bool {
	for _, d := range s.Dirs() {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

// Reload re-reads all *.conf files; .conf.err and others are ignored (NPM-2).
func (s *Store) Reload() {
	hosts := map[string]map[int]bool{}
	for _, d := range s.Dirs() {
		files, _ := filepath.Glob(filepath.Join(d, "*.conf"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			for h, ports := range Parse(string(b)) {
				if hosts[h] == nil {
					hosts[h] = map[int]bool{}
				}
				for p := range ports {
					hosts[h][p] = true
				}
			}
		}
	}
	s.mu.Lock()
	s.hosts = hosts
	s.mu.Unlock()
}

// Ports of a short name, nil if unknown.
func (s *Store) Ports(name string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.hosts[strings.ToLower(name)]
	if m == nil {
		return nil
	}
	out := make([]int, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	return out
}

// Names lists all short names currently configured.
func (s *Store) Names() map[string][]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]int{}
	for h, ports := range s.hosts {
		if n, ok := names.Parse(h); ok && n.Short() {
			for p := range ports {
				out[h] = append(out[h], p)
			}
		}
	}
	return out
}
