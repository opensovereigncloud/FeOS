// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
)

// The limits of an EventBatch (fleet.proto), and the node's buffer.
const (
	maxBatchEvents = 512
	maxBatchBytes  = 1 << 20
	// defaultEventCapacity is how many unacknowledged events the node
	// keeps; beyond it new events are dropped and counted (ADR 0013).
	defaultEventCapacity = 10000
)

// eventBuffer is the node's bounded buffer of events: dense sequence
// numbers per start of the node, kept until the control plane
// acknowledges them, and sent again after a reconnect to whichever
// instance. What does not fit is dropped and counted; its numbers are
// skipped, so that the control plane sees a gap the count explains.
type eventBuffer struct {
	mu       sync.Mutex
	capacity int
	starts   []*startLog // oldest first; the last is the current start
	held     int         // unacknowledged events, over all starts
	wake     chan struct{}
	acked    chan struct{} // closed and replaced at every acknowledgement
}

// startLog is the events of one start of the node.
type startLog struct {
	id      string
	next    uint64 // the next sequence number
	dropped uint64
	acked   uint64           // the highest number acknowledged
	events  []*fleetv1.Event // unacknowledged, ascending
}

func newEventBuffer(capacity int) *eventBuffer {
	return &eventBuffer{capacity: capacity, wake: make(chan struct{}, 1), acked: make(chan struct{})}
}

// newStart begins the events of a new start. Earlier starts keep what is
// not acknowledged, which goes out first; one with nothing left goes.
func (b *eventBuffer) newStart(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.starts[:0]
	for _, s := range b.starts {
		if len(s.events) > 0 {
			kept = append(kept, s)
		}
	}
	b.starts = append(kept, &startLog{id: id, next: 1})
	b.signal()
}

// add gives body the current start's next number and the time, and keeps
// it, or drops it when the buffer is full. It returns the number.
func (b *eventBuffer) add(ev *fleetv1.Event) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.starts) == 0 {
		return 0
	}
	s := b.starts[len(b.starts)-1]
	ev.Seq = s.next
	s.next++
	if ev.Time == nil {
		ev.Time = timestamppb.Now()
	}
	// An event too large for a batch of its own could never be sent.
	if b.held >= b.capacity || eventSize(ev) > maxBatchBytes-batchOverhead(s) {
		s.dropped++
		return ev.Seq
	}
	s.events = append(s.events, ev)
	b.held++
	b.signal()
	return ev.Seq
}

func (b *eventBuffer) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// eventSize is what an event adds to a batch: the field's tag, its length
// and the message.
func eventSize(ev *fleetv1.Event) int {
	return protowire.SizeTag(2) + protowire.SizeBytes(proto.Size(ev))
}

// batchOverhead is the size of a batch of s without events.
func batchOverhead(s *startLog) int {
	return proto.Size(&fleetv1.EventBatch{StartId: s.id, Dropped: s.dropped})
}

// state is what the buffer holds of a start, for tests and the restart.
func (b *eventBuffer) state(start string) (next, acked, dropped uint64, pending int, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.starts {
		if s.id == start {
			return s.next, s.acked, s.dropped, len(s.events), true
		}
	}
	return 0, 0, 0, 0, false
}

// pending is the number of unacknowledged events over all starts.
func (b *eventBuffer) pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.held
}

// dropped is the number of events the current start dropped.
func (b *eventBuffer) droppedNow() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.starts) == 0 {
		return 0
	}
	return b.starts[len(b.starts)-1].dropped
}

// waitAcked waits until seq of start is acknowledged, or ctx ends.
func (b *eventBuffer) waitAcked(ctx context.Context, start string, seq uint64) bool {
	for {
		b.mu.Lock()
		done := true
		for _, s := range b.starts {
			if s.id == start {
				done = s.acked >= seq
			}
		}
		ch := b.acked
		b.mu.Unlock()
		if done {
			return true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// oldest returns the oldest start, which a stream settles first.
func (b *eventBuffer) oldest() *startLog {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.starts) == 0 {
		return nil
	}
	return b.starts[0]
}

// isCurrent reports whether s is the current start.
func (b *eventBuffer) isCurrent(s *startLog) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.starts) > 0 && b.starts[len(b.starts)-1] == s
}

