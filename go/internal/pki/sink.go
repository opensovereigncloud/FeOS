// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"net/url"
	"time"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
)

// IssueSink issues the server certificate of the ferun-sink instance name
// (ADR 0026) for the key of csrDER: ServerAuth, the URI
// spiffe://<td>/sink/<name> and no DNS name, subject CN name, valid for
// ttl. Nodes verify it by its URI, not by a host name. The CSR's own
// subject and names are ignored. The key is Ed25519 or ECDSA P-256. The
// sink asks again at half its lifetime.
func (i *Issuer) IssueSink(csrDER []byte, name string, ttl time.Duration) (*fleetv1.Certificate, *x509.Certificate, error) {
	if !isDNSLabel(name) {
		return nil, nil, fmt.Errorf("sink name %q is not a DNS label", name)
	}
	csr, _, err := CheckCSR(csrDER, true)
	if err != nil {
		return nil, nil, err
	}
	id := SinkID(i.td, name)
	u, err := url.Parse(id)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := i.sign(&x509.Certificate{
		Subject:     pkix.Name{CommonName: name},
		URIs:        []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, csr.PublicKey, ttl)
	if err != nil {
		return nil, nil, err
	}
	return i.certificateProto(id, leaf, leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore)/2)), leaf, nil
}

// ServiceTokenPrefix starts every service token of kind:
// "feos-<kind>-", "feos-sink-" for an instance of ferun-sink.
func ServiceTokenPrefix(kind string) string { return "feos-" + kind + "-" }

// NewServiceToken makes a service token of kind (ADR 0026): token is
// "feos-<kind>-<secret>", 32 random bytes in base64url, shown once; hash
// is what the database keeps, its lowercase hex SHA-256.
func NewServiceToken(kind string) (token, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	token = ServiceTokenPrefix(kind) + base64.RawURLEncoding.EncodeToString(b[:])
	return token, HashJoinSecret(token), nil
}
