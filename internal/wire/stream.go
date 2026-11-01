package wire

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// StreamInfo identifies a stream in the logs of both agents (LOG-3).
type StreamInfo struct {
	ID   string
	Peer string
}

type streamKey struct{}

func WithStream(ctx context.Context, s StreamInfo) context.Context {
	return context.WithValue(ctx, streamKey{}, s)
}

func StreamFrom(ctx context.Context) StreamInfo {
	s, _ := ctx.Value(streamKey{}).(StreamInfo)
	return s
}

// StreamID is derived from the TLS session, so both ends of the connection get the same ID
// without sending it.
func (c *Conn) StreamID() string {
	cs := c.ConnectionState()
	b, _ := cs.ExportKeyingMaterial("switchyard stream id", nil, 4)
	return hex.EncodeToString(b)
}

// NewStreamID is for streams that do not cross a partner connection.
func NewStreamID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
