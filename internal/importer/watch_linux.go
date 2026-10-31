//go:build linux

package importer

import (
	"context"
	"syscall"
	"time"
)

// watchNPM re-reads NPM's files on every change in its directories (NPM-1).
func (im *Importer) watchNPM(ctx context.Context) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		im.log.Error("inotify", "err", err)
		return
	}
	defer syscall.Close(fd)
	const mask = syscall.IN_CREATE | syscall.IN_DELETE | syscall.IN_MOVED_FROM | syscall.IN_MOVED_TO |
		syscall.IN_CLOSE_WRITE
	for _, d := range im.npm.Dirs() {
		if _, err := syscall.InotifyAddWatch(fd, d, mask); err != nil {
			im.log.Warn("inotify watch", "dir", d, "err", err)
		}
	}
	go func() {
		<-ctx.Done()
		syscall.Close(fd)
	}()
	buf := make([]byte, 64*1024)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil || n <= 0 {
			return
		}
		// NPM deletes, writes and renames in quick succession; settle briefly.
		time.Sleep(200 * time.Millisecond)
		im.npm.Reload()
		im.syncShort()
	}
}
