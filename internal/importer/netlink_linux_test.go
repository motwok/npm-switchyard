//go:build linux

package importer

import (
	"encoding/binary"
	"errors"
	"net"
	"syscall"
	"testing"
)

// fakeNetlink replaces the syscalls; reply is what Recvfrom returns.
func fakeNetlink(t *testing.T, sockErr, bindErr, sendErr, recvErr error, reply []byte) {
	t.Helper()
	so, bi, se, re := sysSocket, sysBind, sysSendto, sysRecvfrom
	t.Cleanup(func() { sysSocket, sysBind, sysSendto, sysRecvfrom = so, bi, se, re })
	sysSocket = func(int, int, int) (int, error) { return -1, sockErr }
	sysBind = func(int, syscall.Sockaddr) error { return bindErr }
	sysSendto = func(int, []byte, int, syscall.Sockaddr) error { return sendErr }
	sysRecvfrom = func(_ int, b []byte, _ int) (int, syscall.Sockaddr, error) {
		return copy(b, reply), nil, recvErr
	}
}

// nlMsg builds one netlink message of the given type with an errno payload.
func nlMsg(typ uint16, errno int32) []byte {
	b := make([]byte, syscall.NLMSG_HDRLEN+4+syscall.NLMSG_HDRLEN)
	binary.NativeEndian.PutUint32(b[0:], uint32(len(b)))
	binary.NativeEndian.PutUint16(b[4:], typ)
	binary.NativeEndian.PutUint32(b[syscall.NLMSG_HDRLEN:], uint32(errno))
	return b
}

func TestAddAlias(t *testing.T) {
	ip := net.IPv4(10, 0, 0, 5)
	boom := errors.New("boom")
	if addAlias(1, net.ParseIP("fd00::1"), 64) == nil {
		t.Fatal("IPv6 alias accepted")
	}
	cases := []struct {
		name                   string
		sock, bind, send, recv error
		reply                  []byte
		ok                     bool
	}{
		{"socket", boom, nil, nil, nil, nil, false},
		{"bind", nil, boom, nil, nil, nil, false},
		{"send", nil, nil, boom, nil, nil, false},
		{"recv", nil, nil, nil, boom, nil, false},
		{"garbage", nil, nil, nil, nil, badLength(), false},
		{"ack", nil, nil, nil, nil, nlMsg(syscall.NLMSG_ERROR, 0), true},
		{"exists", nil, nil, nil, nil, nlMsg(syscall.NLMSG_ERROR, -int32(syscall.EEXIST)), true},
		{"denied", nil, nil, nil, nil, nlMsg(syscall.NLMSG_ERROR, -int32(syscall.EPERM)), false},
		{"no error message", nil, nil, nil, nil, nlMsg(syscall.NLMSG_DONE, 0), true},
	}
	for _, c := range cases {
		fakeNetlink(t, c.sock, c.bind, c.send, c.recv, c.reply)
		if err := addAlias(1, ip, 24); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// Removing an alias tolerates an address that is already gone, nothing else.
func TestDelAlias(t *testing.T) {
	ip := net.IPv4(10, 0, 0, 5)
	fakeNetlink(t, nil, nil, nil, nil, nlMsg(syscall.NLMSG_ERROR, -int32(syscall.EADDRNOTAVAIL)))
	if err := delAlias(1, ip, 24); err != nil {
		t.Fatal(err)
	}
	fakeNetlink(t, nil, nil, nil, nil, nlMsg(syscall.NLMSG_ERROR, -int32(syscall.EEXIST)))
	if delAlias(1, ip, 24) == nil {
		t.Fatal("EEXIST accepted for delete")
	}
}

// badLength is a netlink header that claims more bytes than there are.
func badLength() []byte {
	b := make([]byte, syscall.NLMSG_HDRLEN)
	binary.NativeEndian.PutUint32(b[0:], 100)
	return b
}
