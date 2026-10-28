// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// KeyLen is the length of a WireGuard key.
const KeyLen = 32

// Key is a WireGuard key, private or public: 32 bytes of Curve25519. Its
// text form is standard base64, as wg prints it.
type Key [KeyLen]byte

// GenerateKey returns a new private key: 32 random bytes, clamped as
// Curve25519 wants them (wg genkey does the same).
func GenerateKey() (Key, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, fmt.Errorf("generate a WireGuard key: %w", err)
	}
	var k Key
	copy(k[:], priv.Bytes())
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	return k, nil
}

// PublicKey is the public key of the private key k: X25519(k, 9).
func (k Key) PublicKey() Key {
	priv, err := ecdh.X25519().NewPrivateKey(k[:])
	if err != nil {
		// X25519 takes every string of 32 bytes; only a wrong length fails.
		return Key{}
	}
	var pub Key
	copy(pub[:], priv.PublicKey().Bytes())
	return pub
}

// IsZero reports whether k is all zeroes, which the kernel reports for a
// device that has no key.
func (k Key) IsZero() bool { return k == Key{} }

// String is k in base64.
func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

// ParseKey parses a key in base64, as String writes it.
func ParseKey(s string) (Key, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return Key{}, fmt.Errorf("WireGuard key is not base64: %w", err)
	}
	if len(b) != KeyLen {
		return Key{}, fmt.Errorf("WireGuard key has %d bytes, want %d", len(b), KeyLen)
	}
	var k Key
	copy(k[:], b)
	return k, nil
}
