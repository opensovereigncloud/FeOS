// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSandboxesElsewhere(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	mkNode(t, s, "n2")
	onNode, elsewhere, target, nowhere := mkSandbox(t, s, "acme"), mkSandbox(t, s, "acme"), mkSandbox(t, s, "acme"), mkSandbox(t, s, "acme")
	place(t, s, onNode.ID, "n1", "running")
	place(t, s, elsewhere.ID, "n2", "running")
	place(t, s, target.ID, "n2", "creating")
	startOp(t, s, target.ID, "create", "i1", "n1", time.Minute)
	missing := uuid.New()

	got, err := s.SandboxesElsewhere(bg, "n1", []uuid.UUID{onNode.ID, elsewhere.ID, target.ID, nowhere.ID, missing})
	if err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]bool{elsewhere.ID: true, nowhere.ID: true}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("got %v; want %v", got, want)
		}
	}
	if got, err := s.SandboxesElsewhere(bg, "n1", nil); err != nil || len(got) != 0 {
		t.Fatalf("no ids: %v %v", got, err)
	}
}
