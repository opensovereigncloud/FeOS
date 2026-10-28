// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/store"
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

// The mesh's peers are the nodes and the ingresses that are peers, an
// ingress by mesh.IngressPeerName with its /64; removed and keyless ones
// are nobody's peer.
func TestMeshPeersIngresses(t *testing.T) {
	s, _ := newStore(t)
	must(t, s.InsertPodNetwork(bg, testULA))
	mkNode(t, s, "n1")
	for _, g := range []string{"g1", "g2", "g3"} {
		mkIngress(t, s, g)
	}
	_, err := joinMesh(t, s, "n1", key(1), "10.0.0.1:51820")
	must(t, err)
	g1, err := joinIngressMesh(t, s, "g1", key(2), "10.0.1.1:51820")
	must(t, err)
	_, err = joinIngressMesh(t, s, "g2", key(3), "10.0.1.2:51820")
	must(t, err)
	_, err = joinIngressMesh(t, s, "g3", key(4), "10.0.1.3:51820")
	must(t, err)
	p, err := s.MeshPeers(bg)
	must(t, err)
	want := mesh.Peer{Node: "ingress.g1", PublicKey: key(2), Endpoint: "10.0.1.1:51820", PodPrefix: g1.Prefix}
	if len(p.Nodes) != 4 || !p.Nodes[mesh.IngressPeerName("g1")].Equal(want) || p.Nodes["n1"].Endpoint != "10.0.0.1:51820" {
		t.Fatalf("peers %+v", p)
	}
	if p.Version != version(t, s) || p.Version != 4 {
		t.Fatalf("version %d", p.Version)
	}

	// g2 joins again (no key until its next Hello), g3 is removed.
	must(t, s.RejoinIngress(bg, "g2"))
	if found, err := s.RemoveIngress(bg, "g3"); err != nil || !found {
		t.Fatalf("remove %v %v", found, err)
	}
	p, err = s.MeshPeers(bg)
	must(t, err)
	if len(p.Nodes) != 2 || !p.Nodes[mesh.IngressPeerName("g1")].Equal(want) || p.Version != 6 {
		t.Fatalf("peers %+v", p)
	}
	for k := range p.Nodes {
		if k != "n1" && !mesh.IsIngressPeer(k) {
			t.Fatalf("peer %q", k)
		}
	}
	// An ingress named like a node never takes the node's place.
	mkIngress(t, s, "n1")
	_, err = joinIngressMesh(t, s, "n1", key(5), "10.0.1.5:51820")
	must(t, err)
	p, err = s.MeshPeers(bg)
	must(t, err)
	if len(p.Nodes) != 3 || string(p.Nodes["n1"].PublicKey) != string(key(1)) ||
		string(p.Nodes[mesh.IngressPeerName("n1")].PublicKey) != string(key(5)) {
		t.Fatalf("peers %+v", p)
	}
}

