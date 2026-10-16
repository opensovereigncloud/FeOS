// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

func cert(serial, node, kind string) store.NodeCert {
	now := time.Now()
	return store.NodeCert{Serial: serial, Node: node, Kind: kind, KeyHash: "k-" + serial, NotBefore: now.Add(-10 * time.Minute),
		NotAfter: now.Add(7 * 24 * time.Hour), Peer: "192.0.2.1:4000"}
}

// The certificate lineage: a newer certificate presented supersedes the
// older ones one grace ahead and tells every instance; presenting an older
// one changes nothing.
func TestNodeCertLineage(t *testing.T) {
	s, d := newStore(t)
	mkNode(t, s, "n1")
	mkNode(t, s, "n2")
	ch, unsubscribe := d.Subscribe(db.ChanNode)
	defer unsubscribe()

	a, err := s.InsertNodeCert(bg, cert("aa", "n1", store.CertJoin))
	must(t, err)
	if a.IssuedAt.IsZero() || a.TokenID != uuid.Nil || !a.FirstSeenAt.IsZero() {
		t.Fatalf("inserted %+v", a)
	}
	var p store.Presentation
	tx(t, s, func(tx *store.Store) (err error) {
		p, err = tx.RecordPresentation(bg, "n1", "aa", 30*time.Minute)
		return err
	})
	if !p.FirstSeen || !p.Current || p.Superseded != 0 {
		t.Fatalf("first presentation %+v", p)
	}
	if n, _ := s.GetNode(bg, "n1"); n.CurrentSerial != "aa" {
		t.Fatalf("current serial %q", n.CurrentSerial)
	}
	b := cert("bb", "n1", store.CertRenew)
	b.PreviousSerial = "aa"
	_, err = s.InsertNodeCert(bg, b)
	must(t, err)
	tx(t, s, func(tx *store.Store) (err error) {
		p, err = tx.RecordPresentation(bg, "n1", "bb", 30*time.Minute)
		return err
	})
	if !p.FirstSeen || !p.Current || p.Superseded != 1 {
		t.Fatalf("renewal presented %+v", p)
	}
	expectPayload(t, ch, "n1")
	ca, err := s.CheckNodeCert(bg, "n1", "aa")
	must(t, err)
	if !ca.Found || ca.Revoked() || ca.RevokeAhead().IsZero() || ca.RevokedReason != store.RevokedSuperseded ||
		ca.RevokeAhead().Sub(ca.Now) < 29*time.Minute || ca.NodeStatus != model.NodeReady {
		t.Fatalf("superseded check %+v", ca)
	}
	// The old certificate presented again (its own draining connection):
	// nothing changes, no fork here.
	tx(t, s, func(tx *store.Store) (err error) {
		p, err = tx.RecordPresentation(bg, "n1", "aa", 30*time.Minute)
		return err
	})
	if p.FirstSeen || p.Current || p.Superseded != 0 {
		t.Fatalf("old presented again %+v", p)
	}
	if n, _ := s.GetNode(bg, "n1"); n.CurrentSerial != "bb" {
		t.Fatalf("current serial %q", n.CurrentSerial)
	}
	// Unknown serials, or one of another node, are not found.
	if c, err := s.CheckNodeCert(bg, "n2", "bb"); err != nil || c.Found {
		t.Fatalf("serial of another node %+v %v", c, err)
	}
	if c, _ := s.CheckNodeCert(bg, "n1", "cc"); c.Found {
		t.Fatal("unknown serial found")
	}
	if l, err := s.ListNodeCerts(bg, "n1"); err != nil || len(l) != 2 || l[0].Serial != "bb" || l[0].PreviousSerial != "aa" {
		t.Fatalf("history %+v %v", l, err)
	}

	// A fork revokes everything at once, the superseded one included.
	tx(t, s, func(tx *store.Store) error {
		if _, err := tx.LockNode(bg, "n1", store.ForUpdate); err != nil {
			return err
		}
		return tx.ForkNode(bg, "n1", "identity forked")
	})
	expectPayload(t, ch, "n1")
	for _, serial := range []string{"aa", "bb"} {
		c, _ := s.CheckNodeCert(bg, "n1", serial)
		if !c.Revoked() || c.ForkedAt.IsZero() {
			t.Fatalf("%s after the fork %+v", serial, c)
		}
	}
	if c, _ := s.CheckNodeCert(bg, "n1", "bb"); c.RevokedReason != store.RevokedForked {
		t.Fatalf("reason %q", c.RevokedReason)
	}

	// A pinned re-join resets the node and revokes what is left.
	_, err = s.InsertNodeCert(bg, cert("cc", "n1", store.CertJoin))
	must(t, err)
	tx(t, s, func(tx *store.Store) error { return tx.RejoinNode(bg, "n1") })
	expectPayload(t, ch, "n1")
	n, err := s.GetNode(bg, "n1")
	must(t, err)
	if n.Status != model.NodeJoined || n.Forked() || n.CurrentSerial != "" || n.StatusDescription != "" {
		t.Fatalf("rejoined node %+v", n)
	}
	if c, _ := s.CheckNodeCert(bg, "n1", "cc"); !c.Revoked() || c.RevokedReason != store.RevokedRejoined {
		t.Fatalf("cc after the re-join %+v", c)
	}
	// RejoinNode inserts a missing node.
	tx(t, s, func(tx *store.Store) error { return tx.RejoinNode(bg, "n9") })
	if _, err := s.GetNode(bg, "n9"); err != nil {
		t.Fatal(err)
	}

	// Removal revokes too.
	_, err = s.InsertNodeCert(bg, cert("dd", "n2", store.CertJoin))
	must(t, err)
	removed, err := s.RemoveNode(bg, "n2")
	must(t, err)
	if removed.Status != model.NodeRemoved || removed.RemovedAt.IsZero() {
		t.Fatalf("removed %+v", removed)
	}
	if c, _ := s.CheckNodeCert(bg, "n2", "dd"); !c.Revoked() || c.RevokedReason != store.RevokedRemoved || c.NodeStatus != model.NodeRemoved {
		t.Fatalf("dd after removal %+v", c)
	}
	exec(t, s, "UPDATE node_certs SET not_after = now() - interval '100 days' WHERE serial = 'aa'")
	if n, err := s.DeleteOldNodeCerts(bg, 90*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("old certs %d %v", n, err)
	}
}

