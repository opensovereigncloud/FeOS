// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	watchv1 "github.com/gardenlinux/feos/go/gen/feos/watch/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// ring is an owned watch's bounded replay buffer. Its ids are dense and
// grow by one per event from epoch<<32, the owner's epoch, so they keep
// increasing across owners; nothing else assigns ids.
type ring struct {
	buf   []event
	next  int    // where the next event goes
	count int    // how many of buf are live
	last  uint64 // the id of the newest event; epoch<<32 before the first
}

func newRing(size int, epoch int64) ring {
	return ring{buf: make([]event, size), last: uint64(epoch) << 32}
}

// push numbers one event and stores it, dropping the oldest when full.
func (r *ring) push(ev event) {
	r.last++
	ev.ID = r.last
	r.buf[r.next] = ev
	r.next = (r.next + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
}

// since returns the events after cursor; stale when the ring no longer
// holds them all. A cursor at or past the head is caught up, also one
// that was never issued.
func (r *ring) since(cursor uint64) (evs []event, stale bool) {
	if cursor >= r.last {
		return nil, false
	}
	if cursor < r.last-uint64(r.count) {
		return nil, true
	}
	n := int(r.last - cursor)
	out := make([]event, 0, n)
	size := len(r.buf)
	start := ((r.next-n)%size + size) % size
	for i := range n {
		out = append(out, r.buf[(start+i)%size])
	}
	return out, false
}

// ackFrame is a guest's ack with the ring's head when the reader read it:
// every event the guest sent before the ack is numbered by then.
type ackFrame struct {
	cursor uint64
	ack    *watchv1.Ack
}

// link is one guest WatchService/Watch stream of an owned watch.
type link struct {
	node   string
	opened time.Time
	stream watchv1.WatchService_WatchClient
	cancel context.CancelFunc
	acks   chan ackFrame
	// closed is closed once the reader has seen the stream end; err is
	// why, set before.
	closed chan struct{}
	err    error
}

func (l *link) ended() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

// outcome is how a placing went, which tells the supervisor what to wait
// for before it tries again.
type outcome int

const (
	// outDone: placed and acknowledged, or nothing to do.
	outDone outcome = iota
	// outAsleep: the sandbox does not run; it is tried again when the
	// sandbox runs (ferun_sandbox).
	outAsleep
	// outNoNode: the sandbox's node has no connection here; tried again
	// when it has (a ConnEvent).
	outNoNode
	// outRetry: anything else; tried again with backoff.
	outRetry
	// outLater: a lost stream whose backoff has not passed yet; the
	// supervisor's timer opens it.
	outLater
)

// owned is a watch this instance owns in one epoch: its guest stream, its
// ring, and its readers' wake-ups. It ends when the watch is deleted or
// expires, or another instance takes it over (moved); a new epoch on this
// instance is a new owned.
type owned struct {
	s         *Service
	id        uuid.UUID
	sandbox   podid.ID
	container string
	epoch     int64

	// subMu serialises the placing of path sets: the guest answers each
	// subscribe with exactly one ack, in order, so one placing at a time
	// makes the ack a placing waits for its own. It guards gen and the
	// answer stored last.
	subMu sync.Mutex
	// gen is the generation of the set placed last (0: none yet).
	gen int64
	// storedGen and stored are the answer stored last, which is not
	// stored again: its ferun_watch would wake this owner for nothing.
	storedGen int64
	stored    []byte

	mu     sync.Mutex
	paths  []string // the set placed last: what a stale event names
	ring   ring
	wake   chan struct{} // closed and replaced at every push and at the end
	link   *link
	closed bool
	moved  bool // ended because another instance took the watch over
	done   chan struct{}
	// stood: the stream lost last had stood for the longest backoff, so
	// the backoff starts over.
	stood bool

	// kick asks the supervisor to look at the row now; relink also to
	// open a lost stream at once (its node connected, its sandbox runs
	// again); lost tells it the guest stream ended. started makes start
	// idempotent.
	kick    chan struct{}
	relink  chan struct{}
	lost    chan struct{}
	started sync.Once
}

func (s *Service) newOwned(row store.Watch) *owned {
	return &owned{
		s: s, id: row.ID, sandbox: row.SandboxID, container: row.Container, epoch: row.Epoch,
		paths:  append([]string{}, row.Paths...),
		ring:   newRing(s.t.ringSize, row.Epoch),
		wake:   make(chan struct{}),
		done:   make(chan struct{}),
		kick:   make(chan struct{}, 1),
		relink: make(chan struct{}, 1),
		lost:   make(chan struct{}, 1),
	}
}

func (o *owned) notifyLocked() {
	close(o.wake)
	o.wake = make(chan struct{})
}

// push numbers events into the ring and wakes the readers.
func (o *owned) push(evs ...event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pushLocked(evs...)
}

func (o *owned) pushLocked(evs ...event) {
	for _, ev := range evs {
		o.ring.push(ev)
	}
	o.notifyLocked()
}

// cursor is the ring's head.
func (o *owned) cursor() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ring.last
}

