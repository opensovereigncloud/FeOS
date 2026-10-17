// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/db/dbtest"
	"github.com/ironcore-dev/feos/go/internal/leader"
	"github.com/ironcore-dev/feos/go/internal/model"
)

func TestLoopsList(t *testing.T) {
	h := newHarness(t)
	want := map[string]time.Duration{
		LoopNodeExpiry: 5 * time.Second, LoopReconcileSweep: time.Minute,
		LoopJoinTokenExpiry: time.Minute, LoopRetention: time.Hour, LoopMeshPeers: 15 * time.Second,
	}
	loops := h.s.Loops()
	if len(loops) != len(want) {
		t.Fatalf("%d loops", len(loops))
	}
	for _, l := range loops {
		if want[l.Name] != l.Interval || l.Run == nil {
			t.Fatalf("loop %s every %v", l.Name, l.Interval)
		}
	}
}

func TestNodeExpiry(t *testing.T) {
	h := newHarness(t)
	for _, n := range []string{"n1", "n2", "n3", "n4", "n5"} {
		if _, err := h.st.InsertNode(bg, n); err != nil {
			t.Fatal(err)
		}
	}
	// n1 joined a minute ago and never reported; n2 reports; n3's last
	// report is a minute old; n4 is removed, n5 down already.
	h.exec("UPDATE nodes SET joined_at = now() - interval '1 minute'")
	h.exec("UPDATE nodes SET status = 'ready' WHERE name IN ('n2', 'n3')")
	h.exec("UPDATE nodes SET status = 'removed', removed_at = now() WHERE name = 'n4'")
	h.exec("UPDATE nodes SET status = 'down', down_since = now() - interval '1 hour', status_description = 'old' WHERE name = 'n5'")
	h.exec("INSERT INTO node_telemetry (node, last_seen) VALUES ('n2', now() + interval '1 hour'), ('n3', now() - interval '1 minute')")

	// Without a leader nothing happens.
	if err := h.s.expireNodes(bg); err != nil {
		t.Fatal(err)
	}
	el := leader.NewElector(h.d, "i1", dbtest.Logger(t))
	h.s.el = el
	h.s.leadingSince = el.LeadingSince
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = el.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, "the leader", 10*time.Second, el.IsLeader)
	statuses := func() map[string]string {
		nodes, err := h.st.ListNodes(bg)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, n := range nodes {
			out[n.Name] = n.Status
		}
		return out
	}

	// A leader that has not led for a node TTL yet does nothing: after an
	// outage of the control plane, nodes get a full TTL to report again.
	h.s.leadingSince = func() time.Time { return time.Now() }
	if err := h.s.expireNodes(bg); err != nil {
		t.Fatal(err)
	}
	if s := statuses(); s["n1"] != model.NodeJoined || s["n3"] != model.NodeReady {
		t.Fatalf("expired while leading for less than the TTL: %v", s)
	}
	// Nor does one whose database failed within the TTL.
	h.s.leadingSince = func() time.Time { return time.Now().Add(-time.Hour) }
	h.s.lastFailure = func() time.Time { return time.Now() }
	if err := h.s.expireNodes(bg); err != nil {
		t.Fatal(err)
	}
	if s := statuses(); s["n1"] != model.NodeJoined || s["n3"] != model.NodeReady {
		t.Fatalf("expired right after a database failure: %v", s)
	}

	h.s.leadingSince = el.LeadingSince
	h.s.lastFailure = func() time.Time { return time.Now().Add(-time.Hour) }
	waitFor(t, "a node TTL of leadership", 5*time.Second, func() bool { return time.Since(el.LeadingSince()) > time.Second })
	ch, unsubscribe := h.d.Subscribe(db.ChanLeader)
	defer unsubscribe()
	if err := h.s.expireNodes(bg); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, ch, hintNodesDown)
	s := statuses()
	if s["n1"] != model.NodeDown || s["n2"] != model.NodeReady || s["n3"] != model.NodeDown ||
		s["n4"] != model.NodeRemoved || s["n5"] != model.NodeDown {
		t.Fatalf("statuses after expiry %v", s)
	}
	if n := h.node("n3"); n.StatusDescription != "heartbeat expired" || n.DownSince.IsZero() {
		t.Fatalf("expired node %+v", n)
	}
	if n := h.node("n5"); n.StatusDescription != "old" {
		t.Fatalf("a down node was expired again %+v", n)
	}

	// Many expired nodes go down in batches, each a fenced transaction of
	// its own, until one comes back short.
	h.s.expiryBatch = 2
	h.exec(`INSERT INTO nodes (name, status, joined_at)
		SELECT 'm' || g, 'ready', now() - interval '1 minute' FROM generate_series(1, 5) g`)
	if err := h.s.expireNodes(bg); err != nil {
		t.Fatal(err)
	}
	if n := h.count("SELECT count(*) FROM nodes WHERE name LIKE 'm%' AND status = 'down'"); n != 5 {
		t.Fatalf("%d of 5 expired nodes down after batches of 2", n)
	}

	// A leader whose lease was taken over in the database, while it still
	// believes it leads (its next renew would notice), changes nothing:
	// the fence in the transaction refuses it, not the elector's own view.
	if !el.IsLeader() {
		t.Fatal("the elector stepped down before the fence check")
	}
	h.exec("UPDATE leases SET holder = 'other', epoch = epoch + 1, expires_at = now() + interval '1 minute' WHERE name = 'leader'")
	h.exec("UPDATE node_telemetry SET last_seen = now() - interval '1 minute' WHERE node = 'n2'")
	if err := h.s.expireNodes(bg); err != nil {
		t.Fatal(err)
	}
	if !el.IsLeader() {
		t.Fatal("the elector stepped down during the fence check: its own view refused the transaction, not the database")
	}
	if s := statuses(); s["n2"] != model.NodeReady {
		t.Fatalf("expired without the lease: %v", s)
	}
}

