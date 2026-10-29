// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

func mkIngress(t *testing.T, s *store.Store, name string) {
	t.Helper()
	if created, err := s.InsertIngress(bg, name); err != nil || !created {
		t.Fatalf("insert ingress %s: %v %v", name, created, err)
	}
}

func getIngress(t *testing.T, s *store.Store, name string) store.Ingress {
	t.Helper()
	g, err := s.GetIngress(bg, name)
	must(t, err)
	return g
}

// joinIngressMesh runs JoinIngressMesh for ingress name in a transaction,
// under its row lock, as the ingress service's Hello does.
func joinIngressMesh(t *testing.T, s *store.Store, name string, k []byte, endpoint string) (store.IngressMeshJoin, error) {
	t.Helper()
	var j store.IngressMeshJoin
	err := s.InTx(bg, func(tx *store.Store) error {
		g, err := tx.LockIngress(bg, name, store.ForUpdate)
		if err != nil {
			return err
		}
		j, err = tx.JoinIngressMesh(bg, g, testULA, k, endpoint)
		return err
	})
	return j, err
}

// groupVersion returns pod_network.ingress_group_version and the members
// of ferun-ingress.
func groupVersion(t *testing.T, s *store.Store) (int64, []string) {
	t.Helper()
	members, v, err := s.IngressGroup(bg)
	must(t, err)
	return v, members
}

func ingressCert(serial, ingress, kind string, issuedAgo time.Duration) store.IngressCert {
	now := time.Now()
	return store.IngressCert{Serial: serial, Ingress: ingress, Kind: kind, KeyHash: "k-" + serial,
		NotBefore: now.Add(-issuedAgo - time.Minute), NotAfter: now.Add(7 * 24 * time.Hour), Peer: "192.0.2.1:4000"}
}

// insertIngressCert records a certificate issued issuedAgo before now().
func insertIngressCert(t *testing.T, s *store.Store, c store.IngressCert, issuedAgo time.Duration) {
	t.Helper()
	if _, err := s.InsertIngressCert(bg, c); err != nil {
		t.Fatal(err)
	}
	exec(t, s, "UPDATE ingress_certs SET issued_at = now() - $2::interval WHERE serial = $1", c.Serial,
		fmt.Sprintf("%d microseconds", issuedAgo.Microseconds()))
}

func presentIngressCert(t *testing.T, s *store.Store, name, serial string) int64 {
	t.Helper()
	n, err := s.PresentIngressCert(bg, name, serial, 10*time.Minute)
	must(t, err)
	return n
}

// Nodes and ingresses take their subnets from one sequence, so no two get
// the same /96; an ingress's address is <prefix>::1.
func TestIngressSubnets(t *testing.T) {
	s, _ := newStore(t)
	if members, v, err := s.IngressGroup(bg); err != nil || members != nil || v != 0 {
		t.Fatalf("ferun-ingress of a routed cluster: %v %d %v", members, v, err)
	}
	mkIngress(t, s, "g0")
	must(t, s.InTx(bg, func(tx *store.Store) error {
		g, err := tx.LockIngress(bg, "g0", store.ForUpdate)
		must(t, err)
		if _, err := tx.JoinIngressMesh(bg, g, netip.Prefix{}, key(9), "10.0.1.9:51820"); !errors.Is(err, store.ErrNotMeshed) {
			t.Errorf("an ingress on a routed cluster: %v", err)
		}
		return nil
	}))
	must(t, s.InsertPodNetwork(bg, testULA))
	mkNode(t, s, "n1")
	mkNode(t, s, "n2")
	mkIngress(t, s, "g1")
	mkIngress(t, s, "g2")
	if created, err := s.InsertIngress(bg, "g1"); err != nil || created {
		t.Fatalf("a second g1: %v %v", created, err)
	}
	n1, err := joinMesh(t, s, "n1", key(1), "10.0.0.1:51820")
	must(t, err)
	g1, err := joinIngressMesh(t, s, "g1", key(2), "10.0.1.1:51820")
	must(t, err)
	n2, err := joinMesh(t, s, "n2", key(3), "10.0.0.2:51820")
	must(t, err)
	g2, err := joinIngressMesh(t, s, "g2", key(4), "10.0.1.2:51820")
	must(t, err)
	got := []int{n1.Subnet, g1.Subnet, n2.Subnet, g2.Subnet}
	if !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Fatalf("subnets %v", got)
	}
	if g1.Prefix != "fd5e:1b2c:93a0::2:0:0/96" || g1.Address != "fd5e:1b2c:93a0::2:0:1" || !g1.Changed || !g1.AddressChanged ||
		g2.Prefix != "fd5e:1b2c:93a0::4:0:0/96" || g2.Address != "fd5e:1b2c:93a0::4:0:1" {
		t.Fatalf("ingress joins %+v %+v", g1, g2)
	}
	if a := mesh.HostAddress(netip.MustParsePrefix(g2.Prefix)).String(); a != g2.Address {
		t.Fatalf("address %s, host address %s", g2.Address, a)
	}
	row := getIngress(t, s, "g2")
	if row.MeshSubnet != 4 || row.MeshPrefix != g2.Prefix || row.MeshAddress != g2.Address || row.MeshEndpoint != "10.0.1.2:51820" ||
		!row.MeshPeer() || row.Status != model.IngressJoined || row.JoinedAt.IsZero() {
		t.Fatalf("row %+v", row)
	}
	list, err := s.ListIngresses(bg)
	if err != nil || len(list) != 3 || list[0].Name != "g0" || list[1].Name != "g1" || list[2].Name != "g2" {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := s.GetIngress(bg, "nope"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown ingress: %v", err)
	}
	// The sequence is shared up to its end.
	exec(t, s, "SELECT setval('mesh_subnets', 4294967295)")
	if _, err := joinIngressMesh(t, s, "g0", key(9), "10.0.1.9:51820"); !errors.Is(err, store.ErrMeshExhausted) {
		t.Fatalf("past the last subnet: %v", err)
	}
}