func TestNodes(t *testing.T) {
	s, d := newStore(t)
	ch, unsubscribe := d.Subscribe(db.ChanNode)
	defer unsubscribe()
	if ok, err := s.InsertNode(bg, "n1"); err != nil || !ok {
		t.Fatalf("insert %v %v", ok, err)
	}
	// An unpinned join never takes over a name.
	if ok, err := s.InsertNode(bg, "n1"); err != nil || ok {
		t.Fatalf("second insert %v %v", ok, err)
	}
	if _, err := s.InsertNode(bg, "Not_A_Label"); err == nil {
		t.Fatal("a bad name was accepted")
	}
	n, err := s.GetNode(bg, "n1")
	must(t, err)
	if n.Status != model.NodeJoined || n.Slots != 50 || n.CPUOvercommit != 16 || n.MemHeadroomBytes != 1<<30 ||
		string(n.Versions) != "{}" || n.StartID != uuid.Nil {
		t.Fatalf("new node %+v", n)
	}
	if _, err := s.GetNode(bg, "nx"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("missing node: %v", err)
	}

	st := store.NodeStatic{Hostname: "h1", Arch: "x86_64", CPUModel: "EPYC", CPUCores: 8, MemoryTotalBytes: 1 << 34,
		DiskTotalBytes: 1 << 40, Versions: json.RawMessage(`{"feosd":"1.0"}`), ObjectStore: true, ObjectStoreID: "abc", PodPrefix: "2001:db8::/64"}
	if ok, err := s.UpdateNodeStatic(bg, "n1", st); err != nil || !ok {
		t.Fatalf("static %v %v", ok, err)
	}
	if ok, err := s.UpdateNodeStatic(bg, "n1", st); err != nil || ok {
		t.Fatalf("same static again %v %v", ok, err)
	}
	if n, _ := s.GetNode(bg, "n1"); n.Hostname != "h1" || n.CPUCores != 8 || n.ObjectStoreID != "abc" || string(n.Versions) != `{"feosd": "1.0"}` {
		t.Fatalf("static written %+v", n)
	}

	if ok, err := s.MarkNodeReady(bg, "n1"); err != nil || !ok {
		t.Fatalf("ready %v %v", ok, err)
	}
	if ok, _ := s.MarkNodeReady(bg, "n1"); ok {
		t.Fatal("ready twice")
	}

	start := uuid.New()
	if ok, err := s.SetNodeStart(bg, "n1", start); err != nil || !ok {
		t.Fatalf("start %v %v", ok, err)
	}
	expectPayload(t, ch, "n1")
	if ok, _ := s.SetNodeStart(bg, "n1", start); ok {
		t.Fatal("the same start is new")
	}
	until, err := s.SetNodeStopping(bg, "n1", 30*time.Second)
	must(t, err)
	expectPayload(t, ch, "n1")
	if m, err := s.StoppingUntil(bg); err != nil || !m["n1"].Equal(until) {
		t.Fatalf("stopping until %v %v", m, err)
	}
	// A new start clears the rollout deadline.
	if ok, _ := s.SetNodeStart(bg, "n1", uuid.New()); !ok {
		t.Fatal("new start")
	}
	if n, _ := s.GetNode(bg, "n1"); !n.StoppingUntil.IsZero() {
		t.Fatal("stopping_until survived a new start")
	}

	// Expiry: a node whose telemetry is older than the TTL goes down.
	_, err = s.UpsertTelemetry(bg, "n1", store.TelemetryReport{})
	must(t, err)
	if names, err := s.ExpireNodes(bg, time.Hour); err != nil || len(names) != 0 {
		t.Fatalf("fresh node expired %v %v", names, err)
	}
	exec(t, s, "UPDATE node_telemetry SET last_seen = now() - interval '1 minute'")
	names, err := s.ExpireNodes(bg, 15*time.Second)
	if err != nil || len(names) != 1 || names[0] != "n1" {
		t.Fatalf("expired %v %v", names, err)
	}
	if n, _ := s.GetNode(bg, "n1"); n.Status != model.NodeDown || n.DownSince.IsZero() || n.StatusDescription != "heartbeat expired" {
		t.Fatalf("down node %+v", n)
	}
	// Without telemetry, the join time counts.
	_, err = s.InsertNode(bg, "n2")
	must(t, err)
	exec(t, s, "UPDATE nodes SET joined_at = now() - interval '1 minute' WHERE name = 'n2'")
	if names, _ := s.ExpireNodes(bg, 15*time.Second); len(names) != 1 || names[0] != "n2" {
		t.Fatalf("expired without telemetry %v", names)
	}

	// Ready again; draining keeps its description.
	_, err = s.MarkNodeReady(bg, "n1")
	must(t, err)
	tx(t, s, func(tx *store.Store) error {
		if _, err := tx.LockNode(bg, "n1", store.ForUpdate); err != nil {
			return err
		}
		n, err := tx.SetNodeDraining(bg, "n1", true)
		if err == nil && (!n.Draining || n.DrainStartedAt.IsZero()) {
			t.Errorf("draining %+v", n)
		}
		return err
	})
	expectPayload(t, ch, "n1")
	must(t, s.SetNodeDescription(bg, "n1", "draining: 2 left"))
	exec(t, s, "UPDATE nodes SET status = 'down' WHERE name = 'n1'")
	_, err = s.MarkNodeReady(bg, "n1")
	must(t, err)
	if n, _ := s.GetNode(bg, "n1"); n.StatusDescription != "draining: 2 left" || !n.DownSince.IsZero() {
		t.Fatalf("ready while draining %+v", n)
	}
	if n, err := s.SetNodeDraining(bg, "n1", false); err != nil || n.Draining || !n.DrainStartedAt.IsZero() {
		t.Fatalf("undrain %+v %v", n, err)
	}

	slots, headroom, overcommit := 10, int64(0), 0.0
	n, err = s.UpdateNodeSettings(bg, "n1", store.NodeSettings{Slots: &slots, CPUOvercommit: &overcommit})
	if err != nil || n.Slots != 10 || n.CPUOvercommit != 0 || n.MemHeadroomBytes != 1<<30 || n.VCPUCapacity() != 0 {
		t.Fatalf("settings %+v %v", n, err)
	}
	n, err = s.UpdateNodeSettings(bg, "n1", store.NodeSettings{MemHeadroomBytes: &headroom})
	if err != nil || n.Slots != 10 || n.MemHeadroomBytes != 0 {
		t.Fatalf("settings %+v %v", n, err)
	}
	if _, err := s.UpdateNodeSettings(bg, "nx", store.NodeSettings{}); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("settings of a missing node: %v", err)
	}

	// The reconcile claim is rate limited across instances.
	if ok, err := s.ClaimReconcile(bg, "n1", 30*time.Second); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if ok, _ := s.ClaimReconcile(bg, "n1", 30*time.Second); ok {
		t.Fatal("claimed twice")
	}
	if ok, _ := s.ClaimReconcile(bg, "n2", 30*time.Second); ok {
		t.Fatal("claimed a down node")
	}
	must(t, s.SetReconciled(bg, "n1", "hash-1"))
	if tel, err := s.GetTelemetry(bg, "n1"); err != nil || tel.ReconciledHash != "hash-1" {
		t.Fatalf("reconciled hash %+v %v", tel, err)
	}
	if n, _ := s.GetNode(bg, "n1"); n.ReconciledAt.IsZero() {
		t.Fatal("reconciled_at")
	}
	if l, err := s.ListNodes(bg); err != nil || len(l) != 2 {
		t.Fatalf("list %v %v", l, err)
	}
}

