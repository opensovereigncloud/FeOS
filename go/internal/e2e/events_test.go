// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// auditEvent is an event with an audit record of kind for pod.
func auditEvent(seq uint64, pod uuid.UUID, kind string) *fleetv1.Event {
	return &fleetv1.Event{Seq: seq, Time: timestamppb.Now(), Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{
		PodId: pod.String(), Kind: kind, DetailJson: `{"n":` + strconv.FormatUint(seq, 10) + `}`}}}
}

// batch is an event batch of start with audit records numbered seqs.
func batch(start uuid.UUID, dropped uint64, kind string, seqs ...uint64) *fleetv1.EventBatch {
	b := &fleetv1.EventBatch{StartId: start.String(), Dropped: dropped}
	for _, s := range seqs {
		b.Events = append(b.Events, auditEvent(s, uuid.Nil, kind))
	}
	return b
}

// publish sends one batch on a new events stream to in, and returns the
// acknowledgement or the stream's error.
func (c *cluster) publish(in *instance, id identity, b *fleetv1.EventBatch) (*fleetv1.EventAck, error) {
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	stream, err := c.rawUplink(in.NodeAddr(), id).PublishEvents(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(b); err != nil {
		return nil, err
	}
	ack, err := stream.Recv()
	_ = stream.CloseSend()
	return ack, err
}

// startEvents is how many events of a node's start are stored, under how
// many distinct numbers, and the highest.
func (c *cluster) startEvents(node string, start uuid.UUID) (rows, distinct, highest int) {
	c.t.Helper()
	err := c.db.Pool.QueryRow(bg, `SELECT count(*), count(DISTINCT seq), coalesce(max(seq), 0) FROM activity
		WHERE node = $1 AND start_id = $2 AND seq IS NOT NULL`, node, start).Scan(&rows, &distinct, &highest)
	if err != nil {
		c.t.Fatal(err)
	}
	return rows, distinct, highest
}

// Scenario 8: events are stored once across a reconnect to another
// instance, and the numbers the node neither sent nor counted as dropped
// are recorded as a gap; a test node's events all arrive once while its
// uplink moves off a stopping instance.
func TestEventsAcrossInstances(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	a := c.start("a")
	b := c.start("b")

	// The test speaks for a node: the batches as a node resends them.
	id := c.joinIdentity(a.NodeAddr(), c.joinToken("ev1"), "ev1")
	start := uuid.New()
	for _, step := range []struct {
		in   *instance
		b    *fleetv1.EventBatch
		want uint64
	}{
		{a, batch(start, 0, "e2e_audit", 1, 2, 3), 3},
		// The acknowledgement was lost; the node resends on another
		// instance.
		{b, batch(start, 0, "e2e_audit", 2, 3, 4, 5), 5},
		// Two dropped by the node, one never sent: a gap of one.
		{a, batch(start, 2, "e2e_audit", 9, 10), 10},
		// Asking where the control plane is.
		{b, batch(start, 2, "e2e_audit"), 10},
	} {
		ack, err := c.publish(step.in, id, step.b)
		if err != nil || ack.GetStartId() != start.String() || ack.GetSeq() != step.want {
			t.Fatalf("batch on %s: ack %v %v, want %d", step.in.name, ack, err, step.want)
		}
	}
	if rows, distinct, highest := c.startEvents("ev1", start); rows != 7 || distinct != 7 || highest != 10 {
		t.Fatalf("%d rows, %d distinct, highest %d", rows, distinct, highest)
	}
	var from, to int64
	if err := c.db.Pool.QueryRow(bg, "SELECT from_seq, to_seq FROM event_gaps WHERE node = 'ev1' AND start_id = $1",
		start).Scan(&from, &to); err != nil || from != 8 || to != 8 {
		t.Fatalf("gap [%d, %d] %v", from, to, err)
	}
	if n := c.count("SELECT count(*) FROM node_starts WHERE node = 'ev1' AND start_id = $1 AND last_seq = 10 AND dropped = 2",
		start); n != 1 {
		t.Fatal("node_starts does not hold the start's last number and its dropped count")
	}

	// A test node's events while the instance its uplink uses stops.
	n1 := c.join("n1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 300 {
			n1.Publish(auditEvent(0, uuid.Nil, "e2e_move"))
			if i%10 == 0 {
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)
	var used *instance
	for _, in := range []*instance{a, b} {
		if in.NodeAddr() == n1.UplinkAddr() {
			used = in
		}
	}
	if used == nil {
		t.Fatalf("the uplink %q is on no instance", n1.UplinkAddr())
	}
	stopped := used.stopAsync()
	<-done
	eventually(t, 60*time.Second, "every event acknowledged", func() bool { return n1.EventsPending() == 0 })
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	startID := uuid.MustParse(n1.StartID())
	rows, distinct, highest := c.startEvents("n1", startID)
	if rows != distinct || rows != highest || c.count("SELECT count(*) FROM activity WHERE node = 'n1' AND kind = 'e2e_move'") != 300 {
		t.Fatalf("%d rows, %d distinct, highest %d", rows, distinct, highest)
	}
	if n := c.count("SELECT count(*) FROM event_gaps WHERE node = 'n1'"); n != 0 {
		t.Fatalf("%d gaps", n)
	}
}

// Scenario 8: a poisoned event (NUL bytes, a detail that is no JSON) and
// an event without a known body are stored, and the stream goes on.
func TestPoisonedEvents(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	c.start("a")
	n1 := c.join("n1")
	sb := c.create(nil)
	c.run(nil, sb, "true")

	n1.Publish(&fleetv1.Event{Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{PodId: sb.id.String(),
		Kind: "e2e\x00poison", DetailJson: "{\"a\":\"b\x00\", bad"}}})
	n1.Publish(&fleetv1.Event{})
	n1.Publish(&fleetv1.Event{Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{PodId: sb.id.String(),
		Kind: "e2e_after", DetailJson: `{"ok":true}`}}})
	eventually(t, 30*time.Second, "every event acknowledged", func() bool { return n1.EventsPending() == 0 })

	type row struct {
		kind, name, source string
		detail             map[string]any
		sandbox            *podid.ID
	}
	rows, err := c.db.Pool.Query(bg, `SELECT kind, name, source, detail, sandbox_id FROM activity
		WHERE node = 'n1' AND start_id = $1 AND kind IN ('e2epoison', 'node_event', 'e2e_after') ORDER BY seq`,
		uuid.MustParse(n1.StartID()))
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var r row
		var detail []byte
		if err := rows.Scan(&r.kind, &r.name, &r.source, &detail, &r.sandbox); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(detail, &r.detail); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("%d rows: %+v", len(got), got)
	}
	if p := got[0]; p.source != "authn" || p.detail["raw"] == nil || p.sandbox == nil || *p.sandbox != sb.id {
		t.Errorf("the poisoned record %+v", p)
	}
	if u := got[1]; u.kind != "node_event" || u.name != "unknown event" || u.detail["unknown"] != true || u.sandbox != nil {
		t.Errorf("the record without a body %+v", u)
	}
	if after := got[2]; after.kind != "e2e_after" || after.detail["ok"] != true {
		t.Errorf("the record after them %+v", after)
	}
}