// JoinIngressMesh repeated changes nothing; a new endpoint counts for the
// peers and tells the leader and the ingress's builders; the key is fixed
// until the ingress joins again.
func TestJoinIngressMesh(t *testing.T) {
	s, d := newStore(t)
	must(t, s.InsertPodNetwork(bg, testULA))
	mkIngress(t, s, "g1")
	leader, unsubscribeLeader := d.Subscribe(db.ChanLeader)
	defer unsubscribeLeader()
	ingress, unsubscribeIngress := d.Subscribe(db.ChanIngress)
	defer unsubscribeIngress()

	j, err := joinIngressMesh(t, s, "g1", key(1), "10.0.1.1:51820")
	must(t, err)
	expectPayload(t, leader, store.HintEnvGroups)
	expectPayload(t, leader, store.HintMeshPeers)
	expectPayload(t, ingress, store.IngressPayloadMesh)
	expectPayload(t, ingress, store.IngressNamePayload("g1"))
	peers, gv := version(t, s), getIngress(t, s, "g1").GroupVersion

	again, err := joinIngressMesh(t, s, "g1", key(1), "10.0.1.1:51820")
	must(t, err)
	if again != (store.IngressMeshJoin{Subnet: j.Subnet, Prefix: j.Prefix, Address: j.Address}) || version(t, s) != peers ||
		getIngress(t, s, "g1").GroupVersion != gv {
		t.Fatalf("a repeat %+v changed the versions", again)
	}
	expectNoPayload(t, ingress, store.IngressPayloadMesh)

	moved, err := joinIngressMesh(t, s, "g1", key(1), "10.0.1.11:51820")
	must(t, err)
	if !moved.Changed || moved.AddressChanged || moved.Subnet != j.Subnet || version(t, s) != peers+1 {
		t.Fatalf("a new endpoint %+v, peers' version %d", moved, version(t, s))
	}
	if v, _ := groupVersion(t, s); v != gv {
		t.Fatalf("a new endpoint changed ferun-ingress's version to %d", v)
	}
	expectPayload(t, leader, store.HintMeshPeers)
	expectPayload(t, ingress, store.IngressPayloadMesh)
	if p, _ := s.MeshPeers(bg); p.Nodes[mesh.IngressPeerName("g1")].Endpoint != "10.0.1.11:51820" {
		t.Fatalf("peers %+v", p)
	}

	// Another key: refused while the row holds one, also with nothing
	// else changing.
	if _, err := joinIngressMesh(t, s, "g1", key(2), "10.0.1.11:51820"); !errors.Is(err, store.ErrIngressKeyFixed) {
		t.Fatalf("a new key: %v", err)
	}
	if _, err := joinIngressMesh(t, s, "g1", nil, "10.0.1.11:51820"); !errors.Is(err, store.ErrIngressKeyFixed) {
		t.Fatalf("no key: %v", err)
	}
	if g := getIngress(t, s, "g1"); string(g.MeshPublicKey) != string(key(1)) {
		t.Fatalf("the key changed: %+v", g)
	}

	// A join again clears the key and retires it: the next Hello must
	// bring another, which is taken, on the same subnet and address.
	must(t, s.RejoinIngress(bg, "g1"))
	peersBefore := version(t, s)
	if _, err := joinIngressMesh(t, s, "g1", key(1), "10.0.1.11:51820"); !errors.Is(err, store.ErrIngressKeyRetired) {
		t.Fatalf("the retired key after the rejoin: %v", err)
	}
	if g := getIngress(t, s, "g1"); g.MeshPublicKey != nil || string(g.PreviousMeshPublicKey) != string(key(1)) ||
		version(t, s) != peersBefore {
		t.Fatalf("a refused retired key changed the row %+v or the peers", g)
	}
	j2, err := joinIngressMesh(t, s, "g1", key(2), "10.0.1.11:51820")
	must(t, err)
	if j2.Subnet != j.Subnet || j2.Address != j.Address || !j2.Changed || j2.AddressChanged {
		t.Fatalf("after the rejoin %+v", j2)
	}
	if g := getIngress(t, s, "g1"); string(g.MeshPublicKey) != string(key(2)) || g.GroupVersion != gv {
		t.Fatalf("after the rejoin %+v", g)
	}
	if _, err := joinIngressMesh(t, s, "g1", key(1), "10.0.1.11:51820"); !errors.Is(err, store.ErrIngressKeyFixed) {
		t.Fatalf("the old key after the rejoin: %v", err)
	}
}