func TestReconcileSweep(t *testing.T) {
	h := newHarness(t)
	for _, n := range []string{"n1", "n2", "n3", "n4"} {
		h.joined(n)
		h.links.setConnected(n, n != "n2")
	}
	h.exec("UPDATE nodes SET status = 'ready' WHERE name IN ('n1', 'n2')")
	h.exec("UPDATE nodes SET status = 'down' WHERE name = 'n4'")
	if err := h.s.reconcileSweep(bg); err != nil {
		t.Fatal(err)
	}
	h.expectReconcile("n1")
	h.expectNoReconcile()
}

func TestReconcileRequests(t *testing.T) {
	h := newHarness(t)
	h.joined("n1")
	h.joined("n2")
	h.links.setConnected("n1", true)

	// A new connection reconciles; the database spaces the reconciles of
	// a node across instances.
	h.s.OnNodeConn("n1")
	h.expectReconcile("n1")
	h.s.OnNodeConn("n1")
	h.expectNoReconcile()
	h.allowReconcile("n1")
	h.s.OnNodeConn("n1")
	h.expectReconcile("n1")

	// One reconcile of a node at a time on this instance: requests while
	// one runs make one more run after it.
	h.hooks.mu.Lock()
	gate := make(chan struct{})
	h.hooks.gate = gate
	h.hooks.mu.Unlock()
	h.allowReconcile("n1")
	h.s.requestReconcile("n1")
	h.expectReconcile("n1")
	h.allowReconcile("n1")
	h.s.requestReconcile("n1")
	h.s.requestReconcile("n1")
	h.expectNoReconcile()
	close(gate)
	h.expectReconcile("n1")
	h.expectNoReconcile()

	// A node that is down is not reconciled.
	h.exec("UPDATE nodes SET status = 'down', reconcile_requested_at = NULL WHERE name = 'n1'")
	h.s.requestReconcile("n1")
	h.expectNoReconcile()

	// Not connected: ask the others, at most once per 5 s.
	ch, unsubscribe := h.d.Subscribe(db.ChanReconcile)
	defer unsubscribe()
	h.s.requestReconcile("n2")
	expectPayload(t, ch, "n2")
	h.s.requestReconcile("n2")
	expectNoPayload(t, ch, "n2", 300*time.Millisecond)
	h.expectNoReconcile()
}

// runService runs h.s.Run until the test ends.
func runService(t *testing.T, h *harness) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- h.s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
}