// since reads the events after cursor, with the wake channel captured
// under the same lock (an event pushed between the read and the wait
// still wakes the reader). A cursor of an older epoch is stale: the
// events between it and this epoch are gone with the old owner. So is
// one of a later epoch, which no owner issued: within the epoch the ids
// would never reach it. Cursor 0 (a client without one) is the start of
// the first epoch.
func (o *owned) since(cursor uint64) (evs []event, stale bool, wake <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if cursor == 0 {
		cursor = 1 << 32
	}
	if cursor>>32 != uint64(o.epoch) {
		return nil, true, o.wake
	}
	evs, stale = o.ring.since(cursor)
	return evs, stale, o.wake
}

// staleEvent answers one reader whose cursor the ring does not cover: the
// whole set, to re-read. It is not pushed (the other readers are fine);
// its id is the head, so a reader that reconnects right after needs no
// replay.
func (o *owned) staleEvent() event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return event{ID: o.ring.last, Kind: kindStale, StalePaths: append([]string{}, o.paths...)}
}

func (o *owned) isClosed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}

// liveLink is the current guest stream, if it stands.
func (o *owned) liveLink() *link {
	o.mu.Lock()
	l := o.link
	o.mu.Unlock()
	if l == nil || l.ended() {
		return nil
	}
	return l
}

// end lets go of the watch: the guest stream ends, the readers end (with
// an abort when moved, so that their clients reconnect with their cursor),
// and the instance forgets it.
func (o *owned) end(moved bool) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed, o.moved = true, moved
	l := o.link
	o.link = nil
	close(o.done)
	o.notifyLocked()
	o.mu.Unlock()
	if l != nil {
		l.cancel()
	}
	o.s.forget(o)
}

// start runs the supervisor, once; the creator of an owned starts it
// after its first placing, so that the two do not place the same set.
func (o *owned) start() {
	o.started.Do(func() { go o.supervise() })
}

func (o *owned) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// after hands a placing's outcome to the supervisor: a placing that
// should be tried again is.
func (o *owned) after(out outcome) {
	if out == outRetry {
		o.signal(o.lost)
	}
}

// supervise keeps the watch placed. On a kick it reads the row and acts
// (a new set, a lost ownership, a deleted watch); a lost stream is opened
// again after a backoff of 1 s doubling to 30 s, for as long as the
// sandbox runs and its node is connected, and at once on a relink (the
// node connected, the sandbox runs again). The backoff starts over only
// after a stream that stood for the longest wait: a stream that keeps
// ending right after its ack is opened again less and less often, since
// every opening costs the readers a stale event and a re-read of the set.
// A lost stream is seen before a kick that comes with it, so that the
// kick of the owner's own answer does not open the stream before its
// backoff.
func (o *owned) supervise() {
	var backoff time.Duration // the wait before the last opening; 0: none yet
	var due time.Time         // a lost stream is not opened again before
	timer := time.NewTimer(o.s.t.relinkMax)
	timer.Stop()
	defer timer.Stop()
	wait := func() {
		backoff = min(max(2*backoff, o.s.t.relinkMin), o.s.t.relinkMax)
		due = time.Now().Add(backoff)
		timer.Reset(backoff)
	}
	// restart starts the backoff over when the stream lost last had stood.
	restart := func() {
		o.mu.Lock()
		stood := o.stood
		o.stood = false
		o.mu.Unlock()
		if stood {
			backoff = 0
		}
	}
	pendingLost := func() bool {
		select {
		case <-o.lost:
			return true
		default:
			return false
		}
	}
	for {
		relink := false
		select {
		case <-o.done:
			return
		case <-o.s.base.Done():
			return
		case <-o.lost:
			restart()
			wait()
			continue
		case <-o.relink:
			relink = true
		case <-o.kick:
		case <-timer.C:
		}
		if pendingLost() {
			restart()
			wait()
		}
		if relink {
			backoff, due = 0, time.Time{}
			timer.Stop()
		}
		switch o.reconcile(!time.Now().Before(due)) {
		case outRetry:
			// A stream the placing lost is the same failure: one wait.
			pendingLost()
			restart()
			wait()
		case outLater:
		default:
			timer.Stop()
		}
	}
}

