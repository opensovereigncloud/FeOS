// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package table

import (
	"maps"
	"slices"
	"testing"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

func snapshotOf(envs ...wire.Env) *Snapshot {
	s := newSnapshot()
	for _, e := range envs {
		s.Envs[e.ID] = e
	}
	return s
}

func ids(envs []wire.Env) []string {
	var out []string
	for _, e := range envs {
		out = append(out, e.ID)
	}
	return out
}

// Diff puts the new and changed environments and removes those gone, each
// sorted by ID; after a stream sent what Diff gave, the next Diff of the
// same snapshot is empty.
func TestDiff(t *testing.T) {
	cur := snapshotOf(wire.Env{ID: "c", Hash: "c1"}, wire.Env{ID: "a", Hash: "a2"}, wire.Env{ID: "b", Hash: "b1"},
		wire.Env{ID: "e", Hash: "e1"})
	sent := map[string]string{"a": "a1", "b": "b1", "d": "d1", "f": "f1"}
	puts, removed := Diff(sent, cur)
	if got := ids(puts); !slices.Equal(got, []string{"a", "c", "e"}) || puts[0].Hash != "a2" {
		t.Fatalf("puts %+v", puts)
	}
	if !slices.Equal(removed, []string{"d", "f"}) {
		t.Fatalf("removed %v", removed)
	}

	// Everything to a stream that sent nothing.
	puts, removed = Diff(nil, cur)
	if got := ids(puts); !slices.Equal(got, []string{"a", "b", "c", "e"}) || removed != nil {
		t.Fatalf("from nothing: %v %v", got, removed)
	}
	// Nothing once it sent the snapshot.
	sent = Hashes(cur.Envs)
	if !maps.Equal(sent, map[string]string{"a": "a2", "b": "b1", "c": "c1", "e": "e1"}) {
		t.Fatalf("hashes %v", sent)
	}
	if puts, removed := Diff(sent, cur); puts != nil || removed != nil {
		t.Fatalf("after sending: %v %v", puts, removed)
	}
	// Everything removed for an empty table; nothing for no table.
	if puts, removed := Diff(sent, newSnapshot()); puts != nil || !slices.Equal(removed, []string{"a", "b", "c", "e"}) {
		t.Fatalf("empty table: %v %v", puts, removed)
	}
	if puts, removed := Diff(sent, nil); puts != nil || removed != nil {
		t.Fatalf("no table: %v %v", puts, removed)
	}
}