func TestTelemetry(t *testing.T) {
	s, _ := newStore(t)
	mkNode(t, s, "n1")
	if _, err := s.GetTelemetry(bg, "n1"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("before a report: %v", err)
	}
	r := store.TelemetryReport{Load1: 1.5, MemoryAvailableBytes: 100, DiskAvailableBytes: 200, PodsPending: 1, PodsRunning: 2,
		PodsSuspended: 3, PodsOther: 4, InventoryHash: "h"}
	tel, err := s.UpsertTelemetry(bg, "n1", r)
	must(t, err)
	if tel.Load1 != 1.5 || tel.PodsSuspended != 3 || tel.InventoryHash != "h" || tel.LastSeen.IsZero() {
		t.Fatalf("telemetry %+v", tel)
	}
	r.PodsRunning = 7
	tel2, err := s.UpsertTelemetry(bg, "n1", r)
	must(t, err)
	if tel2.PodsRunning != 7 || !tel2.LastSeen.After(tel.LastSeen) {
		t.Fatalf("second report %+v", tel2)
	}
	if all, err := s.ListTelemetry(bg); err != nil || all["n1"].PodsRunning != 7 {
		t.Fatalf("list %v %v", all, err)
	}

	m, err := s.SetInventoryMismatch(bg, "n1", true)
	must(t, err)
	if m.Since.IsZero() || m.For != 0 {
		t.Fatalf("first mismatch %+v", m)
	}
	time.Sleep(20 * time.Millisecond)
	m2, err := s.SetInventoryMismatch(bg, "n1", true)
	must(t, err)
	if !m2.Since.Equal(m.Since) || m2.For < 10*time.Millisecond {
		t.Fatalf("persisting mismatch %+v", m2)
	}
	m3, err := s.SetInventoryMismatch(bg, "n1", false)
	must(t, err)
	if !m3.Since.IsZero() || m3.For != 0 {
		t.Fatalf("cleared %+v", m3)
	}
	if _, err := s.SetInventoryMismatch(bg, "nx", true); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("missing node: %v", err)
	}
}

