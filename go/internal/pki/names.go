// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/ironcore-dev/feos/go/internal/podid"
)

// The SPIFFE layout (provisional; ADR 0014 leaves it open): a node is
// spiffe://<td>/node/<name>, the control plane's instances share
// spiffe://<td>/ferun, a sandbox's pod is
// spiffe://<td>/tenant/<tenant>/sandbox/<id>.
const (
	spiffeScheme    = "spiffe://"
	nodePath        = "/node/"
	controlPlanePth = "/ferun"
	maxCNBytes      = 64
)

// NodeID is the SPIFFE ID of node name.
func NodeID(td, name string) string {
	return spiffeScheme + td + nodePath + name
}

// SandboxID is the SPIFFE ID of a sandbox's pod.
func SandboxID(td, tenant string, id podid.ID) string {
	return spiffeScheme + td + "/tenant/" + tenant + "/sandbox/" + id.String()
}

// ControlPlaneID is the SPIFFE ID that every instance's certificate on the
// nodes' port carries, and no other certificate.
func ControlPlaneID(td string) string {
	return spiffeScheme + td + controlPlanePth
}

// Serial is a certificate's serial number as the database keeps it:
// lowercase hex without leading zeros.
func Serial(c *x509.Certificate) string {
	return c.SerialNumber.Text(16)
}

// ParseNodeID returns the node name of a node certificate: the certificate
// must carry exactly one URI name, spiffe://<td>/node/<name>, with name a
// DNS label.
func ParseNodeID(td string, cert *x509.Certificate) (string, error) {
	if cert == nil {
		return "", errors.New("no certificate")
	}
	if len(cert.URIs) != 1 {
		return "", fmt.Errorf("certificate has %d URI names; a node's has exactly one", len(cert.URIs))
	}
	u := cert.URIs[0]
	s := u.String()
	prefix := spiffeScheme + td + nodePath
	if u.Scheme != "spiffe" || u.Host != td || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.HasPrefix(s, prefix) {
		return "", fmt.Errorf("URI name %q is not a node of trust domain %s", s, td)
	}
	name := s[len(prefix):]
	if !isDNSLabel(name) || s != NodeID(td, name) {
		return "", fmt.Errorf("URI name %q does not name a node", s)
	}
	return name, nil
}

// ValidateTrustDomain checks a SPIFFE trust domain: a lowercase DNS name of
// labels [a-z0-9]([a-z0-9-]{0,61}[a-z0-9])? joined by dots, at most 255
// bytes, without a scheme or a port.
func ValidateTrustDomain(td string) error {
	if err := validateDomain(td); err != nil {
		return fmt.Errorf("trust domain: %w", err)
	}
	return nil
}

// validateDomain checks a lowercase DNS name as ValidateTrustDomain
// describes it; an IP address is refused although its labels would pass.
func validateDomain(s string) error {
	if s == "" {
		return errors.New("empty name")
	}
	if len(s) > 255 {
		return fmt.Errorf("%q is longer than 255 bytes", s)
	}
	if strings.Contains(s, "://") {
		return fmt.Errorf("%q has a scheme; give the name alone", s)
	}
	if strings.Contains(s, ":") {
		return fmt.Errorf("%q has a port; give the name alone", s)
	}
	if net.ParseIP(s) != nil {
		return fmt.Errorf("%q is an IP address, not a DNS name", s)
	}
	for _, l := range strings.Split(s, ".") {
		if !isDNSLabel(l) {
			return fmt.Errorf("%q is not a lowercase DNS name (label %q)", s, l)
		}
	}
	return nil
}

// isDNSLabel reports whether s is [a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?.
func isDNSLabel(s string) bool {
	if len(s) < 1 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// IsNodeName reports whether name is a valid node name: a DNS label, as
// fleet.proto defines it for JoinRequest.name.
func IsNodeName(name string) bool { return isDNSLabel(name) }

// within reports whether name equals domain or lies under it. A domain
// with a leading dot (a name constraint's form for "subdomains only")
// matches only names under it.
func within(name, domain string) bool {
	if strings.HasPrefix(domain, ".") {
		return strings.HasSuffix(name, domain)
	}
	return name == domain || strings.HasSuffix(name, "."+domain)
}

// validatePodID checks a pod's SPIFFE ID: spiffe://<td>/<path> in canonical
// form, path segments of letters, digits, ".", "-" and "_" (the SPIFFE ID
// rules), and outside the namespaces of the control plane (/ferun) and of
// nodes (/node/...), so that a pod's certificate can never pose as either.
func validatePodID(td, id string) error {
	if strings.ContainsAny(id, "%?#@") {
		return fmt.Errorf("SPIFFE ID %q is not canonical", id)
	}
	u, err := url.Parse(id)
	if err != nil {
		return fmt.Errorf("SPIFFE ID %q: %w", id, err)
	}
	if u.Scheme != "spiffe" || u.Host != td || u.Opaque != "" || u.User != nil {
		return fmt.Errorf("SPIFFE ID %q is not in trust domain %s", id, td)
	}
	if u.String() != id {
		return fmt.Errorf("SPIFFE ID %q is not canonical", id)
	}
	path := u.Path
	if path == "" || path == "/" || strings.HasSuffix(path, "/") {
		return fmt.Errorf("SPIFFE ID %q has no path", id)
	}
	for _, seg := range strings.Split(path[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("SPIFFE ID %q has an invalid path segment %q", id, seg)
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
				return fmt.Errorf("SPIFFE ID %q has an invalid character in %q", id, seg)
			}
		}
	}
	if path == controlPlanePth || strings.HasPrefix(path, controlPlanePth+"/") {
		return fmt.Errorf("SPIFFE ID %q is the control plane's; a pod never gets it", id)
	}
	if path == strings.TrimSuffix(nodePath, "/") || strings.HasPrefix(path, nodePath) {
		return fmt.Errorf("SPIFFE ID %q names a node; a pod never gets it", id)
	}
	return nil
}

// podCommonName derives a pod certificate's subject CN from its SPIFFE ID
// (ADR 0014): the trust domain and the path; when that is longer than a CN
// may be (64 bytes), its first 47 bytes, "-" and 16 hex digits of the
// ID's SHA-256, so that distinct IDs keep distinct CNs.
func podCommonName(id string) string {
	cn := strings.TrimPrefix(id, spiffeScheme)
	if len(cn) <= maxCNBytes {
		return cn
	}
	sum := sha256.Sum256([]byte(id))
	return cn[:maxCNBytes-17] + "-" + hex.EncodeToString(sum[:])[:16]
}
