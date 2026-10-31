//go:build linux

package importer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// Syscalls, replaceable in tests.
var (
	sysSocket   = syscall.Socket
	sysBind     = syscall.Bind
	sysSendto   = syscall.Sendto
	sysRecvfrom = syscall.Recvfrom
)

// addAlias adds ip/bits to the interface with RTM_NEWADDR (IMP-5).
func addAlias(ifIndex int, ip net.IP, bits int) error {
	ip4 := ip.To4()
	if ip4 == nil {
		return errors.New("only IPv4 aliases")
	}
	fd, err := sysSocket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := sysBind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}

	attr := func(typ uint16, data []byte) []byte {
		l := syscall.SizeofRtAttr + len(data)
		b := make([]byte, (l+3)&^3)
		binary.NativeEndian.PutUint16(b[0:], uint16(l))
		binary.NativeEndian.PutUint16(b[2:], typ)
		copy(b[4:], data)
		return b
	}
	ifa := make([]byte, syscall.SizeofIfAddrmsg)
	ifa[0] = syscall.AF_INET
	ifa[1] = byte(bits)
	binary.NativeEndian.PutUint32(ifa[4:], uint32(ifIndex))
	body := append(ifa, attr(syscall.IFA_LOCAL, ip4)...)
	body = append(body, attr(syscall.IFA_ADDRESS, ip4)...)

	msg := make([]byte, syscall.NLMSG_HDRLEN, syscall.NLMSG_HDRLEN+len(body))
	msg = append(msg, body...)
	binary.NativeEndian.PutUint32(msg[0:], uint32(len(msg)))
	binary.NativeEndian.PutUint16(msg[4:], syscall.RTM_NEWADDR)
	binary.NativeEndian.PutUint16(msg[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_EXCL)
	binary.NativeEndian.PutUint32(msg[8:], 1)

	if err := sysSendto(fd, msg, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, _, err := sysRecvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.Header.Type == syscall.NLMSG_ERROR {
			code := *(*int32)(unsafe.Pointer(&m.Data[0]))
			if code == 0 || -code == int32(syscall.EEXIST) {
				return nil
			}
			return fmt.Errorf("RTM_NEWADDR %s: %w", ip, syscall.Errno(-code))
		}
	}
	return nil
}
