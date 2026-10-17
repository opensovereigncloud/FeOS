// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"

	"github.com/ironcore-dev/feos/go/internal/store"
)

var testULA = netip.MustParsePrefix("fd5e:1b2c:93a0::/48")

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// joinMesh runs JoinMesh for node in a transaction, as fleet does.
func joinMesh(t *testing.T, s *store.Store, node string, k []byte, endpoint string) (store.MeshJoin, error) {
	t.Helper()
	var j store.MeshJoin
	err := s.InTx(bg, func(tx *store.Store) error {
		n, err := tx.LockNode(bg, node, store.ForUpdate)
		if err != nil {
			return err
		}
		j, err = tx.JoinMesh(bg, n, testULA, k, endpoint)
		return err
	})
	return j, err
}

func version(t *testing.T, s *store.Store) uint64 {
	t.Helper()
	p, err := s.MeshPeers(bg)
	must(t, err)
	return p.Version
}

// Every node gets a subnet of its own, the same one on every call; a
// change of key or endpoint counts, a repeat does not; a removed node is
// nobody's peer and its subnet is not given out again, nor is the one of
// a node that joined anew.
func TestJoinMesh(t *testing.T) {
	s, _ := newStore(t)
	if _, ok, err := s.PodNetwork(bg); ok || err != nil {
		t.Fatalf("a new database is meshed: %v %v", ok, err)
	}
	if p, err := s.MeshPeers(bg); err != nil || p.Version != 0 || len(p.Nodes) != 0 {
		t.Fatalf("peers of a routed cluster: %+v %v", p, err)
	}
	must(t, s.InsertPodNetwork(bg, testULA))
	if ula, ok, err := s.PodNetwork(bg); !ok || err != nil || ula != testULA {
		t.Fatalf("pod network %v %v %v", ula, ok, err)
	}
	if err := s.InsertPodNetwork(bg, netip.MustParsePrefix("fd00:1:2::/48")); err == nil {
		t.Fatal("a second pod network")
	}
	for _, n := range []string{"n1", "n2", "n3"} {
		mkNode(t, s, n)
	}

	j1, err := joinMesh(t, s, "n1", key(1), "10.0.0.1:51820")
	must(t, err)
	j2, err := joinMesh(t, s, "n2", key(2), "10.0.0.2:51820")
	must(t, err)
	if j1.Subnet != 1 || j1.Prefix != "fd5e:1b2c:93a0:1::/64" || !j1.Changed ||
		j2.Subnet != 2 || j2.Prefix != "fd5e:1b2c:93a0:2::/64" {
		t.Fatalf("joins %+v %+v", j1, j2)
	}
	if v := version(t, s); v != 2 {
		t.Fatalf("version %d after two joins", v)
	}
	again, err := joinMesh(t, s, "n1", key(1), "10.0.0.1:51820")
	must(t, err)
	if again != (store.MeshJoin{Subnet: 1, Prefix: j1.Prefix}) || version(t, s) != 2 {
		t.Fatalf("a repeat %+v changed the version to %d", again, version(t, s))
	}
	moved, err := joinMesh(t, s, "n1", key(1), "10.0.0.11:51820")
	must(t, err)
	if !moved.Changed || moved.Subnet != 1 || version(t, s) != 3 {
		t.Fatalf("a new endpoint %+v, version %d", moved, version(t, s))
	}
	p, err := s.MeshPeers(bg)
	must(t, err)
	if len(p.Nodes) != 2 || p.Nodes["n1"].Endpoint != "10.0.0.11:51820" || !bytes.Equal(p.Nodes["n2"].PublicKey, key(2)) ||
		p.Nodes["n2"].PodPrefix != "fd5e:1b2c:93a0:2::/64" {
		t.Fatalf("peers %+v", p)
	}

	// Down nodes stay peers; removed ones do not, and their subnet stays
	// theirs.
	exec(t, s, "UPDATE nodes SET status = 'down' WHERE name = 'n2'")
	if p, _ := s.MeshPeers(bg); len(p.Nodes) != 2 {
		t.Fatalf("a down node is no peer: %+v", p)
	}
	must(t, s.InTx(bg, func(tx *store.Store) error {
		_, err := tx.RemoveNode(bg, "n2")
		return err
	}))
	if p, _ := s.MeshPeers(bg); len(p.Nodes) != 1 || p.Version != 4 {
		t.Fatalf("after the removal: %+v", p)
	}
	j3, err := joinMesh(t, s, "n3", key(3), "10.0.0.3:51820")
	must(t, err)
	if j3.Subnet != 3 {
		t.Fatalf("n3 got %+v: a removed node's subnet came round", j3)
	}

	// A node that joins anew is off the mesh until it asks again, and gets
	// a new subnet.
	must(t, s.InTx(bg, func(tx *store.Store) error { return tx.RejoinNode(bg, "n1") }))
	n1, err := s.GetNode(bg, "n1")
	must(t, err)
	if n1.MeshSubnet != 0 || n1.MeshPrefix != "" || n1.MeshPublicKey != nil || n1.MeshEndpoint != "" || n1.MeshPeer() {
		t.Fatalf("a rejoined node keeps its mesh: %+v", n1)
	}
	if p, _ := s.MeshPeers(bg); len(p.Nodes) != 1 || p.Version != 6 {
		t.Fatalf("after the rejoin: %+v", p)
	}
	j1, err = joinMesh(t, s, "n1", key(9), "10.0.0.1:51820")
	must(t, err)
	if j1.Subnet != 4 {
		t.Fatalf("the rejoined node got %+v", j1)
	}
	// A rejoin of a removed node does not count: it was nobody's peer.
	before := version(t, s)
	must(t, s.InTx(bg, func(tx *store.Store) error { return tx.RejoinNode(bg, "n2") }))
	if v := version(t, s); v != before {
		t.Fatalf("a removed node's rejoin changed the version from %d to %d", before, v)
	}
}

// The sequence ends at 65535 and never starts again.
func TestJoinMeshExhausted(t *testing.T) {
	s, _ := newStore(t)
	must(t, s.InsertPodNetwork(bg, testULA))
	mkNode(t, s, "n1")
	mkNode(t, s, "n2")
	exec(t, s, "SELECT setval('mesh_subnets', 65534)")
	j, err := joinMesh(t, s, "n1", key(1), "10.0.0.1:51820")
	must(t, err)
	if j.Subnet != 65535 || j.Prefix != "fd5e:1b2c:93a0:ffff::/64" {
		t.Fatalf("the last subnet: %+v", j)
	}
	if _, err := joinMesh(t, s, "n2", key(2), "10.0.0.2:51820"); !errors.Is(err, store.ErrMeshExhausted) {
		t.Fatalf("past the last subnet: %v", err)
	}
	// The one that has its subnet keeps it.
	if j, err := joinMesh(t, s, "n1", key(1), "10.0.0.1:51821"); err != nil || j.Subnet != 65535 {
		t.Fatalf("%+v %v", j, err)
	}
}
