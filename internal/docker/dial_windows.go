//go:build windows

package docker

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

const pipe = `\\.\pipe\docker_engine`

func dial(ctx context.Context) (net.Conn, error) {
	return winio.DialPipeContext(ctx, pipe)
}

func selfFromMountinfo() string { return "" }
