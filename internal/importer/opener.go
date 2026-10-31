package importer

import (
	"context"
	"net"

	"github.com/motwok/npm-switchyard/internal/wire"
)

// Opener opens a stream for a name to a partner or to the agent itself (IMP-9).
type Opener interface {
	Open(ctx context.Context, partner string, t wire.Target) (net.Conn, byte, error)
	Known(partner string) bool
}
