package wire

import (
	"encoding/binary"
	"errors"
	"io"
)

// Control channel message types.
const (
	MsgPing    byte = 'P'
	MsgPong    byte = 'Q'
	MsgRequest byte = 'R' // listener asks the dialer for a stream (PAR-6)
)

// First byte the dialer writes on a stream connection.
const (
	StreamOpen   byte = 'O' // dialer has the client; listener exports the target
	StreamAttach byte = 'A' // dialer exports the target for a listener request
)

// Protocols.
const (
	TCP byte = 't'
	UDP byte = 'u'
)

// Stream status sent by the exporting side.
const (
	StatusOK          byte = 0
	StatusNoTarget    byte = 1
	StatusUnreachable byte = 2
)

func StatusReason(s byte) string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusNoTarget:
		return "no_target"
	default:
		return "unreachable"
	}
}

const maxFrame = 0xffff

// WriteMsg writes type, 2-byte length and payload.
func WriteMsg(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxFrame {
		return errors.New("frame too large")
	}
	b := make([]byte, 3+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint16(b[1:], uint16(len(payload)))
	copy(b[3:], payload)
	_, err := w.Write(b)
	return err
}

func ReadMsg(r io.Reader) (byte, []byte, error) {
	h := make([]byte, 3)
	if _, err := io.ReadFull(r, h); err != nil {
		return 0, nil, err
	}
	p := make([]byte, binary.BigEndian.Uint16(h[1:]))
	_, err := io.ReadFull(r, p)
	return h[0], p, err
}

// Target is what a stream is for.
type Target struct {
	Proto byte
	Port  uint16
	Name  string
}

func (t Target) Marshal() []byte {
	b := []byte{t.Proto}
	b = binary.BigEndian.AppendUint16(b, t.Port)
	return append(b, t.Name...)
}

func UnmarshalTarget(b []byte) (Target, error) {
	if len(b) < 3 || (b[0] != TCP && b[0] != UDP) {
		return Target{}, errors.New("bad target")
	}
	return Target{Proto: b[0], Port: binary.BigEndian.Uint16(b[1:]), Name: string(b[3:])}, nil
}

// Request is a MsgRequest payload: stream id plus target.
func MarshalRequest(id uint32, t Target) []byte {
	return append(binary.BigEndian.AppendUint32(nil, id), t.Marshal()...)
}

func UnmarshalRequest(b []byte) (uint32, Target, error) {
	if len(b) < 4 {
		return 0, Target{}, errors.New("bad request")
	}
	t, err := UnmarshalTarget(b[4:])
	return binary.BigEndian.Uint32(b), t, err
}

// Datagram framing for UDP flows (TRN-3).
func WriteDatagram(w io.Writer, p []byte) error {
	if len(p) > maxFrame {
		return errors.New("datagram too large")
	}
	b := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(b, uint16(len(p)))
	copy(b[2:], p)
	_, err := w.Write(b)
	return err
}

func ReadDatagram(r io.Reader, buf []byte) (int, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(h[:]))
	if n > len(buf) {
		return 0, errors.New("datagram too large")
	}
	_, err := io.ReadFull(r, buf[:n])
	return n, err
}