// reconcile brings the owned watch in line with its row: gone, it ends;
// owned by another instance or in another epoch, it ends as moved; with a
// newer set, the set is placed; without a stream, the set is placed again
// when reopen allows it (its backoff has passed), else outLater.
func (o *owned) reconcile(reopen bool) outcome {
	ctx, cancel := context.WithTimeout(o.s.base, o.s.t.ackTimeout+o.s.t.rowTimeout)
	defer cancel()
	row, err := o.s.st.GetWatch(ctx, o.id)
	switch {
	case errors.Is(err, store.ErrNoRow):
		o.end(false)
		return outDone
	case err != nil:
		if o.s.base.Err() == nil {
			o.s.rl.warn("row", "watch: cannot read a watch's row", "watch", o.id, "err", err)
		}
		return outRetry
	case row.Owner != o.s.id || row.Epoch != o.epoch:
		o.end(true)
		return outDone
	}
	o.subMu.Lock()
	defer o.subMu.Unlock()
	if o.isClosed() {
		return outDone
	}
	paths, gen := row.Paths, row.Gen
	if gen <= o.gen {
		if o.liveLink() != nil {
			return outDone
		}
		o.mu.Lock()
		paths = append([]string{}, o.paths...)
		o.mu.Unlock()
		if len(paths) == 0 {
			// Nothing to watch: no stream needed until a set arrives.
			return outDone
		}
		if !reopen {
			return outLater
		}
		gen = o.gen
	}
	_, out, err := o.place(ctx, paths, gen)
	if err != nil {
		return outRetry
	}
	return out
}

// The errors of a placing a request answers.
var (
	errNoWatch   = apierr.New(http.StatusNotFound, "no such watch")
	errCancelled = apierr.New(http.StatusRequestTimeout, "the request was cancelled")
)

func (s *Service) errAckTimeout() error {
	return apierr.New(http.StatusGatewayTimeout, "the sandbox did not acknowledge the path set within %s", s.t.ackTimeout)
}

// place puts the set of generation gen on the guest, opening a guest
// stream when none stands, waits for the ack, and stores the answer in
// the row for the PUTs other instances wait on. A set that cannot be
// placed is kept, and answered with every path failed and the reason.
// When a set was placed before and its stream was lost, the ack is
// followed by a stale event: the time without a stream was not watched.
// A placing that gives up on its ack drops the stream: the guest answers
// every subscribe in order, and the late ack would be taken as the answer
// to the next set. The caller holds subMu.
func (o *owned) place(ctx context.Context, paths []string, gen int64) (response, outcome, error) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return response{}, outDone, errNoWatch
	}
	o.paths = append([]string{}, paths...)
	o.mu.Unlock()
	relink := o.gen > 0 && o.liveLink() == nil && len(paths) > 0
	o.gen = gen

	l, out, reason, err := o.openLink(ctx)
	if err != nil {
		return response{}, outRetry, err
	}
	if l == nil {
		res := notPlaced(o.id, o.cursor(), paths, reason)
		o.store(gen, res)
		return res, out, nil
	}
	// A failed send means the stream ended; its reader sees the end, says
	// why and detaches it.
	_ = l.stream.Send(&watchv1.WatchRequest{Frame: &watchv1.WatchRequest_Subscribe{
		Subscribe: &watchv1.Subscribe{Paths: paths}}})
	timer := time.NewTimer(o.s.t.ackTimeout)
	defer timer.Stop()
	select {
	case a := <-l.acks:
		res := ackResponse(o.id, a.cursor, a.ack)
		o.store(gen, res)
		if relink {
			o.push(event{Kind: kindStale, StalePaths: append([]string{}, paths...)})
		}
		return res, outDone, nil
	case <-l.closed:
		reason, out := endReason(l.err)
		res := notPlaced(o.id, o.cursor(), paths, reason)
		o.store(gen, res)
		return res, out, nil
	case <-ctx.Done():
		o.abandon(l)
		return response{}, outRetry, errCancelled
	case <-timer.C:
		o.abandon(l)
		return response{}, outRetry, o.s.errAckTimeout()
	}
}

// endReason reads why a guest stream ended: a pod that is not running on
// its node (as the row thought) is asleep; anything else is a stream to
// open again.
func endReason(err error) (string, outcome) {
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.NotFound:
		return reasonAsleep, outAsleep
	}
	return reasonEnded, outRetry
}

