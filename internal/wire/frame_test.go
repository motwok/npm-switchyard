package wire

import (
	"bytes"
	"strings"
	"testing"
)

func TestStatusReason(t *testing.T) {
	if StatusReason(StatusOK) != "ok" || StatusReason(StatusNoTarget) != "no_target" || StatusReason(StatusUnreachable) != "unreachable" {
		t.Fatal("reasons")
	}
}

func TestFrameErrors(t *testing.T) {
	if _, err := UnmarshalTarget([]byte{'x', 0, 1}); err == nil {
		t.Fatal("bad protocol accepted")
	}
	if _, err := UnmarshalTarget(nil); err == nil {
		t.Fatal("short target accepted")
	}
	if _, _, err := UnmarshalRequest([]byte{1}); err == nil {
		t.Fatal("short request accepted")
	}
	if WriteMsg(&bytes.Buffer{}, MsgPing, make([]byte, 70000)) == nil || WriteDatagram(&bytes.Buffer{}, make([]byte, 70000)) == nil {
		t.Fatal("oversize accepted")
	}
	var b bytes.Buffer
	_ = WriteDatagram(&b, []byte("toolong"))
	if _, err := ReadDatagram(&b, make([]byte, 2)); err == nil {
		t.Fatal("datagram larger than buffer accepted")
	}
	if _, _, err := ReadMsg(strings.NewReader("")); err == nil {
		t.Fatal("empty read")
	}
	if _, err := ReadDatagram(strings.NewReader(""), nil); err == nil {
		t.Fatal("empty datagram read")
	}
	if _, err := readHello(strings.NewReader(strings.Repeat("x", 25))); err == nil {
		t.Fatal("wrong magic accepted")
	}
	if _, err := leafKey(nil); err == nil {
		t.Fatal("no certificate accepted")
	}
	if _, err := leafKey([][]byte{{1, 2}}); err == nil {
		t.Fatal("garbage certificate accepted")
	}
}