// The lease statements of the leader: acquire only when free or expired,
// the epoch one up on every acquisition (also by the same holder), renew
// and fence only in the holder's epoch before expiry, release.
func TestLeases(t *testing.T) {
	s, _ := newStore(t)
	const name = store.LeaderLease
	if _, found, err := s.ReadLease(bg, name); err != nil || found {
		t.Fatalf("read before: %v %v", found, err)
	}
	epoch, ok, err := s.AcquireLease(bg, name, "i1", 15*time.Second)
	if err != nil || !ok || epoch != 1 {
		t.Fatalf("acquire: %d %v %v", epoch, ok, err)
	}
	if _, ok, err := s.AcquireLease(bg, name, "i2", 15*time.Second); err != nil || ok {
		t.Fatalf("acquire held lease: %v %v", ok, err)
	}
	if _, ok, _ := s.AcquireLease(bg, name, "i1", 15*time.Second); ok {
		t.Fatal("the holder re-acquired a valid lease")
	}
	l, found, err := s.ReadLease(bg, name)
	if err != nil || !found || l.Holder != "i1" || l.Epoch != 1 || !l.Valid || l.ExpiresAt.Sub(l.RenewedAt) != 15*time.Second {
		t.Fatalf("read %+v %v %v", l, found, err)
	}

	exp, ok, err := s.RenewLease(bg, name, "i1", 1, 15*time.Second)
	if err != nil || !ok || !exp.After(l.ExpiresAt) {
		t.Fatalf("renew %v %v %v", exp, ok, err)
	}
	if _, ok, _ := s.RenewLease(bg, name, "i1", 2, 15*time.Second); ok {
		t.Fatal("renewed in a wrong epoch")
	}
	if _, ok, _ := s.RenewLease(bg, name, "i2", 1, 15*time.Second); ok {
		t.Fatal("renewed by another holder")
	}
	tx(t, s, func(tx *store.Store) error {
		ok, err := tx.FenceLease(bg, name, "i1", 1)
		if err == nil && !ok {
			t.Error("fence of the holder failed")
		}
		if ok, _ := tx.FenceLease(bg, name, "i1", 2); ok {
			t.Error("fence in a wrong epoch passed")
		}
		return err
	})

	// Expired: anyone, the old holder included, acquires with a new epoch.
	exec(t, s, "UPDATE leases SET expires_at = now() - interval '1 second'")
	if _, ok, _ := s.RenewLease(bg, name, "i1", 1, 15*time.Second); ok {
		t.Fatal("renewed an expired lease")
	}
	if ok, _ := s.FenceLease(bg, name, "i1", 1); ok {
		t.Fatal("fenced an expired lease")
	}
	epoch, ok, err = s.AcquireLease(bg, name, "i1", 15*time.Second)
	if err != nil || !ok || epoch != 2 {
		t.Fatalf("re-acquire by the old holder: %d %v %v", epoch, ok, err)
	}
	// Release lets another take it at once.
	if ok, err := s.ReleaseLease(bg, name, "i1", 1); err != nil || ok {
		t.Fatalf("release in an old epoch: %v %v", ok, err)
	}
	if ok, err := s.ReleaseLease(bg, name, "i1", 2); err != nil || !ok {
		t.Fatalf("release: %v %v", ok, err)
	}
	epoch, ok, err = s.AcquireLease(bg, name, "i2", 15*time.Second)
	if err != nil || !ok || epoch != 3 {
		t.Fatalf("acquire after release: %d %v %v", epoch, ok, err)
	}
	// A renewal waits at most 3 s for the row lock.
	holder, err := d(t, s).Pool.Begin(bg)
	must(t, err)
	defer holder.Rollback(bg)
	_, err = holder.Exec(bg, "SELECT 1 FROM leases WHERE name = $1 FOR UPDATE", name)
	must(t, err)
	start := time.Now()
	if _, _, err := s.RenewLease(bg, name, "i2", 3, 15*time.Second); err == nil {
		t.Fatal("renewal behind a lock succeeded")
	}
	if waited := time.Since(start); waited < 2*time.Second || waited > 10*time.Second {
		t.Fatalf("renewal waited %s for the lock", waited)
	}
}

