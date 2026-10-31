package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

// Keys are base64 of 32 bytes, as WireGuard's: an Ed25519 seed or public key.

func GenerateKey() string {
	seed := make([]byte, ed25519.SeedSize)
	_, _ = rand.Read(seed)
	return base64.StdEncoding.EncodeToString(seed)
}

func decode32(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 32 {
		return nil, errors.New("want base64 of 32 bytes")
	}
	return b, nil
}

func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := decode32(s)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(b), nil
}

func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := decode32(s)
	if err != nil {
		return nil, err
	}
	return ed25519.PublicKey(b), nil
}

func PublicKeyString(k ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}
