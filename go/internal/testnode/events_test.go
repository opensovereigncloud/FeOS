// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
)

func audit(kind string) *fleetv1.Event {
	return &fleetv1.Event{Body: &fleetv1.Event_Audit{Audit: &fleetv1.AuditRecord{PodId: uuid.NewString(), Kind: kind, DetailJson: `{"a":1}`}}}
}

// The buffer numbers densely, batches at most 512 events and 1 MiB,
// counts what does not fit, and forgets what is acknowledged.
func TestEventBuffer(t *testing.T) {
	b := newEventBuffer(2000)
	b.newStart("s1")
	for range 1200 {
		b.add(audit("kube_request"))
	}
	s := b.oldest()
	var sizes []int
	var next uint64 = 1
	for {
		batch := b.nextBatch(s)
		if batch == nil {
			break
		}
		for _, ev := range batch.Events {
			if ev.Seq != next || ev.Time == nil {
				t.Fatalf("seq %d, want %d", ev.Seq, next)
			}
			next++
		}
		if proto.Size(batch) > maxBatchBytes {
			t.Fatalf("batch of %d bytes", proto.Size(batch))
		}
		sizes = append(sizes, len(batch.Events))
		b.ack(s, batch.Events[len(batch.Events)-1].Seq)
	}
	if len(sizes) != 3 || sizes[0] != 512 || sizes[1] != 512 || sizes[2] != 176 || b.pending() != 0 {
		t.Fatalf("batches %v, pending %d", sizes, b.pending())
	}

	// Large events: the byte limit cuts first.
	big := strings.Repeat("x", 300<<10)
	for range 4 {
		ev := audit("tls_connection")
		ev.GetAudit().DetailJson = big
		b.add(ev)
	}
	if batch := b.nextBatch(s); len(batch.Events) != 3 || proto.Size(batch) > maxBatchBytes {
		t.Fatalf("a batch of %d large events, %d bytes", len(batch.Events), proto.Size(batch))
	}
	// One that could never be sent is dropped.
	huge := audit("x")
	huge.GetAudit().DetailJson = strings.Repeat("y", maxBatchBytes)
	seq := b.add(huge)
	if next, _, dropped, pending, _ := b.state("s1"); dropped != 1 || pending != 4 || next != seq+1 {
		t.Fatalf("next %d, dropped %d, pending %d", next, dropped, pending)
	}

	// Full: dropped and counted, the numbers skipped.
	small := newEventBuffer(3)
	small.newStart("s")
	for range 5 {
		small.add(audit("k"))
	}
	st := small.oldest()
	batch := small.nextBatch(st)
	if len(batch.Events) != 3 || batch.Dropped != 2 || batch.Events[2].Seq != 3 {
		t.Fatalf("full buffer: %v", batch)
	}
	if seq := small.add(audit("k")); seq != 6 {
		t.Fatalf("next seq %d", seq)
	}

	// An earlier start with nothing left is retired; the current never is.
	small.ack(st, 3)
	small.newStart("t")
	if small.oldest().id != "t" {
		t.Fatalf("an empty earlier start kept")
	}
	cur := small.oldest()
	small.add(audit("k"))
	small.newStart("u")
	if small.oldest() != cur || small.retire(cur) {
		t.Fatal("an earlier start with events retired")
	}
	small.ack(cur, 1)
	if !small.retire(cur) || small.oldest().id != "u" || small.retire(small.oldest()) {
		t.Fatal("retire")
	}
}

// Every event arrives once, in order, with dense numbers from 1; the
// start's first says the node started.
func TestEventsDelivered(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	pc := pods(t, in, "n1")
	uid, _ := createRunning(t, pc)
	n.Publish(audit("kube_request"))
	n.Publish(&fleetv1.Event{}) // no known body
	eventually(t, 5*time.Second, "events stored", func() bool { return len(e.f.storedEvents()) == 5 })
	got := e.f.storedEvents()
	for i, s := range got {
		if s.start != n.StartID() || s.ev.Seq != uint64(i+1) {
			t.Fatalf("event %d: start %s seq %d", i, s.start, s.ev.Seq)
		}
	}
	if k := got[0].ev.GetNode().GetKind(); k != "started" {
		t.Fatalf("first event %v", got[0].ev)
	}
	if p := got[1].ev.GetPod(); p.GetPodId() != uid.String() || p.State != podv1.PodState_POD_STATE_PENDING {
		t.Fatalf("second event %v", got[1].ev)
	}
	if p := got[2].ev.GetPod(); p.State != podv1.PodState_POD_STATE_RUNNING {
		t.Fatalf("third event %v", got[2].ev)
	}
	if got[3].ev.GetAudit() == nil || got[4].ev.Body != nil {
		t.Fatalf("published events %v %v", got[3].ev, got[4].ev)
	}
	eventually(t, 5*time.Second, "all acknowledged", func() bool { return n.EventsPending() == 0 })
}