func d(t *testing.T, s *store.Store) *db.DB {
	t.Helper()
	if s.DB() == nil {
		t.Fatal("store without a DB")
	}
	return s.DB()
}

func TestInstances(t *testing.T) {
	s, _ := newStore(t)
	must(t, s.UpsertInstance(bg, store.InstanceInfo{ID: "i1", Address: ":8100", NodeAddress: "10.0.0.1:7443", Version: "v1"}))
	must(t, s.UpsertInstance(bg, store.InstanceInfo{ID: "i2", Address: ":8100", Version: "v1"}))
	must(t, s.UpsertInstance(bg, store.InstanceInfo{ID: "i3", NodeAddress: "10.0.0.3:7443"}))
	if live, err := s.InstanceLive(bg, "i1"); err != nil || !live {
		t.Fatalf("live %v %v", live, err)
	}
	must(t, s.SetInstanceStopping(bg, "i1"))
	if live, _ := s.InstanceLive(bg, "i1"); live {
		t.Fatal("a stopping instance is live")
	}
	exec(t, s, "UPDATE instances SET last_seen = now() - interval '1 minute' WHERE id = 'i3'")
	if live, _ := s.InstanceLive(bg, "i3"); live {
		t.Fatal("a silent instance is live")
	}
	if live, _ := s.InstanceLive(bg, "nope"); live {
		t.Fatal("a missing instance is live")
	}
	// Nodes see the stopping instance, not the one without a node address
	// or the silent one.
	addrs, err := s.InstancesForNodes(bg)
	if err != nil || len(addrs) != 1 || addrs[0].ID != "i1" || addrs[0].NodeAddress != "10.0.0.1:7443" {
		t.Fatalf("for nodes %+v %v", addrs, err)
	}
	// A heartbeat keeps started_at and stopping.
	in, _ := s.GetInstance(bg, "i1")
	must(t, s.UpsertInstance(bg, store.InstanceInfo{ID: "i1", Address: ":9100"}))
	in2, err := s.GetInstance(bg, "i1")
	if err != nil || !in2.StartedAt.Equal(in.StartedAt) || in2.Stopping.IsZero() || in2.Address != ":9100" || !in2.LastSeen.After(in.LastSeen) {
		t.Fatalf("heartbeat %+v %v", in2, err)
	}
	list, err := s.ListInstances(bg)
	if err != nil || len(list) != 3 || list[0].Live || !list[1].Live || list[2].Live {
		t.Fatalf("list %+v %v", list, err)
	}
	exec(t, s, "UPDATE instances SET last_seen = now() - interval '2 hours' WHERE id = 'i3'")
	if n, err := s.DeleteStaleInstances(bg, time.Hour); err != nil || n != 1 {
		t.Fatalf("stale %d %v", n, err)
	}
	must(t, s.DeleteInstance(bg, "i1"))
	if _, err := s.GetInstance(bg, "i1"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("deleted instance: %v", err)
	}
}

