// Package wire implements the authenticated agent-to-agent connection (spec 5, 11).
package wire

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"
)

const (
	magic        = "NSY1"
	ekmLabel     = "EXPORTER-npm-switchyard"
	VersionMin   = 1
	VersionMax   = 1
	handshakeTTL = 15 * time.Second
)

// Kind of an authenticated connection.
const (
	KindControl byte = 'C'
	KindStream  byte = 'S'
)

var ErrNoVersion = errors.New("no common protocol version")

type InstanceID [16]byte

func NewInstanceID() InstanceID {
	var id InstanceID
	_, _ = rand.Read(id[:])
	return id
}

// Less orders instance IDs for the control channel tie-break (PAR-4).
func (a InstanceID) Less(b InstanceID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

type hello struct {
	verMin, verMax uint16
	id             InstanceID
	kind           byte
}

func (h hello) marshal() []byte {
	b := make([]byte, 0, 4+2+2+16+1)
	b = append(b, magic...)
	b = binary.BigEndian.AppendUint16(b, h.verMin)
	b = binary.BigEndian.AppendUint16(b, h.verMax)
	b = append(b, h.id[:]...)
	return append(b, h.kind)
}

func readHello(r io.Reader) (hello, error) {
	b := make([]byte, 25)
	if _, err := io.ReadFull(r, b); err != nil {
		return hello{}, err
	}
	if string(b[:4]) != magic {
		return hello{}, errors.New("not a switchyard peer")
	}
	var h hello
	h.verMin = binary.BigEndian.Uint16(b[4:])
	h.verMax = binary.BigEndian.Uint16(b[6:])
	copy(h.id[:], b[8:24])
	h.kind = b[24]
	return h, nil
}

// Negotiate returns the highest version both ranges share.
func Negotiate(aMin, aMax, bMin, bMax uint16) (uint16, error) {
	v := min(aMax, bMax)
	if v < max(aMin, bMin) {
		return 0, ErrNoVersion
	}
	return v, nil
}

// Proof is HMAC-SHA256(psk, role || ekm); the role stops reflection of the peer's proof.
func Proof(psk []byte, role string, ekm []byte) []byte {
	m := hmac.New(sha256.New, psk)
	m.Write([]byte(role))
	m.Write([]byte{0})
	m.Write(ekm)
	return m.Sum(nil)
}

// Conn is an authenticated connection.
type Conn struct {
	*tls.Conn
	Partner  int // index into the key list
	PeerID   InstanceID
	Version  uint16
	Kind     byte
	Dialer   bool
	PeerAddr net.Addr
}

// Local holds this agent's identity for handshakes.
type Local struct {
	ID   InstanceID
	cert tls.Certificate
}

func NewLocal() (*Local, error) {
	cert, err := selfSigned()
	if err != nil {
		return nil, err
	}
	return &Local{ID: NewInstanceID(), cert: cert}, nil
}

func (l *Local) serverConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{l.cert}, MinVersion: tls.VersionTLS13}
}

func (l *Local) clientConfig() *tls.Config {
	// Server identity is proven by the PSK, not the certificate.
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13} //nolint:gosec
}

func ekm(c *tls.Conn) ([]byte, error) {
	st := c.ConnectionState()
	return st.ExportKeyingMaterial(ekmLabel, nil, 32)
}

// Dial authenticates as dialer with one known key.
func (l *Local) Dial(raw net.Conn, key []byte, kind byte) (*Conn, error) {
	_ = raw.SetDeadline(time.Now().Add(handshakeTTL))
	defer raw.SetDeadline(time.Time{})
	c := tls.Client(raw, l.clientConfig())
	if err := c.Handshake(); err != nil {
		return nil, err
	}
	m, err := ekm(c)
	if err != nil {
		return nil, err
	}
	out := hello{VersionMin, VersionMax, l.ID, kind}.marshal()
	out = append(out, Proof(key, "dialer", m)...)
	if _, err := c.Write(out); err != nil {
		return nil, err
	}
	h, err := readHello(c)
	if err != nil {
		return nil, err
	}
	p := make([]byte, sha256.Size)
	if _, err := io.ReadFull(c, p); err != nil {
		return nil, err
	}
	if !hmac.Equal(p, Proof(key, "listener", m)) {
		return nil, errors.New("partner proof invalid")
	}
	v, err := Negotiate(VersionMin, VersionMax, h.verMin, h.verMax)
	if err != nil {
		return nil, fmt.Errorf("%w: ours %d-%d, theirs %d-%d", err, VersionMin, VersionMax, h.verMin, h.verMax)
	}
	return &Conn{Conn: c, PeerID: h.id, Version: v, Kind: kind, Dialer: true, PeerAddr: raw.RemoteAddr()}, nil
}

// Accept authenticates as listener, trying every key (PAR-2).
func (l *Local) Accept(raw net.Conn, keys [][]byte) (*Conn, error) {
	_ = raw.SetDeadline(time.Now().Add(handshakeTTL))
	defer raw.SetDeadline(time.Time{})
	c := tls.Server(raw, l.serverConfig())
	if err := c.Handshake(); err != nil {
		return nil, err
	}
	m, err := ekm(c)
	if err != nil {
		return nil, err
	}
	h, err := readHello(c)
	if err != nil {
		return nil, err
	}
	p := make([]byte, sha256.Size)
	if _, err := io.ReadFull(c, p); err != nil {
		return nil, err
	}
	idx := -1
	for i, k := range keys {
		if hmac.Equal(p, Proof(k, "dialer", m)) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, errors.New("no configured psk matches")
	}
	out := hello{VersionMin, VersionMax, l.ID, h.kind}.marshal()
	out = append(out, Proof(keys[idx], "listener", m)...)
	if _, err := c.Write(out); err != nil {
		return nil, err
	}
	v, err := Negotiate(VersionMin, VersionMax, h.verMin, h.verMax)
	if err != nil {
		return nil, fmt.Errorf("%w: ours %d-%d, theirs %d-%d", err, VersionMin, VersionMax, h.verMin, h.verMax)
	}
	return &Conn{Conn: c, Partner: idx, PeerID: h.id, Version: v, Kind: h.kind, PeerAddr: raw.RemoteAddr()}, nil
}

func selfSigned() (tls.Certificate, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "npm-switchyard"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(100 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, nil
}