// The first address of an ingress enters ferun-ingress at a new version,
// which the row keeps; two ingresses joining at once get versions in the
// order their transactions commit, and each version shows exactly the
// members committed with it.
func TestIngressGroupCommitOrder(t *testing.T) {
	s, d := newStore(t)
	must(t, s.InsertPodNetwork(bg, testULA))
	mkIngress(t, s, "a")
	mkIngress(t, s, "b")
	if v, members := groupVersion(t, s); v != 0 || len(members) != 0 {
		t.Fatalf("ferun-ingress before any address: %d %v", v, members)
	}

	txA, err := d.Pool.Begin(bg)
	must(t, err)
	defer func() { _ = txA.Rollback(bg) }()
	a := store.On(txA)
	ga, err := a.LockIngress(bg, "a", store.ForUpdate)
	must(t, err)
	ja, err := a.JoinIngressMesh(bg, ga, testULA, key(1), "10.0.1.1:51820")
	must(t, err)
	if !ja.AddressChanged {
		t.Fatalf("a's join %+v", ja)
	}

	type joined struct {
		j   store.IngressMeshJoin
		err error
	}
	pidB := make(chan int32, 1)
	joinedB := make(chan joined, 1)
	commitB := make(chan struct{})
	doneB := make(chan error, 1)
	go func() {
		txB, err := d.Pool.Begin(bg)
		if err != nil {
			pidB <- 0
			joinedB <- joined{err: err}
			return
		}
		defer func() { _ = txB.Rollback(bg) }()
		var pid int32
		if err := txB.QueryRow(bg, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			pidB <- 0
			joinedB <- joined{err: err}
			return
		}
		pidB <- pid
		b := store.On(txB)
		gb, err := b.LockIngress(bg, "b", store.ForUpdate)
		if err != nil {
			joinedB <- joined{err: err}
			return
		}
		j, err := b.JoinIngressMesh(bg, gb, testULA, key(2), "10.0.1.2:51820")
		joinedB <- joined{j, err}
		if err != nil {
			return
		}
		<-commitB
		doneB <- txB.Commit(bg)
	}()
	defer close(commitB)

	// B blocks on pod_network's row, which A holds.
	pid := <-pidB
	if pid == 0 {
		t.Fatalf("B: %v", (<-joinedB).err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		var blocked bool
		must(t, d.Pool.QueryRow(bg, "SELECT cardinality(pg_blocking_pids($1)) > 0", pid).Scan(&blocked))
		if blocked {
			break
		}
		select {
		case r := <-joinedB:
			t.Fatalf("B did not wait for A: %+v", r)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("B does not block on A")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if v, members := groupVersion(t, s); v != 0 || len(members) != 0 {
		t.Fatalf("ferun-ingress before a commit: %d %v", v, members)
	}

	must(t, txA.Commit(bg))
	rb := <-joinedB
	must(t, rb.err)
	if !rb.j.AddressChanged {
		t.Fatalf("b's join %+v", rb.j)
	}
	rowA := getIngress(t, s, "a")
	if v, members := groupVersion(t, s); v != rowA.GroupVersion || v == 0 || !slices.Equal(members, []string{ja.Address}) {
		t.Fatalf("ferun-ingress after A's commit: %d %v; a at %d", v, members, rowA.GroupVersion)
	}

	commitB <- struct{}{}
	must(t, <-doneB)
	rowB := getIngress(t, s, "b")
	if rowB.GroupVersion <= rowA.GroupVersion {
		t.Fatalf("B committed after A at version %d, A at %d", rowB.GroupVersion, rowA.GroupVersion)
	}
	if v, members := groupVersion(t, s); v != rowB.GroupVersion || !slices.Equal(members, []string{ja.Address, rb.j.Address}) {
		t.Fatalf("ferun-ingress after B's commit: %d %v", v, members)
	}
}

// A join again keeps the ingress's place on the mesh, clears its key,
// endpoint and current certificate, and revokes its certificates; a
// removed ingress comes back into ferun-ingress.
func TestRejoinIngress(t *testing.T) {
	s, d := newStore(t)
	must(t, s.InsertPodNetwork(bg, testULA))
	mkIngress(t, s, "g1")
	j, err := joinIngressMesh(t, s, "g1", key(1), "10.0.1.1:51820")
	must(t, err)
	insertIngressCert(t, s, ingressCert("aa", "g1", store.CertJoin, time.Hour), time.Hour)
	presentIngressCert(t, s, "g1", "aa")
	before := getIngress(t, s, "g1")
	peers := version(t, s)
	ingress, unsubscribe := d.Subscribe(db.ChanIngress)
	defer unsubscribe()

	must(t, s.RejoinIngress(bg, "g1"))
	g := getIngress(t, s, "g1")
	if g.MeshSubnet != j.Subnet || g.MeshPrefix != j.Prefix || g.MeshAddress != j.Address || g.MeshPublicKey != nil ||
		g.MeshEndpoint != "" || g.CurrentSerial != "" || g.MeshPeer() || g.GroupVersion != before.GroupVersion ||
		g.Status != model.IngressJoined || string(g.PreviousMeshPublicKey) != string(key(1)) {
		t.Fatalf("rejoined %+v", g)
	}
	c, err := s.CheckIngressCert(bg, "g1", "aa")
	must(t, err)
	if !c.Found || !c.Revoked() || c.RevokedReason != store.RevokedRejoined {
		t.Fatalf("the certificate after the rejoin %+v", c)
	}
	if v := version(t, s); v != peers+1 {
		t.Fatalf("peers' version %d after the rejoin of a peer, was %d", v, peers)
	}
	expectPayload(t, ingress, store.IngressNamePayload("g1"))
	expectPayload(t, ingress, store.IngressPayloadMesh)
	if p, _ := s.MeshPeers(bg); len(p.Nodes) != 0 {
		t.Fatalf("a rejoined ingress is a peer before its Hello: %+v", p)
	}
	if v, members := groupVersion(t, s); v != before.GroupVersion || !slices.Equal(members, []string{j.Address}) {
		t.Fatalf("ferun-ingress after the rejoin: %d %v", v, members)
	}

	// A rejoin that is not a peer's leaves the peers alone, and keeps the
	// key retired.
	must(t, s.RejoinIngress(bg, "g1"))
	if v := version(t, s); v != peers+1 {
		t.Fatalf("a keyless rejoin changed the peers' version to %d", v)
	}
	if g := getIngress(t, s, "g1"); string(g.PreviousMeshPublicKey) != string(key(1)) {
		t.Fatalf("a keyless rejoin forgot the retired key: %+v", g)
	}

	// A pinned token for a name that does not exist makes it.
	must(t, s.RejoinIngress(bg, "g2"))
	if g := getIngress(t, s, "g2"); g.Status != model.IngressJoined || g.MeshSubnet != 0 {
		t.Fatalf("rejoined new %+v", g)
	}

	// A removed ingress joins again and its address is a member again, at
	// a new version.
	found, err := s.RemoveIngress(bg, "g1")
	if err != nil || !found {
		t.Fatalf("remove %v %v", found, err)
	}
	removedAt, _ := groupVersion(t, s)
	must(t, s.RejoinIngress(bg, "g1"))
	g = getIngress(t, s, "g1")
	if g.Status != model.IngressJoined || !g.RemovedAt.IsZero() || g.GroupVersion != removedAt+1 || g.MeshAddress != j.Address {
		t.Fatalf("a removed ingress rejoined %+v; removed at version %d", g, removedAt)
	}
	if v, members := groupVersion(t, s); v != g.GroupVersion || !slices.Equal(members, []string{j.Address}) {
		t.Fatalf("ferun-ingress after the removed one's rejoin: %d %v", v, members)
	}
}

// Removing an ingress revokes its certificates, takes it out of
// ferun-ingress at a new version and off the mesh; its row stays.
func TestRemoveIngress(t *testing.T) {
	s, d := newStore(t)
	must(t, s.InsertPodNetwork(bg, testULA))
	mkIngress(t, s, "g1")
	mkIngress(t, s, "g2")
	j1, err := joinIngressMesh(t, s, "g1", key(1), "10.0.1.1:51820")
	must(t, err)
	j2, err := joinIngressMesh(t, s, "g2", key(2), "10.0.1.2:51820")
	must(t, err)
	insertIngressCert(t, s, ingressCert("aa", "g1", store.CertJoin, time.Hour), time.Hour)
	gv, members := groupVersion(t, s)
	if !slices.Equal(members, []string{j1.Address, j2.Address}) {
		t.Fatalf("members %v", members)
	}
	peers := version(t, s)
	leader, unsubscribeLeader := d.Subscribe(db.ChanLeader)
	defer unsubscribeLeader()
	ingress, unsubscribeIngress := d.Subscribe(db.ChanIngress)
	defer unsubscribeIngress()

	found, err := s.RemoveIngress(bg, "g1")
	if err != nil || !found {
		t.Fatalf("remove %v %v", found, err)
	}
	g := getIngress(t, s, "g1")
	if g.Status != model.IngressRemoved || g.RemovedAt.IsZero() || g.GroupVersion != gv+1 {
		t.Fatalf("removed %+v", g)
	}
	if c, _ := s.CheckIngressCert(bg, "g1", "aa"); !c.Revoked() || c.RevokedReason != store.RevokedRemoved ||
		c.IngressStatus != model.IngressRemoved {
		t.Fatalf("the certificate after the removal %+v", c)
	}
	if v, members := groupVersion(t, s); v != gv+1 || !slices.Equal(members, []string{j2.Address}) {
		t.Fatalf("ferun-ingress after the removal: %d %v", v, members)
	}
	p, err := s.MeshPeers(bg)
	must(t, err)
	if _, ok := p.Nodes[mesh.IngressPeerName("g1")]; ok || len(p.Nodes) != 1 || p.Version != peers+1 {
		t.Fatalf("peers after the removal %+v", p)
	}
	expectPayload(t, leader, store.HintEnvGroups)
	expectPayload(t, leader, store.HintMeshPeers)
	expectPayload(t, ingress, store.IngressNamePayload("g1"))
	expectPayload(t, ingress, store.IngressPayloadMesh)

	// Again: nothing more to count.
	if found, err := s.RemoveIngress(bg, "g1"); err != nil || !found {
		t.Fatalf("remove again %v %v", found, err)
	}
	if v, _ := groupVersion(t, s); v != gv+1 || version(t, s) != peers+1 {
		t.Fatalf("a second removal counted: %d %d", v, version(t, s))
	}
	if again := getIngress(t, s, "g1"); !again.RemovedAt.Equal(g.RemovedAt) {
		t.Fatalf("a second removal moved removed_at: %v %v", again.RemovedAt, g.RemovedAt)
	}
	if found, err := s.RemoveIngress(bg, "nope"); err != nil || found {
		t.Fatalf("remove unknown %v %v", found, err)
	}
	// An ingress without an address leaves no version behind.
	mkIngress(t, s, "g3")
	if found, err := s.RemoveIngress(bg, "g3"); err != nil || !found {
		t.Fatalf("remove g3 %v %v", found, err)
	}
	if v, _ := groupVersion(t, s); v != gv+1 {
		t.Fatalf("an addressless removal counted: %d", v)
	}
	// Its subnet is never given out again.
	mkNode(t, s, "n1")
	if j, err := joinMesh(t, s, "n1", key(9), "10.0.0.1:51820"); err != nil || j.Subnet != 3 {
		t.Fatalf("n1 after the removal %+v %v", j, err)
	}
}

// A newer certificate presented supersedes the older ones one grace ahead
// and tells every instance; an older one presented supersedes nothing.
func TestPresentIngressCert(t *testing.T) {
	s, d := newStore(t)
	mkIngress(t, s, "g1")
	mkIngress(t, s, "g2")
	insertIngressCert(t, s, ingressCert("aa", "g1", store.CertJoin, 2*time.Hour), 2*time.Hour)
	insertIngressCert(t, s, ingressCert("a0", "g1", store.CertJoin, 3*time.Hour), 3*time.Hour)
	insertIngressCert(t, s, ingressCert("cc", "g2", store.CertJoin, time.Hour), time.Hour)
	ingress, unsubscribe := d.Subscribe(db.ChanIngress)
	defer unsubscribe()
	// The newest certificate not revoked, while none is current.
	if newest, err := s.NewestIngressCert(bg, "g1"); err != nil || newest != "aa" {
		t.Fatalf("newest of g1 %q %v", newest, err)
	}
	if newest, err := s.NewestIngressCert(bg, "nope"); err != nil || newest != "" {
		t.Fatalf("newest of nobody %q %v", newest, err)
	}

	// The first presentation: current, and the older certificate is
	// superseded.
	if n := presentIngressCert(t, s, "g1", "aa"); n != 1 {
		t.Fatalf("first presentation superseded %d", n)
	}
	if g := getIngress(t, s, "g1"); g.CurrentSerial != "aa" {
		t.Fatalf("current serial %q", g.CurrentSerial)
	}
	expectPayload(t, ingress, store.IngressNamePayload("g1"))

	b := ingressCert("bb", "g1", store.CertRenew, time.Hour)
	b.PreviousSerial = "aa"
	insertIngressCert(t, s, b, time.Hour)
	if n := presentIngressCert(t, s, "g1", "bb"); n != 1 {
		t.Fatalf("the renewal superseded %d", n)
	}
	expectPayload(t, ingress, store.IngressNamePayload("g1"))
	// A superseded one is revoked ahead: it is not the newest any more.
	if newest, err := s.NewestIngressCert(bg, "g1"); err != nil || newest != "bb" {
		t.Fatalf("newest of g1 after the renewal %q %v", newest, err)
	}
	ca, err := s.CheckIngressCert(bg, "g1", "aa")
	must(t, err)
	if !ca.Found || ca.Revoked() || ca.RevokedReason != store.RevokedSuperseded || ca.RevokeAhead().IsZero() ||
		ca.RevokeAhead().Sub(ca.Now) < 9*time.Minute || ca.RevokeAhead().Sub(ca.Now) > 10*time.Minute {
		t.Fatalf("superseded %+v", ca)
	}
	// The other ingress's certificate is untouched.
	if cc, _ := s.CheckIngressCert(bg, "g2", "cc"); !cc.RevokedAt.IsZero() {
		t.Fatalf("another ingress's certificate %+v", cc)
	}

	// The old one presented again (its draining connection): nothing.
	if n := presentIngressCert(t, s, "g1", "aa"); n != 0 {
		t.Fatalf("an older certificate superseded %d", n)
	}
	if n := presentIngressCert(t, s, "g1", "bb"); n != 0 {
		t.Fatalf("the current certificate again superseded %d", n)
	}
	if g := getIngress(t, s, "g1"); g.CurrentSerial != "bb" {
		t.Fatalf("current serial %q", g.CurrentSerial)
	}
	if cb, _ := s.CheckIngressCert(bg, "g1", "bb"); !cb.RevokedAt.IsZero() {
		t.Fatalf("the current certificate %+v", cb)
	}
	expectNoPayload(t, ingress, store.IngressNamePayload("g1"))

	for _, tc := range []struct{ name, serial string }{{"g1", "nope"}, {"g1", "cc"}, {"nope", "aa"}} {
		if _, err := s.PresentIngressCert(bg, tc.name, tc.serial, time.Minute); !errors.Is(err, store.ErrNoRow) {
			t.Fatalf("present %s of %s: %v", tc.serial, tc.name, err)
		}
	}
	certs, err := s.ListIngressCerts(bg, "g1")
	if err != nil || len(certs) != 3 || certs[0].Serial != "bb" || certs[0].PreviousSerial != "aa" ||
		certs[0].Kind != store.CertRenew || certs[2].Serial != "a0" {
		t.Fatalf("list %+v %v", certs, err)
	}
	c, err := s.GetIngressCert(bg, "bb")
	if err != nil || c.Ingress != "g1" || c.KeyHash != "k-bb" || c.TokenID != uuid.Nil || c.Peer != "192.0.2.1:4000" {
		t.Fatalf("get %+v %v", c, err)
	}
	if _, err := s.GetIngressCert(bg, "nope"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("get unknown: %v", err)
	}
}

// CheckIngressCert reads what a connection's check decides on.
func TestCheckIngressCert(t *testing.T) {
	s, _ := newStore(t)
	mkIngress(t, s, "g1")
	mkIngress(t, s, "g2")
	token := uuid.New()
	fresh := ingressCert("f1", "g1", store.CertJoin, time.Hour)
	fresh.TokenID = token
	insertIngressCert(t, s, fresh, time.Hour)
	for _, serial := range []string{"s1", "s2", "r1", "e1"} {
		insertIngressCert(t, s, ingressCert(serial, "g1", store.CertRenew, time.Hour), time.Hour)
	}
	insertIngressCert(t, s, ingressCert("o1", "g2", store.CertJoin, time.Hour), time.Hour)
	exec(t, s, "UPDATE ingress_certs SET revoked_at = now() + interval '5 minutes', revoked_reason = 'superseded' WHERE serial = 's1'")
	exec(t, s, "UPDATE ingress_certs SET revoked_at = now() - interval '1 second', revoked_reason = 'superseded' WHERE serial = 's2'")
	exec(t, s, "UPDATE ingress_certs SET revoked_at = now(), revoked_reason = 'rejoined' WHERE serial = 'r1'")
	exec(t, s, "UPDATE ingress_certs SET not_after = now() - interval '1 second' WHERE serial = 'e1'")

	check := func(name, serial string) store.IngressCertCheck {
		t.Helper()
		c, err := s.CheckIngressCert(bg, name, serial)
		must(t, err)
		return c
	}
	if c := check("g1", "f1"); !c.Found || c.IngressStatus != model.IngressJoined || c.Revoked() || !c.RevokeAhead().IsZero() ||
		c.Expired() || c.RevokedReason != "" || c.Now.IsZero() || c.NotAfter.Sub(c.Now) < 6*24*time.Hour {
		t.Fatalf("fresh %+v", c)
	}
	if c, _ := s.GetIngressCert(bg, "f1"); c.TokenID != token {
		t.Fatalf("token id %v", c.TokenID)
	}
	if c := check("g1", "s1"); !c.Found || c.Revoked() || c.RevokeAhead().IsZero() || c.RevokedReason != store.RevokedSuperseded {
		t.Fatalf("superseded ahead %+v", c)
	}
	if c := check("g1", "s2"); !c.Revoked() || !c.RevokeAhead().IsZero() || c.RevokedReason != store.RevokedSuperseded {
		t.Fatalf("superseded past %+v", c)
	}
	if c := check("g1", "r1"); !c.Revoked() || c.RevokedReason != store.RevokedRejoined {
		t.Fatalf("revoked %+v", c)
	}
	if c := check("g1", "e1"); !c.Expired() || c.Revoked() {
		t.Fatalf("expired %+v", c)
	}
	if c := check("g1", "nope"); c.Found {
		t.Fatalf("unknown %+v", c)
	}
	if c := check("g1", "o1"); c.Found {
		t.Fatalf("another ingress's %+v", c)
	}
	if c := check("nope", "f1"); c.Found {
		t.Fatalf("an unknown ingress %+v", c)
	}
	// RevokeIngressCerts revokes the unrevoked and the ones ahead, from
	// now.
	n, err := s.RevokeIngressCerts(bg, "g1", store.RevokedRemoved)
	if err != nil || n != 3 {
		t.Fatalf("revoked %d %v", n, err)
	}
	if c := check("g1", "s1"); !c.Revoked() || c.RevokedReason != store.RevokedRemoved {
		t.Fatalf("superseded ahead, then revoked %+v", c)
	}
	if c := check("g1", "r1"); c.RevokedReason != store.RevokedRejoined {
		t.Fatalf("revoked twice %+v", c)
	}
	if c := check("g2", "o1"); c.Revoked() {
		t.Fatalf("another ingress's revoked %+v", c)
	}
}

// SeenIngress records the last sight and the version, and clears left_at.
func TestSeenIngress(t *testing.T) {
	s, _ := newStore(t)
	mkIngress(t, s, "g1")
	if g := getIngress(t, s, "g1"); !g.LastSeen.IsZero() || !g.LeftAt.IsZero() || g.Version != "" {
		t.Fatalf("new %+v", g)
	}
	exec(t, s, "UPDATE ingresses SET left_at = now() - interval '1 minute' WHERE name = 'g1'")
	must(t, s.SeenIngress(bg, "g1", "v1.2.3"))
	g := getIngress(t, s, "g1")
	if g.LastSeen.IsZero() || !g.LeftAt.IsZero() || g.Version != "v1.2.3" {
		t.Fatalf("seen %+v", g)
	}
	must(t, s.SeenIngress(bg, "g1", ""))
	if g := getIngress(t, s, "g1"); g.Version != "v1.2.3" {
		t.Fatalf("an unknown version replaced the known one: %+v", g)
	}
	if err := s.SeenIngress(bg, "nope", "v1"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("seen unknown: %v", err)
	}
}

// A node's version of ferun-ingress is written once per change, which
// tells the ingress's builders; a rejoin clears it.
func TestSetNodeIngressGroupVersion(t *testing.T) {
	s, d := newStore(t)
	mkNode(t, s, "n1")
	ingress, unsubscribe := d.Subscribe(db.ChanIngress)
	defer unsubscribe()
	if n, _ := s.GetNode(bg, "n1"); n.IngressGroupVersion != 0 {
		t.Fatalf("new node %+v", n)
	}
	if ok, err := s.SetNodeIngressGroupVersion(bg, "n1", 3); err != nil || !ok {
		t.Fatalf("set %v %v", ok, err)
	}
	expectPayload(t, ingress, store.IngressPayloadAll)
	if n, _ := s.GetNode(bg, "n1"); n.IngressGroupVersion != 3 {
		t.Fatalf("version %d", n.IngressGroupVersion)
	}
	if ok, err := s.SetNodeIngressGroupVersion(bg, "n1", 3); err != nil || ok {
		t.Fatalf("set again %v %v", ok, err)
	}
	expectNoPayload(t, ingress, store.IngressPayloadAll)
	if ok, err := s.SetNodeIngressGroupVersion(bg, "nope", 3); err != nil || ok {
		t.Fatalf("set unknown %v %v", ok, err)
	}
	nodes, err := s.ListNodes(bg)
	if err != nil || len(nodes) != 1 || nodes[0].IngressGroupVersion != 3 {
		t.Fatalf("list %+v %v", nodes, err)
	}
	must(t, s.InTx(bg, func(tx *store.Store) error { return tx.RejoinNode(bg, "n1") }))
	if n, _ := s.GetNode(bg, "n1"); n.IngressGroupVersion != 0 {
		t.Fatalf("after the rejoin %d", n.IngressGroupVersion)
	}
	expectPayload(t, ingress, store.IngressPayloadAll)
}

func TestIngressPayloads(t *testing.T) {
	id := uuid.MustParse("6f1c1f4e-2b55-4c8e-9a0d-0e5a3b8f2c11")
	if p := store.IngressEnvPayload(id); p != "env:6f1c1f4e-2b55-4c8e-9a0d-0e5a3b8f2c11" {
		t.Fatalf("env payload %q", p)
	}
	if p := store.IngressNamePayload("ferun-ingress-0"); p != "ingress:ferun-ingress-0" {
		t.Fatalf("name payload %q", p)
	}
	if store.IngressPayloadAll != "*" || store.IngressPayloadMesh != "mesh" {
		t.Fatal("payload constants")
	}
}
