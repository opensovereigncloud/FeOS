// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package seal seals ferund's secrets at rest: the bindings' credentials
// and the environments' secrets, AES-256-GCM under the key of
// --secret-key-file. The caller's additional data binds a blob to its
// row, so that a blob copied into another row does not open.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// Version is the first byte of every blob, so that a later key rotation
// can tell generations apart.
const Version = 1

// KeySize is the size of the key: AES-256.
const KeySize = 32

// Sealer seals and opens blobs under one key. It is safe for concurrent
// use.
type Sealer struct {
	aead cipher.AEAD
}

// New returns the Sealer of a 32-byte key.
func New(key []byte) (*Sealer, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("the secret key has %d bytes, want %d (AES-256)", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext bound to aad: the version byte, a random nonce,
// then the AES-GCM ciphertext.
func (s *Sealer) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{Version}, nonce...)
	return s.aead.Seal(out, nonce, plaintext, aad), nil
}

// Open is Seal's inverse; a blob that is not one, or that was sealed
// under another key or other additional data, is an error.
func (s *Sealer) Open(sealed, aad []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(sealed) < 1+ns || sealed[0] != Version {
		return nil, errors.New("sealed credential is malformed")
	}
	pt, err := s.aead.Open(nil, sealed[1:1+ns], sealed[1+ns:], aad)
	if err != nil {
		return nil, errors.New("sealed credential does not open (was the key of --secret-key-file changed?)")
	}
	return pt, nil
}
