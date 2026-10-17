// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ironcore-dev/feos/go/internal/store"
)

// The rollout grace a node has left is measured on the database clock: the
// time to its stopping_until, 0 without one or once it passed.
func TestNodeStoppingFor(t *testing.T) {
	s, _ := newStore(t)
	for _, n := range []string{"n1", "n2", "n3"} {
		mkNode(t, s, n)
	}
	if _, err := s.SetNodeStopping(bg, "n1", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	exec(t, s, "UPDATE nodes SET stopping_until = now() - interval '1 second' WHERE name = 'n2'")
	left, err := s.NodeStoppingFor(bg, "n1")
	if err != nil || left <= 29*time.Second || left > 30*time.Second {
		t.Fatalf("n1: %v %v", left, err)
	}
	for _, n := range []string{"n2", "n3"} {
		if left, err := s.NodeStoppingFor(bg, n); err != nil || left != 0 {
			t.Fatalf("%s: %v %v", n, left, err)
		}
	}
	if _, err := s.NodeStoppingFor(bg, "ghost"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("an unknown node: %v", err)
	}
	all, err := s.NodesStoppingFor(bg)
	if err != nil || len(all) != 3 || all["n1"] <= 29*time.Second || all["n2"] != 0 || all["n3"] != 0 {
		t.Fatalf("all: %v %v", all, err)
	}
}