// store writes the answer to the set of generation gen into the row,
// while this instance owns the watch in its epoch, unless it is the
// answer stored last: an owner that places again and again what it
// cannot place (a sandbox that does not run) writes once. The caller
// holds subMu.
func (o *owned) store(gen int64, res response) {
	b, err := json.Marshal(res)
	if err != nil || (gen == o.storedGen && bytes.Equal(b, o.stored)) {
		return
	}
	ctx, cancel := context.WithTimeout(o.s.base, o.s.t.rowTimeout)
	defer cancel()
	if _, err := o.s.st.SetWatchResult(ctx, o.id, o.s.id, o.epoch, gen, b); err != nil {
		if o.s.base.Err() == nil {
			o.s.rl.warn("row", "watch: cannot store an answer", "watch", o.id, "err", err)
		}
		return
	}
	o.storedGen, o.stored = gen, b
}

// openLink returns the standing guest stream, or opens one: only for a
// sandbox that runs (a watch never wakes one), on a node connected to
// this instance, within the cap of streams per sandbox. Without a stream
// it returns the reason for the answer's paths and the outcome. The
// stream lives until the watch ends or the stream fails, not as long as
// ctx.
func (o *owned) openLink(ctx context.Context) (*link, outcome, string, error) {
	if l := o.liveLink(); l != nil {
		return l, outDone, "", nil
	}
	state, node, err := o.s.sb.State(ctx, o.sandbox)
	switch {
	case errors.Is(err, model.ErrNotFound):
		return nil, outAsleep, reasonAsleep, nil
	case err != nil:
		if ctx.Err() != nil {
			return nil, outRetry, "", errCancelled
		}
		return nil, outRetry, "", apierr.New(http.StatusBadGateway, "watch: %v", err)
	case state != model.StateRunning || node == "":
		return nil, outAsleep, reasonAsleep, nil
	case !o.s.conns.Connected(node):
		return nil, outNoNode, reasonNoNode, nil
	}
	if !o.s.takeLinkSlot(o.sandbox) {
		return nil, outRetry, reasonTooMany, nil
	}
	cc, err := o.s.conns.Conn(ctx, node)
	if err != nil {
		o.s.releaseLinkSlot(o.sandbox)
		return nil, outNoNode, reasonNoNode, nil
	}
	sctx, cancel := context.WithCancel(o.s.base)
	stream, err := watchv1.NewWatchServiceClient(cc).Watch(nodelink.PodContext(sctx, o.sandbox))
	if err != nil {
		cancel()
		o.s.releaseLinkSlot(o.sandbox)
		reason, out := endReason(err)
		return nil, out, reason, nil
	}
	// An error here ends the stream; the reader reports it.
	_ = stream.Send(&watchv1.WatchRequest{Frame: &watchv1.WatchRequest_Open{Open: &watchv1.Open{Container: o.container}}})
	l := &link{node: node, opened: time.Now(), stream: stream, cancel: cancel, acks: make(chan ackFrame, 1),
		closed: make(chan struct{})}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		cancel()
		o.s.releaseLinkSlot(o.sandbox)
		return nil, outDone, "", errNoWatch
	}
	o.link = l
	o.mu.Unlock()
	go o.read(l)
	return l, outDone, "", nil
}

// read is the one reader of a guest stream: it numbers the events into
// the ring and hands the acks, with the ring's head, to the placing that
// waits. Being one goroutine is what makes an ack's cursor mean "every
// event before the new set is numbered". A stream has at most one
// subscribe without its ack (placings take turns, and one that gives up
// drops the stream), so the ack always finds room.
func (o *owned) read(l *link) {
	defer o.detach(l)
	for {
		msg, err := l.stream.Recv()
		if err != nil {
			l.err = err
			return
		}
		if a := msg.GetAck(); a != nil {
			select {
			case l.acks <- ackFrame{cursor: o.cursor(), ack: a}:
			default: // nobody waits and the buffer is full
			}
			continue
		}
		if evs := eventsOf(msg.GetEvents()); len(evs) > 0 {
			o.push(evs...)
		}
	}
}

// detach records that a guest stream ended, and gives its slot back.
func (o *owned) detach(l *link) {
	o.s.releaseLinkSlot(o.sandbox)
	o.abandon(l)
	close(l.closed)
}

// abandon lets go of a guest stream: it ends, and while it was the
// watch's stream, the readers get a stale event for the whole set (events
// were lost) and the supervisor places the set again on a new one. The
// reader sees the end and detaches it.
func (o *owned) abandon(l *link) {
	l.cancel()
	o.mu.Lock()
	current := o.link == l && !o.closed
	if current {
		o.link = nil
		o.stood = time.Since(l.opened) >= o.s.t.relinkMax
		if len(o.paths) > 0 {
			o.pushLocked(event{Kind: kindStale, StalePaths: append([]string{}, o.paths...)})
		}
	}
	o.mu.Unlock()
	if current {
		o.signal(o.lost)
	}
}