// A batch the control plane could not store is sent again, on the next
// stream, after asking where the control plane is; nothing is lost or
// stored twice.
func TestEventsResentAfterFailure(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	eventually(t, 5*time.Second, "started stored", func() bool { return len(e.f.storedEvents()) == 1 })
	e.f.mu.Lock()
	e.f.failStream = 2
	e.f.mu.Unlock()
	for range 3 {
		n.Publish(audit("ssh_signature"))
	}
	eventually(t, 10*time.Second, "events stored", func() bool { return len(e.f.storedEvents()) == 4 })
	for i, s := range e.f.storedEvents() {
		if s.ev.Seq != uint64(i+1) {
			t.Fatalf("stored %d as %d", s.ev.Seq, i+1)
		}
	}
	// Every stream began by asking.
	asks := 0
	for _, b := range e.f.batchesSeen() {
		if b.events == 0 {
			asks++
		}
	}
	if asks < 3 {
		t.Fatalf("%d asks over three streams", asks)
	}
	never(t, 500*time.Millisecond, "stored twice", func() bool { return len(e.f.storedEvents()) != 4 })
}

// What a start did not send goes first after a restart, then the new
// start's events; batches stay within the limits.
func TestEventsAcrossRestart(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	first := n.StartID()
	eventually(t, 5*time.Second, "started stored", func() bool { return len(e.f.storedEvents()) == 1 })
	n.Partition(true)
	eventually(t, 5*time.Second, "disconnected", func() bool { return !in.srv.Connected("n1") })
	for range 1300 {
		n.Publish(audit("kube_request"))
	}
	n.Restart(false) // partitioned: it restarts but does not dial
	second := n.StartID()
	if second == first {
		t.Fatal("same start ID")
	}
	n.Publish(audit("kube_deny"))
	never(t, 300*time.Millisecond, "dialled while partitioned", func() bool { return in.srv.Connected("n1") })
	n.Partition(false)
	eventually(t, 10*time.Second, "all stored", func() bool { return len(e.f.storedEvents()) == 1+1300+2 })
	got := e.f.storedEvents()
	for i, s := range got[:1301] {
		if s.start != first || s.ev.Seq != uint64(i+1) {
			t.Fatalf("event %d of the first start: %s %d", i, s.start, s.ev.Seq)
		}
	}
	if s := got[1301]; s.start != second || s.ev.Seq != 1 || s.ev.GetNode().GetKind() != "started" {
		t.Fatalf("the second start's first: %v", s)
	}
	if s := got[1302]; s.start != second || s.ev.Seq != 2 || s.ev.GetAudit().GetKind() != "kube_deny" {
		t.Fatalf("the second start's second: %v", s)
	}
	for _, b := range e.f.batchesSeen() {
		if b.events > maxBatchEvents || b.size > maxBatchBytes {
			t.Fatalf("batch of %d events, %d bytes", b.events, b.size)
		}
	}
	eventually(t, 5*time.Second, "telemetry of the new start", func() bool {
		r, ok := e.f.lastReport()
		return ok && r.t.StartId == second
	})
}

// Events that do not fit are counted in every batch of their start, and
// their numbers skipped.
func TestEventsDroppedAreCounted(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	cfg := e.config("n1", in)
	n := e.newNode(cfg)
	n.events.capacity = 5 // "started" and four more
	for range 10 {
		n.Publish(audit("kube_request"))
	}
	if n.EventsDropped() != 6 {
		t.Fatalf("dropped %d", n.EventsDropped())
	}
	e.run(n)
	eventually(t, 10*time.Second, "stored", func() bool { return len(e.f.storedEvents()) == 5 })
	// Room again once the node has the acknowledgement.
	eventually(t, 5*time.Second, "acknowledged", func() bool { return n.EventsPending() == 0 })
	if seq := n.Publish(audit("kube_request")); seq != 12 {
		t.Fatalf("next seq %d", seq)
	}
	eventually(t, 5*time.Second, "stored after the gap", func() bool { return len(e.f.storedEvents()) == 6 })
	if s := e.f.storedEvents()[5]; s.ev.Seq != 12 {
		t.Fatalf("after the gap: %d", s.ev.Seq)
	}
	for _, b := range e.f.batchesSeen() {
		if b.dropped != 6 {
			t.Fatalf("a batch counts %d dropped", b.dropped)
		}
	}
}