// A WireGuard key is one peer's: neither a node nor an ingress takes a key
// that another node or ingress holds, whichever came first, so that no
// node's device gives one peer's prefix to another; a removed one holds
// none. The same peer keeps its own key, with a new endpoint too.
func TestMeshKeyOwner(t *testing.T) {
	s, _ := newStore(t)
	must(t, s.InsertPodNetwork(bg, testULA))
	for _, n := range []string{"n1", "n2"} {
		mkNode(t, s, n)
	}
	for _, g := range []string{"g1", "g2"} {
		mkIngress(t, s, g)
	}
	_, err := joinMesh(t, s, "n1", key(1), "10.0.0.1:51820")
	must(t, err)
	peers := version(t, s)

	// An ingress with a node's key, and a node with it.
	if _, err := joinIngressMesh(t, s, "g1", key(1), "10.0.1.1:51820"); !errors.Is(err, store.ErrMeshKeyTaken) {
		t.Fatalf("an ingress with n1's key: %v", err)
	}
	if _, err := joinMesh(t, s, "n2", key(1), "10.0.0.2:51820"); !errors.Is(err, store.ErrMeshKeyTaken) {
		t.Fatalf("a node with n1's key: %v", err)
	}
	if g := getIngress(t, s, "g1"); g.MeshPublicKey != nil || g.MeshSubnet != 0 || version(t, s) != peers {
		t.Fatalf("a refused key changed g1 %+v or the peers", g)
	}
	// An ingress's key, taken by another ingress or a node.
	_, err = joinIngressMesh(t, s, "g1", key(2), "10.0.1.1:51820")
	must(t, err)
	if _, err := joinIngressMesh(t, s, "g2", key(2), "10.0.1.2:51820"); !errors.Is(err, store.ErrMeshKeyTaken) {
		t.Fatalf("an ingress with g1's key: %v", err)
	}
	if _, err := joinMesh(t, s, "n2", key(2), "10.0.0.2:51820"); !errors.Is(err, store.ErrMeshKeyTaken) {
		t.Fatalf("a node with g1's key: %v", err)
	}
	// The holders keep their keys, with new endpoints.
	if j, err := joinMesh(t, s, "n1", key(1), "10.0.0.11:51820"); err != nil || !j.Changed {
		t.Fatalf("n1 moved: %+v %v", j, err)
	}
	if j, err := joinIngressMesh(t, s, "g1", key(2), "10.0.1.11:51820"); err != nil || !j.Changed {
		t.Fatalf("g1 moved: %+v %v", j, err)
	}
	p, err := s.MeshPeers(bg)
	must(t, err)
	if len(p.Nodes) != 2 || !bytes.Equal(p.Nodes["n1"].PublicKey, key(1)) ||
		!bytes.Equal(p.Nodes[mesh.IngressPeerName("g1")].PublicKey, key(2)) {
		t.Fatalf("peers %+v", p)
	}
	// A node changes to a key of its own, which nobody holds.
	if _, err := joinMesh(t, s, "n1", key(3), "10.0.0.11:51820"); err != nil {
		t.Fatal(err)
	}
	// A removed node holds none: its old key is free again.
	must(t, s.InTx(bg, func(tx *store.Store) error {
		_, err := tx.RemoveNode(bg, "n1")
		return err
	}))
	if _, err := joinIngressMesh(t, s, "g2", key(3), "10.0.1.2:51820"); err != nil {
		t.Fatalf("a removed node's key: %v", err)
	}
	// So does a removed ingress, and one that joined again.
	if found, err := s.RemoveIngress(bg, "g2"); err != nil || !found {
		t.Fatalf("remove g2: %v %v", found, err)
	}
	must(t, s.RejoinIngress(bg, "g1"))
	if _, err := joinMesh(t, s, "n2", key(3), "10.0.0.2:51820"); err != nil {
		t.Fatalf("a removed ingress's key: %v", err)
	}
	if _, err := joinIngressMesh(t, s, "g1", key(3), "10.0.1.1:51820"); !errors.Is(err, store.ErrMeshKeyTaken) {
		t.Fatalf("g1 with n2's key: %v", err)
	}
}

// Two writers of one new key wait for each other on pod_network's row: the
// one that checks after the other's commit is refused.
func TestMeshKeyOwnerConcurrent(t *testing.T) {
	s, d := newStore(t)
	must(t, s.InsertPodNetwork(bg, testULA))
	mkNode(t, s, "n1")
	mkIngress(t, s, "g1")

	txA, err := d.Pool.Begin(bg)
	must(t, err)
	defer func() { _ = txA.Rollback(bg) }()
	a := store.On(txA)
	n, err := a.LockNode(bg, "n1", store.ForUpdate)
	must(t, err)
	_, err = a.JoinMesh(bg, n, testULA, key(5), "10.0.0.1:51820")
	must(t, err)

	pidB := make(chan int32, 1)
	doneB := make(chan error, 1)
	go func() {
		txB, err := d.Pool.Begin(bg)
		if err != nil {
			pidB <- 0
			doneB <- err
			return
		}
		defer func() { _ = txB.Rollback(bg) }()
		var pid int32
		if err := txB.QueryRow(bg, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			pidB <- 0
			doneB <- err
			return
		}
		pidB <- pid
		b := store.On(txB)
		g, err := b.LockIngress(bg, "g1", store.ForUpdate)
		if err != nil {
			doneB <- err
			return
		}
		if _, err := b.JoinIngressMesh(bg, g, testULA, key(5), "10.0.1.1:51820"); err != nil {
			doneB <- err
			return
		}
		doneB <- txB.Commit(bg)
	}()
	pid := <-pidB
	if pid == 0 {
		t.Fatalf("B: %v", <-doneB)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		var blocked bool
		must(t, d.Pool.QueryRow(bg, "SELECT cardinality(pg_blocking_pids($1)) > 0", pid).Scan(&blocked))
		if blocked {
			break
		}
		select {
		case err := <-doneB:
			t.Fatalf("B did not wait for A: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("B does not block on A")
		}
		time.Sleep(10 * time.Millisecond)
	}
	must(t, txA.Commit(bg))
	if err := <-doneB; !errors.Is(err, store.ErrMeshKeyTaken) {
		t.Fatalf("B after A's commit: %v", err)
	}
	if g := getIngress(t, s, "g1"); g.MeshPublicKey != nil {
		t.Fatalf("g1 got n1's key: %+v", g)
	}
}
