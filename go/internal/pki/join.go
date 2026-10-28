// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// JoinTokenPrefix starts every join token of a node and the part a node
// sends.
const JoinTokenPrefix = "feos-join-"

// JoinSecretLen is the length of what a node sends: the prefix and 43
// characters of base64url, 32 random bytes.
const JoinSecretLen = len(JoinTokenPrefix) + 43

// IngressJoinTokenPrefix starts every join token of an instance of
// ferun-ingress (kind ingress) and the part the instance sends. It is not
// JoinTokenPrefix, and IngressJoinSecretLen is not JoinSecretLen, so that
// a node's Join, also of a ferund that knows no kinds, refuses an
// ingress's token by its form before any lookup.
const IngressJoinTokenPrefix = "feos-ingress-join-"

// IngressJoinSecretLen is the length of what an instance of ferun-ingress
// sends: the prefix and 43 characters of base64url, 32 random bytes.
const IngressJoinSecretLen = len(IngressJoinTokenPrefix) + 43

// NewJoinToken makes a node's join token pinned to the root with SPKI
// hash pin. token is what the operator gets, "feos-join-<secret>.<pin>";
// sent is what the node sends, "feos-join-<secret>"; hash is what the
// database keeps, HashJoinSecret(sent).
func NewJoinToken(pin string) (token, sent, hash string, err error) {
	return newJoinToken(JoinTokenPrefix, pin)
}

// NewIngressJoinToken makes the join token of an instance of
// ferun-ingress, as NewJoinToken a node's: token is
// "feos-ingress-join-<secret>.<pin>", sent "feos-ingress-join-<secret>".
func NewIngressJoinToken(pin string) (token, sent, hash string, err error) {
	return newJoinToken(IngressJoinTokenPrefix, pin)
}

func newJoinToken(prefix, pin string) (token, sent, hash string, err error) {
	if len(pin) != 64 {
		return "", "", "", fmt.Errorf("pin %q is not a hex SHA-256", pin)
	}
	if _, err := hex.DecodeString(pin); err != nil {
		return "", "", "", fmt.Errorf("pin %q is not a hex SHA-256", pin)
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", "", err
	}
	sent = prefix + base64.RawURLEncoding.EncodeToString(b[:])
	return sent + "." + strings.ToLower(pin), sent, HashJoinSecret(sent), nil
}

// HashJoinSecret is the lowercase hex SHA-256 of the part of a join token
// a node sends.
func HashJoinSecret(sent string) string {
	sum := sha256.Sum256([]byte(sent))
	return hex.EncodeToString(sum[:])
}

// SplitJoinToken splits a node's token "feos-join-<secret>.<pin>" into
// the part the node sends and the pin it checks the control plane with.
// An ingress's token is refused.
func SplitJoinToken(token string) (sent, pin string, err error) {
	return splitJoinToken(token, JoinTokenPrefix)
}

// SplitIngressJoinToken splits the token of an instance of ferun-ingress,
// "feos-ingress-join-<secret>.<pin>", as SplitJoinToken a node's. A node's
// token is refused.
func SplitIngressJoinToken(token string) (sent, pin string, err error) {
	return splitJoinToken(token, IngressJoinTokenPrefix)
}

// IsIngressJoinSecret reports whether sent has the form of what an
// instance of ferun-ingress sends: "feos-ingress-join-<secret>".
func IsIngressJoinSecret(sent string) bool {
	return isJoinSecret(sent, IngressJoinTokenPrefix)
}

// isJoinSecret reports whether sent is prefix and 43 characters of
// base64url.
func isJoinSecret(sent, prefix string) bool {
	if !strings.HasPrefix(sent, prefix) || len(sent) != len(prefix)+43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(sent[len(prefix):])
	return err == nil
}

func splitJoinToken(token, prefix string) (sent, pin string, err error) {
	sent, pin, ok := strings.Cut(token, ".")
	if !ok || !strings.HasPrefix(sent, prefix) || len(sent) != len(prefix)+43 {
		return "", "", fmt.Errorf("join token is not %s<secret>.<pin>", prefix)
	}
	if !isJoinSecret(sent, prefix) {
		return "", "", errors.New("join token's secret is not base64url")
	}
	if len(pin) != 64 {
		return "", "", errors.New("join token's pin is not a hex SHA-256")
	}
	if _, err := hex.DecodeString(pin); err != nil {
		return "", "", errors.New("join token's pin is not a hex SHA-256")
	}
	return sent, strings.ToLower(pin), nil
}

// VerifyJoinServer checks the certificates a control-plane instance
// presented on the join connection, before the node sends its token: the
// presented certificate whose SPKI hash equals pin, a CA, is the only
// root; the chain must verify to it for the DNS name serverName, and the
// leaf's one URI name must be exactly spiffe://<td>/ferun. A server whose
// chain holds no such root is refused.
func VerifyJoinServer(pin, serverName, td string, rawCerts [][]byte) error {
	certs, err := parseChain(rawCerts)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	found := false
	for _, c := range certs[1:] {
		if equalHex(spkiHash(c.RawSubjectPublicKeyInfo), pin) {
			if !c.IsCA || !c.BasicConstraintsValid {
				return errors.New("the certificate matching the join token's pin is not a CA")
			}
			roots.AddCert(c)
			found = true
		}
	}
	if !found {
		return errors.New("the control plane's chain holds no root matching the join token's pin")
	}
	return verifyServer(roots, certs, serverName, td)
}

// CheckBundlePin checks a JoinResponse's trust bundle (DER roots) before a
// node accepts it: the bundle must hold the root its join token pins.
func CheckBundlePin(bundleDER [][]byte, pin string) error {
	for _, raw := range bundleDER {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("trust bundle: %w", err)
		}
		if equalHex(spkiHash(c.RawSubjectPublicKeyInfo), pin) {
			return nil
		}
	}
	return errors.New("the trust bundle does not hold the root the join token pins")
}

// VerifyControlPlane checks the certificates a control-plane instance
// presented against the trust bundle: the chain must verify for the DNS
// name serverName, and the leaf's one URI name must be exactly
// spiffe://<td>/ferun, which only the control plane's certificates carry.
func VerifyControlPlane(bundle *x509.CertPool, serverName, td string, rawCerts [][]byte) error {
	if bundle == nil {
		return errors.New("no trust bundle")
	}
	certs, err := parseChain(rawCerts)
	if err != nil {
		return err
	}
	return verifyServer(bundle, certs, serverName, td)
}

func parseChain(rawCerts [][]byte) ([]*x509.Certificate, error) {
	if len(rawCerts) == 0 {
		return nil, errors.New("the server presented no certificate")
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, fmt.Errorf("the server's certificate: %w", err)
		}
		certs = append(certs, c)
	}
	return certs, nil
}

func verifyServer(roots *x509.CertPool, certs []*x509.Certificate, serverName, td string) error {
	leaf := certs[0]
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:       serverName,
		Roots:         roots,
		Intermediates: inter,
		CurrentTime:   time.Now(),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("the control plane's certificate: %w", err)
	}
	if err := noIPs(leaf); err != nil {
		return err
	}
	want := ControlPlaneID(td)
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != want {
		got := make([]string, len(leaf.URIs))
		for i, u := range leaf.URIs {
			got[i] = u.String()
		}
		return fmt.Errorf("the server's certificate names %v, not the control plane %s", got, want)
	}
	return nil
}
