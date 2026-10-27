// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
)

// MaxCSRBytes is the largest signing request accepted.
const MaxCSRBytes = 16 << 10

// CheckCSR parses and checks a PKCS#10 signing request in DER: at most 16
// KiB, a valid self-signature, and a key of an allowed type (Ed25519, and
// ECDSA P-256 when allowECDSA). Everything else in it, its subject and its
// names included, is ignored by issuance (fleet.proto). keyHash is the
// lowercase hex SHA-256 of the key's DER SubjectPublicKeyInfo.
func CheckCSR(csrDER []byte, allowECDSA bool) (csr *x509.CertificateRequest, keyHash string, err error) {
	if len(csrDER) == 0 {
		return nil, "", errors.New("no certificate signing request")
	}
	if len(csrDER) > MaxCSRBytes {
		return nil, "", fmt.Errorf("certificate signing request of %d bytes exceeds %d", len(csrDER), MaxCSRBytes)
	}
	csr, err = x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, "", fmt.Errorf("certificate signing request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", fmt.Errorf("certificate signing request: %w", err)
	}
	switch k := csr.PublicKey.(type) {
	case ed25519.PublicKey:
	case *ecdsa.PublicKey:
		if !allowECDSA {
			return nil, "", errors.New("certificate signing request: the key must be Ed25519")
		}
		if k.Curve != elliptic.P256() {
			return nil, "", errors.New("certificate signing request: an ECDSA key must be on P-256")
		}
	default:
		if allowECDSA {
			return nil, "", fmt.Errorf("certificate signing request: key type %T; Ed25519 or ECDSA P-256 only", csr.PublicKey)
		}
		return nil, "", fmt.Errorf("certificate signing request: key type %T; Ed25519 only", csr.PublicKey)
	}
	return csr, spkiHash(csr.RawSubjectPublicKeyInfo), nil
}

// CreateCSR makes a PKCS#10 signing request in DER for key, with subject
// CN commonName: what a node (or a pod's feos-authn) sends. The control
// plane ignores the subject; it is there for the holder's logs.
func CreateCSR(key crypto.Signer, commonName string) ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}, key)
}
