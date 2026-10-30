// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto/ed25519"
	"crypto/x509"
	"strings"
	"testing"
	"time"
)

// A sink's certificate is a server's with the one URI
// spiffe://<td>/sink/<name> and no other name; it never passes as a
// node's or an ingress's, nor theirs as a sink's.
func TestIssueSink(t *testing.T) {
	iss, _ := newCA(t)
	if id := SinkID(testTD, "sink-0"); id != "spiffe://"+testTD+"/sink/sink-0" {
		t.Errorf("SinkID = %s", id)
	}
	csr, key := ed25519CSR(t, &x509.CertificateRequest{DNSNames: []string{"ferun." + testDomain}})
	before := time.Now()
	pc, leaf, err := iss.IssueSink(csr, "sink-0", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !leaf.PublicKey.(ed25519.PublicKey).Equal(key.Public()) {
		t.Error("the certificate is not for the CSR's key")
	}
	if len(leaf.DNSNames) != 0 || len(leaf.IPAddresses) != 0 || len(leaf.URIs) != 1 ||
		leaf.URIs[0].String() != SinkID(testTD, "sink-0") {
		t.Errorf("names %v %v %v", leaf.DNSNames, leaf.IPAddresses, leaf.URIs)
	}
	checkBackdated(t, leaf, before, 24*time.Hour)
	if err := verifyLeaf(iss, leaf, x509.ExtKeyUsageServerAuth); err != nil {
		t.Errorf("verify: %v", err)
	}
	if err := verifyLeaf(iss, leaf, x509.ExtKeyUsageClientAuth); err == nil {
		t.Error("a sink certificate verified as a client's")
	}
	if name, err := ParseSinkID(testTD, leaf); err != nil || name != "sink-0" {
		t.Errorf("ParseSinkID = %q %v", name, err)
	}
	if pc.SpiffeId != SinkID(testTD, "sink-0") || len(pc.Chain) != 2 || len(pc.TrustBundle) != 1 {
		t.Errorf("proto %v", pc)
	}
	if _, err := ParseNodeID(testTD, leaf); err == nil {
		t.Error("ParseNodeID accepted a sink certificate")
	}
	if _, err := ParseIngressID(testTD, leaf); err == nil {
		t.Error("ParseIngressID accepted a sink certificate")
	}
	_, node, err := iss.IssueNode(csr, "sink-0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSinkID(testTD, node); err == nil {
		t.Error("ParseSinkID accepted a node certificate")
	}
	for _, name := range []string{"", "Sink", "a.b", "a/b", strings.Repeat("a", 64)} {
		if _, _, err := iss.IssueSink(csr, name, time.Hour); err == nil {
			t.Errorf("sink name %q accepted", name)
		}
	}
}

func TestNewServiceToken(t *testing.T) {
	a, ha, err := NewServiceToken("sink")
	if err != nil {
		t.Fatal(err)
	}
	b, hb, _ := NewServiceToken("sink")
	if !strings.HasPrefix(a, "feos-sink-") || len(a) != len("feos-sink-")+43 || a == b || ha == hb ||
		ha != HashJoinSecret(a) {
		t.Errorf("tokens %q %q, hashes %q %q", a, b, ha, hb)
	}
}
