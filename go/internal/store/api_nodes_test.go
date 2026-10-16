// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"
	"time"

	"github.com/ironcore-dev/feos/go/internal/store"
)

// The expiries of the nodes' current certificates, in one statement.
func TestNodeCertExpiries(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.InsertNode(bg, "n1"); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := s.InsertNodeCert(bg, store.NodeCert{Serial: "s1", Node: "n1", Kind: store.CertJoin, KeyHash: "k",
		NotBefore: time.Now(), NotAfter: after}); err != nil {
		t.Fatal(err)
	}
	m, err := s.NodeCertExpiries(bg, []string{"s1", "s9"})
	if err != nil || len(m) != 1 || !m["s1"].Equal(after) {
		t.Fatalf("expiries %v %v", m, err)
	}
	if m, err := s.NodeCertExpiries(bg, nil); err != nil || len(m) != 0 {
		t.Fatalf("none %v %v", m, err)
	}
}
