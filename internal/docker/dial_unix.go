//go:build !windows

package docker

import (
	"bufio"
	"context"
	"net"
	"os"
	"regexp"
)

const socket = "/var/run/docker.sock"

// mountinfo is a variable so tests can point it elsewhere.
var mountinfo = "/proc/self/mountinfo"

func dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", socket)
}

var containerDir = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// The hostname may be overridden; mountinfo shows Docker's per-container paths.
func selfFromMountinfo() string {
	f, err := os.Open(mountinfo)
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if m := containerDir.FindStringSubmatch(s.Text()); m != nil {
			return m[1]
		}
	}
	return ""
}
