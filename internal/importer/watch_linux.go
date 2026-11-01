//go:build linux

package importer

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// watchNPM re-reads NPM's files on every change in its directories (NPM-1). Directories NPM
// has not created yet, or that were deleted and created again, are picked up by the poll.
func (im *Importer) watchNPM(ctx context.Context) {
	poll := im.tune.npmPoll
	// Non-blocking and wrapped in os.File: closing it interrupts a pending Read, a raw
	// syscall.Read would block forever.
	fd, err := im.tune.inotifyInit(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		im.log.Error("inotify", "err", err)
		return
	}
	f := os.NewFile(uintptr(fd), "inotify")
	go func() {
		<-ctx.Done()
		f.Close()
	}()
	const mask = syscall.IN_CREATE | syscall.IN_DELETE | syscall.IN_MOVED_FROM | syscall.IN_MOVED_TO |
		syscall.IN_CLOSE_WRITE | syscall.IN_DELETE_SELF
	var mu sync.Mutex
	watched := map[string]bool{}
	dirs := map[int32]string{} // watch descriptor -> directory
	warned := map[string]bool{}
	go func() {
		for ctx.Err() == nil {
			added := false
			for _, d := range im.npm.Dirs() {
				mu.Lock()
				w := watched[d]
				mu.Unlock()
				if w {
					continue
				}
				wd, err := syscall.InotifyAddWatch(fd, d, mask)
				if err != nil {
					if !errors.Is(err, syscall.ENOENT) && !warned[d] {
						warned[d] = true
						im.log.Warn("inotify watch", "dir", d, "err", err)
					}
					continue
				}
				mu.Lock()
				watched[d] = true
				dirs[int32(wd)] = d
				mu.Unlock()
				added = true
			}
			if added {
				im.reloadNPM()
				im.syncShort()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(poll):
			}
		}
	}()
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		if err != nil || n <= 0 {
			return
		}
		for _, ev := range inotifyEvents(buf[:n]) {
			mu.Lock()
			d, ok := dirs[ev.wd]
			if ev.mask&syscall.IN_IGNORED == 0 {
				mu.Unlock()
				im.log.Debug("NPM file changed", "dir", d, "file", ev.name)
				continue
			}
			// The directory is gone; the poll watches it again once it reappears.
			if ok {
				delete(watched, d)
				delete(dirs, ev.wd)
				im.log.Info("NPM directory gone, waiting for it", "dir", d)
			}
			mu.Unlock()
		}
		// NPM deletes, writes and renames in quick succession; settle briefly.
		time.Sleep(200 * time.Millisecond)
		im.reloadNPM()
		im.syncShort()
	}
}

type inotifyEvent struct {
	wd   int32
	mask uint32
	name string
}

// inotifyEvents splits a read into its events (struct inotify_event: wd, mask, cookie, len,
// name[len]).
func inotifyEvents(b []byte) []inotifyEvent {
	var out []inotifyEvent
	for len(b) >= syscall.SizeofInotifyEvent {
		end := min(len(b), syscall.SizeofInotifyEvent+int(binary.NativeEndian.Uint32(b[12:])))
		out = append(out, inotifyEvent{
			wd:   int32(binary.NativeEndian.Uint32(b[0:])),
			mask: binary.NativeEndian.Uint32(b[4:]),
			name: strings.TrimRight(string(b[syscall.SizeofInotifyEvent:end]), "\x00"),
		})
		b = b[end:]
	}
	return out
}

func inotifyInit1(flags int) (int, error) { return syscall.InotifyInit1(flags) }