// notifyUntil sends payload on channel until cond holds: Run subscribes
// in its own goroutine, and a notification before that is not seen.
func (h *harness) notifyUntil(channel, payload, what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		if err := db.Notify(bg, h.d.Pool, channel, payload); err != nil {
			h.t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestRunFollowsNodeAndReconcileNotifications(t *testing.T) {
	h := newHarness(t)
	h.joined("n1")
	h.joined("n2")
	h.links.setConnected("n1", true)
	// This instance's clock runs an hour ahead of the database's. The
	// rollout deadline nodelink gets still lies the grace the node has
	// left ahead of it: the database's clock decides when the grace ends.
	h.s.now = func() time.Time { return time.Now().Add(time.Hour) }
	graceLeft := func(at time.Time, want time.Duration) bool {
		left := at.Sub(h.s.now())
		return left > want-5*time.Second && left <= want
	}
	runService(t, h)

	// ferun_node: the rollout deadline is reloaded and the connections
	// re-checked.
	h.exec("UPDATE nodes SET stopping_until = now() + interval '30 seconds' WHERE name = 'n1'")
	h.notifyUntil(db.ChanNode, "n1", "the re-check of n1", func() bool {
		rechecks, _, stopping := h.links.snapshot()
		return contains(rechecks, "n1") && graceLeft(stopping["n1"], 30*time.Second)
	})
	h.exec("UPDATE nodes SET stopping_until = NULL WHERE name = 'n1'")
	h.notifyUntil(db.ChanNode, "n1", "the cleared deadline", func() bool {
		_, _, stopping := h.links.snapshot()
		at, ok := stopping["n1"]
		return ok && at.IsZero()
	})

	// ferun_reconcile: an instance connected to the node reconciles it;
	// one that is not ignores it.
	if err := db.Notify(bg, h.d.Pool, db.ChanReconcile, "n1"); err != nil {
		t.Fatal(err)
	}
	h.expectReconcile("n1")
	if err := db.Notify(bg, h.d.Pool, db.ChanReconcile, "n2"); err != nil {
		t.Fatal(err)
	}
	h.expectNoReconcile()

	// The listener reconnects ("" on every channel): every deadline is
	// reloaded, every connection re-checked, every connected node
	// reconciled.
	h.allowReconcile("n1")
	h.exec("UPDATE nodes SET stopping_until = now() + interval '20 seconds' WHERE name = 'n2'")
	h.exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name = 'ferund-listener' AND datname = current_database()`)
	waitFor(t, "RecheckAll", 10*time.Second, func() bool {
		_, all, stopping := h.links.snapshot()
		return all > 0 && graceLeft(stopping["n2"], 20*time.Second)
	})
	h.expectReconcile("n1")
	h.expectNoReconcile()
}

func TestRunStopsBackgroundWork(t *testing.T) {
	h := newHarness(t)
	h.joined("n1")
	h.links.setConnected("n1", true)
	h.hooks.mu.Lock()
	h.hooks.gate = make(chan struct{})
	h.hooks.mu.Unlock()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- h.s.Run(ctx) }()
	h.s.requestReconcile("n1")
	h.expectReconcile("n1")
	// The reconcile in flight is cancelled with Run's context, and Run
	// waits for it.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if h.s.goBackground(func(context.Context) {}) {
		t.Fatal("background work starts after Run ended")
	}
}

func TestRetention(t *testing.T) {
	h := newHarness(t)
	h.s.cfg.ActivityRetention = time.Hour
	serial := h.joined("n1")
	current, old, recent := uuid.New(), uuid.New(), uuid.New()
	h.exec("UPDATE nodes SET start_id = $1 WHERE name = 'n1'", current)
	h.exec(`INSERT INTO node_starts (node, start_id, first_seen, last_event_at) VALUES
		('n1', $1, now() - interval '40 days', now() - interval '31 days'),
		('n1', $2, now() - interval '40 days', now() - interval '31 days'),
		('n1', $3, now() - interval '40 days', now() - interval '1 day')`, current, old, recent)
	h.exec(`INSERT INTO event_gaps (node, start_id, from_seq, to_seq, detected_at) VALUES
		('n1', $1, 1, 2, now() - interval '2 hours'), ('n1', $1, 5, 6, now())`, current)
	h.exec(`INSERT INTO node_certs (serial, node, kind, key_hash, not_before, not_after) VALUES
		('0ld', 'n1', 'join', 'k', now() - interval '200 days', now() - interval '91 days')`)
	h.exec(`INSERT INTO issued_certs (serial, pod_id, incarnation, node, spiffe_id, hpke_public_key, not_after) VALUES
		('p0ld', gen_random_uuid(), gen_random_uuid(), 'n1', 'x', '\x00', now() - interval '91 days'),
		('pnew', gen_random_uuid(), gen_random_uuid(), 'n1', 'x', '\x00', now() - interval '1 day')`)
	if err := h.s.retention(bg); err != nil {
		t.Fatal(err)
	}
	var starts []uuid.UUID
	rows, err := h.d.Pool.Query(bg, "SELECT start_id FROM node_starts ORDER BY first_seen, start_id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		starts = append(starts, id)
	}
	rows.Close()
	if len(starts) != 2 || (starts[0] != current && starts[1] != current) || (starts[0] != recent && starts[1] != recent) {
		t.Fatalf("node starts %v; want the current and the recent one", starts)
	}
	if n := h.count("SELECT count(*) FROM event_gaps"); n != 1 {
		t.Fatalf("%d gaps; want the recent one (retention 1 h)", n)
	}
	if n := h.count("SELECT count(*) FROM node_certs WHERE serial IN ('0ld', $1)", serial); n != 1 {
		t.Fatalf("%d node certs; want the live one", n)
	}
	if n := h.count("SELECT count(*) FROM issued_certs"); n != 1 {
		t.Fatalf("%d pod certs; want the recent one", n)
	}

	// Join tokens used or expired more than 30 days ago go.
	h.token("")
	h.token("")
	h.token("")
	h.exec(`UPDATE join_tokens SET expires_at = now() - interval '31 days'
		WHERE id = (SELECT id FROM join_tokens WHERE used_at IS NULL ORDER BY created_at, id LIMIT 1)`)
	h.exec(`UPDATE join_tokens SET used_at = now() - interval '31 days', used_name = 'x', expires_at = now() + interval '1 hour'
		WHERE id = (SELECT id FROM join_tokens WHERE used_at IS NULL AND expires_at > now() ORDER BY created_at, id LIMIT 1)`)
	before := h.count("SELECT count(*) FROM join_tokens")
	if err := h.s.expireJoinTokens(bg); err != nil {
		t.Fatal(err)
	}
	if after := h.count("SELECT count(*) FROM join_tokens"); after != before-2 {
		t.Fatalf("%d join tokens of %d left; want the two stale ones gone", after, before)
	}
}