// Scenario 8: a storage failure ends the events stream with UNAVAILABLE
// and acknowledges nothing; once storage works, the resent batch is
// stored.
func TestEventStorageFailure(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	a := c.start("a")
	id := c.joinIdentity(a.NodeAddr(), c.joinToken("ev1"), "ev1")
	start := uuid.New()
	b := batch(start, 0, "e2e_ok", 1, 2)
	b.Events = append(b.Events, auditEvent(3, uuid.Nil, "e2e_refused"))

	c.sql("ALTER TABLE activity ADD CONSTRAINT e2e_refuse CHECK (kind <> 'e2e_refused')")
	ack, err := c.publish(a, id, b)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("the batch storage refused: ack %v, %v", ack, err)
	}
	if rows, _, _ := c.startEvents("ev1", start); rows != 0 {
		t.Fatalf("%d rows stored of a failed batch", rows)
	}
	if n := c.count("SELECT count(*) FROM node_starts WHERE node = 'ev1' AND last_seq > 0"); n != 0 {
		t.Fatal("the failed batch moved the start's last number")
	}

	c.sql("ALTER TABLE activity DROP CONSTRAINT e2e_refuse")
	ack, err = c.publish(a, id, b)
	if err != nil || ack.GetSeq() != 3 {
		t.Fatalf("the resent batch: %v %v", ack, err)
	}
	if rows, distinct, _ := c.startEvents("ev1", start); rows != 3 || distinct != 3 {
		t.Fatalf("%d rows, %d distinct", rows, distinct)
	}
}
