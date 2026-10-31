package wire

import (
	"crypto/tls"
	"net"
)

func tlsServer(c net.Conn, l *Local) *tls.Conn { return tls.Server(c, l.serverConfig()) }