func TestJoinTokens(t *testing.T) {
	s, _ := newStore(t)
	jt, err := s.CreateJoinToken(bg, "h1", "admin", "", time.Hour)
	must(t, err)
	if jt.ID == uuid.Nil || jt.Used() || jt.ExpiresAt.Sub(jt.CreatedAt) != time.Hour {
		t.Fatalf("token %+v", jt)
	}
	pinned, err := s.CreateJoinToken(bg, "h2", "admin", "n1", time.Hour)
	must(t, err)
	tx(t, s, func(tx *store.Store) error {
		got, err := tx.LockJoinToken(bg, "h1")
		if err != nil {
			return err
		}
		if got.Hash != "h1" || got.NodeName != "" {
			t.Errorf("locked %+v", got)
		}
		return tx.MarkJoinTokenUsed(bg, "h1", "n7", "kh", "serial", "192.0.2.7:1")
	})
	got, err := s.LockJoinToken(bg, "h1")
	if err != nil || !got.Used() || got.UsedName != "n7" || got.UsedKeyHash != "kh" || got.UsedSerial != "serial" || got.UsedPeer != "192.0.2.7:1" {
		t.Fatalf("used %+v %v", got, err)
	}
	if _, err := s.LockJoinToken(bg, "nope"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown: %v", err)
	}
	if err := s.MarkJoinTokenUsed(bg, "nope", "n", "k", "s", "p"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("mark unknown: %v", err)
	}
	l, err := s.ListJoinTokens(bg)
	if err != nil || len(l) != 2 || l[0].ID != pinned.ID {
		t.Fatalf("list %+v %v", l, err)
	}
	exec(t, s, "UPDATE join_tokens SET used_at = now() - interval '40 days' WHERE hash = 'h1'")
	if n, err := s.DeleteStaleJoinTokens(bg, 30*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("stale %d %v", n, err)
	}
	if ok, err := s.DeleteJoinToken(bg, pinned.ID); err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
}
