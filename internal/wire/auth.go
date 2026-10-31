// Package wire implements the authenticated agent-to-agent connection (spec 5, 11).
package wire

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"time"
)

const (
	magic        = "NSY1"
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

// Fingerprint names a public key in SNI (PAR-2): "k" + base32 of 15 bytes of its SHA-256.
func Fingerprint(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return "k" + strings.ToLower(base32.StdEncoding.EncodeToString(h[:15]))
}

// Partner is one pair's keys: the own key towards it and its pinned public key.
type Partner struct {
	Own  ed25519.PrivateKey
	Peer ed25519.PublicKey
}

// Conn is an authenticated connection.
type Conn struct {
	*tls.Conn
	Partner  int
	PeerID   InstanceID
	Version  uint16
	Kind     byte
	Dialer   bool
	PeerAddr net.Addr
}

// Local holds this agent's identities for handshakes.
type Local struct {
	ID       InstanceID
	partners []Partner
	certs    map[string]tls.Certificate // fingerprint of own public key -> certificate
	global   tls.Certificate
	byPeer   map[string]int // peer public key -> partner index

	verMin, verMax uint16 // supported protocol versions
}

func NewLocal(global ed25519.PrivateKey, partners []Partner) (*Local, error) {
	l := &Local{ID: NewInstanceID(), partners: partners, certs: map[string]tls.Certificate{}, byPeer: map[string]int{},
		verMin: VersionMin, verMax: VersionMax}
	var err error
	if l.global, err = certFor(global); err != nil {
		return nil, err
	}
	l.certs[Fingerprint(global.Public().(ed25519.PublicKey))] = l.global
	for i, p := range partners {
		fp := Fingerprint(p.Own.Public().(ed25519.PublicKey))
		if _, ok := l.certs[fp]; !ok {
			c, err := certFor(p.Own)
			if err != nil {
				return nil, err
			}
			l.certs[fp] = c
		}
		l.byPeer[string(p.Peer)] = i
	}
	return l, nil
}

func leafKey(raw [][]byte) (ed25519.PublicKey, error) {
	if len(raw) == 0 {
		return nil, errors.New("no certificate")
	}
	c, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return nil, err
	}
	k, ok := c.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("certificate key is not Ed25519")
	}
	return k, nil
}

// Dial authenticates as dialer towards partner idx.
func (l *Local) Dial(raw net.Conn, idx int, kind byte) (*Conn, error) {
	_ = raw.SetDeadline(time.Now().Add(handshakeTTL))
	defer raw.SetDeadline(time.Time{})
	p := l.partners[idx]
	cert := l.certs[Fingerprint(p.Own.Public().(ed25519.PublicKey))]
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ServerName:   Fingerprint(p.Peer),
		// Trust comes from the pinned key, not from a CA.
		InsecureSkipVerify: true, //nolint:gosec
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			k, err := leafKey(raw)
			if err != nil {
				return err
			}
			if !bytes.Equal(k, p.Peer) {
				return errors.New("partner key does not match public_key")
			}
			return nil
		},
	}
	c := tls.Client(raw, cfg)
	if err := c.Handshake(); err != nil {
		return nil, err
	}
	if _, err := c.Write(hello{l.verMin, l.verMax, l.ID, kind}.marshal()); err != nil {
		return nil, err
	}
	h, err := readHello(c)
	if err != nil {
		return nil, err
	}
	v, err := Negotiate(l.verMin, l.verMax, h.verMin, h.verMax)
	if err != nil {
		return nil, fmt.Errorf("%w: ours %d-%d, theirs %d-%d", err, l.verMin, l.verMax, h.verMin, h.verMax)
	}
	return &Conn{Conn: c, Partner: idx, PeerID: h.id, Version: v, Kind: kind, Dialer: true, PeerAddr: raw.RemoteAddr()}, nil
}

// Accept authenticates as listener and identifies the partner by its key (PAR-2).
func (l *Local) Accept(raw net.Conn) (*Conn, error) {
	_ = raw.SetDeadline(time.Now().Add(handshakeTTL))
	defer raw.SetDeadline(time.Time{})
	var used string
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAnyClientCert,
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			used = h.ServerName
			if c, ok := l.certs[h.ServerName]; ok {
				return &c, nil
			}
			used = ""
			return &l.global, nil
		},
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			k, err := leafKey(raw)
			if err != nil {
				return err
			}
			if _, ok := l.byPeer[string(k)]; !ok {
				return errors.New("unknown partner key")
			}
			return nil
		},
	}
	c := tls.Server(raw, cfg)
	if err := c.Handshake(); err != nil {
		return nil, err
	}
	k := c.ConnectionState().PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	idx := l.byPeer[string(k)]
	want := Fingerprint(l.partners[idx].Own.Public().(ed25519.PublicKey))
	if used == "" {
		used = Fingerprint(l.global.PrivateKey.(ed25519.PrivateKey).Public().(ed25519.PublicKey))
	}
	if used != want {
		return nil, errors.New("partner expected a different own key")
	}
	h, err := readHello(c)
	if err != nil {
		return nil, err
	}
	if _, err := c.Write(hello{l.verMin, l.verMax, l.ID, h.kind}.marshal()); err != nil {
		return nil, err
	}
	v, err := Negotiate(l.verMin, l.verMax, h.verMin, h.verMax)
	if err != nil {
		return nil, fmt.Errorf("%w: ours %d-%d, theirs %d-%d", err, l.verMin, l.verMax, h.verMin, h.verMax)
	}
	return &Conn{Conn: c, Partner: idx, PeerID: h.id, Version: v, Kind: h.kind, PeerAddr: raw.RemoteAddr()}, nil
}

func certFor(k ed25519.PrivateKey) (tls.Certificate, error) {
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "npm-switchyard"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(100 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, nil
}
