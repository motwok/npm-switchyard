//go:build linux

package importer

import (
	"context"
	"syscall"
	"time"
)

// sysInotifyInit is replaceable in tests; npmPoll is how often missing directories are retried.
var (
	sysInotifyInit = syscall.InotifyInit1
	npmPoll        = 5 * time.Second
)

// watchNPM re-reads NPM's files on every change in its directories (NPM-1). Directories NPM
// has not created yet are picked up as soon as they appear.
func (im *Importer) watchNPM(ctx context.Context) {
	poll := npmPoll
	fd, err := sysInotifyInit(syscall.IN_CLOEXEC)
	if err != nil {
		im.log.Error("inotify", "err", err)
		return
	}
	go func() {
		<-ctx.Done()
		syscall.Close(fd)
	}()
	const mask = syscall.IN_CREATE | syscall.IN_DELETE | syscall.IN_MOVED_FROM | syscall.IN_MOVED_TO |
		syscall.IN_CLOSE_WRITE
	go func() {
		watched := map[string]bool{}
		for ctx.Err() == nil {
			added := false
			for _, d := range im.npm.Dirs() {
				if watched[d] {
					continue
				}
				if _, err := syscall.InotifyAddWatch(fd, d, mask); err == nil {
					watched[d] = true
					added = true
				}
			}
			if added {
				im.npm.Reload()
				im.syncShort()
			}
			if len(watched) == len(im.npm.Dirs()) {
				return
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