// retire removes s when it is an earlier start with nothing left to send.
func (b *eventBuffer) retire(s *startLog) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.starts) < 2 || b.starts[len(b.starts)-1] == s || len(s.events) > 0 {
		return false
	}
	for i, x := range b.starts {
		if x == s {
			b.starts = append(b.starts[:i:i], b.starts[i+1:]...)
			return true
		}
	}
	return false
}

// nextBatch returns the next batch of s: its oldest unacknowledged events,
// at most 512 and 1 MiB; nil when s has none.
func (b *eventBuffer) nextBatch(s *startLog) *fleetv1.EventBatch {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(s.events) == 0 {
		return nil
	}
	batch := &fleetv1.EventBatch{StartId: s.id, Dropped: s.dropped}
	size := proto.Size(batch)
	for _, ev := range s.events {
		n := eventSize(ev)
		if len(batch.Events) == maxBatchEvents || size+n > maxBatchBytes {
			break
		}
		batch.Events = append(batch.Events, ev)
		size += n
	}
	return batch
}

// ask is the batch without events that asks where the control plane is
// for s.
func (b *eventBuffer) ask(s *startLog) *fleetv1.EventBatch {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &fleetv1.EventBatch{StartId: s.id, Dropped: s.dropped}
}

// ack applies an acknowledgement for s: every event up to seq is stored.
func (b *eventBuffer) ack(s *startLog, seq uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if seq > s.acked {
		s.acked = seq
	}
	i := 0
	for i < len(s.events) && s.events[i].Seq <= seq {
		i++
	}
	if i > 0 {
		s.events = append(s.events[:0:0], s.events[i:]...)
		b.held -= i
	}
	close(b.acked)
	b.acked = make(chan struct{})
}

// publish runs the ack protocol of fleet.proto on one PublishEvents
// stream until ctx ends (nil) or the stream fails (its error). For every
// start, oldest first, it asks where the control plane is with a batch
// without events, then sends what follows, one batch at a time, each
// after the acknowledgement of the one before. An earlier start that has
// nothing left is retired; at the current start it waits for new events.
// The acknowledgements are read by a goroutine of their own, so that a
// stream the control plane ends is noticed while the node waits.
func (b *eventBuffer) publish(ctx context.Context, client fleetv1.FleetServiceClient) error {
	sctx, cancel := context.WithCancel(ctx)
	stream, err := client.PublishEvents(sctx)
	if err != nil {
		cancel()
		return err
	}
	acks := make(chan *fleetv1.EventAck, 1)
	ended := make(chan error, 1)
	reader := make(chan struct{})
	go func() {
		defer close(reader)
		for {
			ack, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					err = errors.New("the control plane ended the events stream")
				}
				ended <- err
				return
			}
			select {
			case acks <- ack:
			case <-sctx.Done():
				return
			}
		}
	}()
	defer func() {
		cancel()
		<-reader
	}()

	asked := map[*startLog]bool{}
	exchange := func(s *startLog, batch *fleetv1.EventBatch) error {
		// io.EOF: the stream ended; its status comes to the reader.
		if err := stream.Send(batch); err != nil && err != io.EOF {
			return err
		}
		select {
		case ack := <-acks:
			if ack.GetStartId() != s.id {
				return fmt.Errorf("acknowledgement for start %q, not %q", ack.GetStartId(), s.id)
			}
			b.ack(s, ack.GetSeq())
			return nil
		case err := <-ended:
			return err
		case <-ctx.Done():
			return nil
		}
	}
	for {
		if ctx.Err() != nil {
			_ = stream.CloseSend()
			return nil
		}
		s := b.oldest()
		if s == nil {
			return nil
		}
		if !asked[s] {
			if err := exchange(s, b.ask(s)); err != nil {
				return err
			}
			asked[s] = true
			continue
		}
		if batch := b.nextBatch(s); batch != nil {
			if err := exchange(s, batch); err != nil {
				return err
			}
			continue
		}
		if b.retire(s) || !b.isCurrent(s) {
			continue
		}
		select {
		case <-b.wake:
		case err := <-ended:
			return err
		case <-ctx.Done():
		}
	}
}
