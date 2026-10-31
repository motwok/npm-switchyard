package peer

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/motwok/npm-switchyard/internal/wire"
)

// UDPIdle closes a flow after this long without traffic in either direction (TRN-3).
var UDPIdle = 60 * time.Second

type closeWriter interface{ CloseWrite() error }

// netConner exposes the socket under a TLS or wrapping connection.
type netConner interface{ NetConn() net.Conn }

// reset makes the next Close of c a TCP reset instead of an orderly close (TRN-2).
func reset(c net.Conn) {
	for {
		switch x := c.(type) {
		case *net.TCPConn:
			_ = x.SetLinger(0)
			return
		case netConner:
			c = x.NetConn()
		default:
			return
		}
	}
}

// Pipe copies both ways with half-close (TRN-2). count gets "in" for bytes towards a and
// "out" for bytes towards b. An error on one side, such as a reset, resets the other.
func Pipe(a, b net.Conn, count func(dir string, n int64)) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn, dir string) {
		defer wg.Done()
		n, err := io.Copy(dst, src)
		count(dir, n)
		if err != nil {
			reset(a)
			reset(b)
			a.Close()
			b.Close()
			return
		}
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b, "in")
	go cp(b, a, "out")
	wg.Wait()
	a.Close()
	b.Close()
}

// Idle closes a UDP flow after UDPIdle without traffic. Deadlines are not used on the
// stream because a timeout in the middle of a frame would lose framing.
type Idle struct {
	last atomic.Int64
	stop chan struct{}
	once sync.Once
}

func WatchIdle(closers ...io.Closer) *Idle {
	i := &Idle{stop: make(chan struct{})}
	i.Touch()
	limit := UDPIdle
	go func() {
		t := time.NewTicker(limit / 12)
		defer t.Stop()
		for {
			select {
			case <-i.stop:
				return
			case <-t.C:
				if time.Since(time.Unix(0, i.last.Load())) >= limit {
					for _, c := range closers {
						c.Close()
					}
					return
				}
			}
		}
	}()
	return i
}

func (i *Idle) Touch() { i.last.Store(time.Now().UnixNano()) }

func (i *Idle) Stop() { i.once.Do(func() { close(i.stop) }) }

// PipeUDPExport relays length-prefixed datagrams on s to the connected UDP socket u.
func PipeUDPExport(s net.Conn, u net.Conn, count func(dir string, n int64)) {
	idle := WatchIdle(s, u)
	defer idle.Stop()
	defer s.Close()
	defer u.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65535)
		for {
			n, err := u.Read(buf)
			if err != nil {
				s.Close()
				return
			}
			idle.Touch()
			if wire.WriteDatagram(s, buf[:n]) != nil {
				return
			}
			count("in", int64(n))
		}
	}()
	buf := make([]byte, 65535)
	for {
		n, err := wire.ReadDatagram(s, buf)
		if err != nil {
			break
		}
		idle.Touch()
		if _, err := u.Write(buf[:n]); err != nil {
			break
		}
		count("out", int64(n))
	}
	u.Close() // ends the reader at once, e.g. after a failed send
	<-done
}
