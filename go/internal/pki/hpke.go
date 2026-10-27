// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto/ecdh"
	"crypto/hpke"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// bindingsInfo is the HPKE info of fleet.proto's SealedBindings.
const bindingsInfo = "feos bindings v1"

// HPKEKEM is the KEM of SealedBindings, DHKEM(X25519, HKDF-SHA256): what a
// run's HPKE key pair is made with (HPKEKEM().GenerateKey()).
func HPKEKEM() hpke.KEM { return hpke.DHKEM(ecdh.X25519()) }

// bindingsAAD is the additional data of a sealed box: the pod's ID as it
// is written (a short ID, or a UUID in lowercase hyphenated form), a zero
// byte and the version, so that the version is authenticated too.
func bindingsAAD(podID podid.ID, version string) []byte {
	return append(append([]byte(podID.String()), 0), version...)
}

// Seal seals a pod's bindings to the run's HPKE public key pub (X25519, 32
// bytes) in base mode: suite DHKEM(X25519, HKDF-SHA256), HKDF-SHA256,
// ChaCha20-Poly1305, info "feos bindings v1", aad bound to the pod and the
// version.
func Seal(pub []byte, podID podid.ID, b *fleetv1.PodBindings, version string) (*fleetv1.SealedBindings, error) {
	pk, err := HPKEKEM().NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("HPKE public key: %w", err)
	}
	enc, s, err := hpke.NewSender(pk, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), []byte(bindingsInfo))
	if err != nil {
		return nil, err
	}
	pt, err := proto.Marshal(b)
	if err != nil {
		return nil, err
	}
	ct, err := s.Seal(bindingsAAD(podID, version), pt)
	if err != nil {
		return nil, err
	}
	return &fleetv1.SealedBindings{Enc: enc, Ciphertext: ct, Version: version}, nil
}

// Open opens a sealed box with the run's HPKE private key: what a pod's
// feos-authn does, and the test node.
func Open(priv hpke.PrivateKey, podID podid.ID, s *fleetv1.SealedBindings) (*fleetv1.PodBindings, error) {
	if s == nil {
		return nil, errors.New("no sealed bindings")
	}
	r, err := hpke.NewRecipient(s.GetEnc(), priv, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), []byte(bindingsInfo))
	if err != nil {
		return nil, fmt.Errorf("open sealed bindings: %w", err)
	}
	pt, err := r.Open(bindingsAAD(podID, s.GetVersion()), s.GetCiphertext())
	if err != nil {
		return nil, fmt.Errorf("open sealed bindings: %w", err)
	}
	var b fleetv1.PodBindings
	if err := proto.Unmarshal(pt, &b); err != nil {
		return nil, fmt.Errorf("open sealed bindings: %w", err)
	}
	return &b, nil
}
