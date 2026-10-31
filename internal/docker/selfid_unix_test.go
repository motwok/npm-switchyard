//go:build !windows

package docker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelfFromMountinfo(t *testing.T) {
	old := mountinfo
	defer func() { mountinfo = old }()
	f := filepath.Join(t.TempDir(), "mountinfo")
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	_ = os.WriteFile(f, []byte("1 2 0:1 /var/lib/docker/containers/"+id+"/hostname /etc/hostname rw\n"), 0o644)
	mountinfo = f
	if selfFromMountinfo() != id {
		t.Fatal("id not found")
	}
	mountinfo = filepath.Join(t.TempDir(), "missing")
	if selfFromMountinfo() != "" {
		t.Fatal("missing file")
	}
}
