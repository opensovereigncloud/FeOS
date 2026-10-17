// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package leader is the lifecycle of an instance among its peers: the
// leader lease, which one instance holds at a time and under which it runs
// the leader's loops (the policies, sweeps and retention jobs of the other
// packages), and the instance's own heartbeat row, which tells the others
// that it lives, and that it stops.
//
// The lease is the row "leader" of leases, with a TTL of 15 s and an epoch
// that grows on every acquisition: the fencing token. A leader renews it
// every 5 s and steps down at once when a renew fails, finds the lease
// gone, or has not succeeded within 13 s of the last successful renew's
// send (the TTL less a margin of 2 s), so that it never acts on a lease
// another instance may already hold. The leader's bulk transactions call
// Fence first, which takes a share lock on the lease row in that epoch.
package leader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// Loop is a job the leader runs on its interval and when woken.
type Loop = model.Loop

// The lease's timings (§7 of the design).
const (
	// LeaseTTL is how long a lease lasts after its acquisition or renewal.
	LeaseTTL = 15 * time.Second
	// RenewInterval is how often the leader renews its lease.
	RenewInterval = 5 * time.Second
	// ReadInterval is how often an instance that does not lead reads the
	// lease to see whether it expired.
	ReadInterval = 5 * time.Second
	// StepDownMargin is how much earlier than the lease's expiry a leader
	// whose renewals fail steps down: the TTL is measured on the database's
	// clock from a renew's commit, the step-down deadline on the
	// instance's clock from the renew's send.
	StepDownMargin = 2 * time.Second
	// StaleInstanceAge is how long an instance's row stays after its last
	// heartbeat before the leader deletes it.
	StaleInstanceAge = time.Hour
)

// ErrNotLeader is returned by Fence when this instance does not hold the
// lease in the epoch it led with: a transaction that gets it must abort.
var ErrNotLeader = errors.New("not the leader")

// LoopStaleInstances names the leader's loop that deletes the rows of
// instances not seen for StaleInstanceAge.
const LoopStaleInstances = "stale-instances"

// Elector competes for the leader lease and, while it holds it, runs the
// registered loops.
type Elector struct {
	d   *db.DB
	st  *store.Store
	id  string
	log *slog.Logger

	// The timings, fields so that tests can shorten them.
	ttl, renewEvery, readEvery, margin time.Duration

	mu    sync.Mutex
	loops []Loop
	term  *term // nil while not leading
	// last is the most recent term, kept after it ended, so that Resign
	// releases its lease also when the term ended with Run's context.
	last     *term
	resigned bool
}

// term is one tenure of the lease.
type term struct {
	epoch int64
	since time.Time // when the acquisition was sent, on this instance's clock
	// deadline is the local step-down deadline: the send time of the last
	// successful renew (or the acquisition) plus TTL less the margin.
	// Guarded by Elector.mu.
	deadline time.Time
	cancel   context.CancelFunc
	done     chan struct{} // closed when the term's loops have stopped
}

// NewElector returns an Elector for instanceID. It registers the leader's
// own loop, which deletes the rows of instances not seen for an hour. log
// is the instance's, which names the instance already.
func NewElector(d *db.DB, instanceID string, log *slog.Logger) *Elector {
	if log == nil {
		log = slog.Default()
	}
	e := &Elector{
		d: d, st: store.New(d), id: instanceID, log: log.With("component", "leader"),
		ttl: LeaseTTL, renewEvery: RenewInterval, readEvery: ReadInterval, margin: StepDownMargin,
	}
	e.Register(Loop{Name: LoopStaleInstances, Interval: time.Minute, Run: e.deleteStaleInstances})
	return e
}

// Register adds loops. Loops registered while a term runs start with the
// next term; register them before Run.
func (e *Elector) Register(loops ...Loop) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.loops = append(e.loops, loops...)
}

// Run competes for the lease until ctx ends and returns nil then. While
// not leading it reads the lease every 5 s and tries to acquire it when
// there is none or it has expired; while leading it renews the lease every
// 5 s and runs the loops. A failed renew, a lost lease or the step-down
// deadline ends the term at once: the loops' context is cancelled and Run
// waits for them before it competes again. Run does not release the lease
// when ctx ends (Resign does), so that an instance that dies and one whose
// Run is cancelled look the same to the others: the lease expires.
//
// At SIGTERM (§7 step 1) the instance calls Resign, which stops the loops
// and releases the lease; ctx is the instance's own and ends with its last
// step (§7 step 5), not at SIGTERM. Resign releases the lease of the last
// term also when ctx has ended already.
func (e *Elector) Run(ctx context.Context) error {
	wait := time.Duration(0)
	for {
		if !sleep(ctx, wait) {
			return nil
		}
		wait = e.readEvery
		if e.isResigned() {
			<-ctx.Done()
			return nil
		}
		epoch, sent, ok := e.tryAcquire(ctx)
		if !ok {
			continue
		}
		e.lead(ctx, epoch, sent)
	}
}

func (e *Elector) isResigned() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.resigned
}

// tryAcquire reads the lease and acquires it when there is none or it has
// expired. It reports the epoch and the time the acquisition was sent.
func (e *Elector) tryAcquire(ctx context.Context) (int64, time.Time, bool) {
	rctx, cancel := context.WithTimeout(ctx, e.ttl-e.margin)
	defer cancel()
	l, found, err := e.st.ReadLease(rctx, store.LeaderLease)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("cannot read the leader lease", "err", err)
		}
		return 0, time.Time{}, false
	}
	if found && l.Valid {
		return 0, time.Time{}, false
	}
	sent := time.Now()
	epoch, ok, err := e.st.AcquireLease(rctx, store.LeaderLease, e.id, e.ttl)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("cannot acquire the leader lease", "err", err)
		}
		return 0, time.Time{}, false
	}
	return epoch, sent, ok
}

// lead is one term: it runs the loops and renews the lease until a renew
// fails, the step-down deadline passes, Resign ends it or ctx ends.
func (e *Elector) lead(ctx context.Context, epoch int64, sent time.Time) {
	tctx, cancel := context.WithCancel(ctx)
	t := &term{epoch: epoch, since: sent, deadline: sent.Add(e.ttl - e.margin), cancel: cancel, done: make(chan struct{})}
	e.mu.Lock()
	if e.resigned {
		e.mu.Unlock()
		cancel()
		close(t.done)
		if err := e.release(ctx, epoch); err != nil {
			e.log.Warn("cannot release the leader lease acquired after resigning", "epoch", epoch, "err", err)
		}
		return
	}
	e.term, e.last = t, t
	loops := slices.Clone(e.loops)
	e.mu.Unlock()
	e.log.Info("leading", "epoch", epoch)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.schedule(tctx, loops)
	}()

	reason := e.renewLoop(tctx, t)

	e.mu.Lock()
	if e.term == t {
		e.term = nil
	}
	e.mu.Unlock()
	cancel()
	wg.Wait()
	close(t.done)
	e.log.Info("stepped down", "epoch", epoch, "reason", reason)
}

// renewLoop renews the lease every renewEvery until the term ends; it
// returns why it ended.
func (e *Elector) renewLoop(ctx context.Context, t *term) string {
	e.mu.Lock()
	deadline := t.deadline
	e.mu.Unlock()
	tick := time.NewTicker(e.renewEvery)
	defer tick.Stop()
	stepDown := time.NewTimer(time.Until(deadline))
	defer stepDown.Stop()
	for {
		select {
		case <-ctx.Done():
			return "term ended"
		case <-stepDown.C:
			return "step-down deadline reached"
		case <-tick.C:
		}
		sent := time.Now()
		rctx, cancel := context.WithDeadline(ctx, deadline)
		_, ok, err := e.st.RenewLease(rctx, store.LeaderLease, e.id, t.epoch, e.ttl)
		cancel()
		switch {
		case ctx.Err() != nil:
			return "term ended"
		case err != nil:
			e.log.Warn("cannot renew the leader lease; stepping down", "epoch", t.epoch, "err", err)
			return "renew failed"
		case !ok:
			e.log.Warn("the leader lease is lost; stepping down", "epoch", t.epoch)
			return "lease lost"
		}
		deadline = sent.Add(e.ttl - e.margin)
		e.mu.Lock()
		t.deadline = deadline
		e.mu.Unlock()
		stepDown.Reset(time.Until(deadline))
	}
}

// schedule runs every loop on its interval, once at the start of the term
// and whenever ferun_leader wakes it, one run at a time per loop, until ctx
// ends; it returns when every loop's run has returned.
func (e *Elector) schedule(ctx context.Context, loops []Loop) {
	ch, unsubscribe := e.d.Subscribe(db.ChanLeader)
	defer unsubscribe()
	wakes := make([]chan struct{}, len(loops))
	var wg sync.WaitGroup
	for i, l := range loops {
		wakes[i] = make(chan struct{}, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.runLoop(ctx, l, wakes[i])
		}()
	}
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case hint := <-ch:
			for _, i := range woken(loops, hint) {
				select {
				case wakes[i] <- struct{}{}:
				default: // a wake is pending already: the run it causes covers this one
				}
			}
		}
	}
}

// woken returns the indexes of the loops a ferun_leader hint wakes: those
// whose name contains the hint, or every loop when none does and for the
// hub's "" (re-read: a notification may have been missed) and "*".
func woken(loops []Loop, hint string) []int {
	var idx []int
	if hint != "" && hint != "*" {
		for i, l := range loops {
			if strings.Contains(l.Name, hint) {
				idx = append(idx, i)
			}
		}
	}
	if len(idx) == 0 {
		idx = make([]int, len(loops))
		for i := range loops {
			idx[i] = i
		}
	}
	return idx
}

// runLoop runs one loop at once, then after every Interval (counted from
// the end of a run) and on every wake, until ctx ends.
func (e *Elector) runLoop(ctx context.Context, l Loop, wake <-chan struct{}) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-wake:
		}
		e.runOnce(ctx, l)
		if l.Interval > 0 {
			timer.Reset(l.Interval)
		} else {
			timer.Stop()
		}
	}
}

// runOnce runs a loop once. Its error is logged, never fatal; a panic in
// it is logged with its stack and the loop runs again on its schedule.
func (e *Elector) runOnce(ctx context.Context, l Loop) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("leader loop panicked", "loop", l.Name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if err := l.Run(ctx); err != nil && ctx.Err() == nil {
		e.log.Warn("leader loop failed", "loop", l.Name, "err", err)
	}
}

// IsLeader reports whether this instance holds the lease and its
// step-down deadline has not passed.
func (e *Elector) IsLeader() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.term != nil && time.Now().Before(e.term.deadline)
}

// LeadingSince returns when the current term began (when its acquisition
// was sent, on this instance's clock), or the zero time when not leading.
func (e *Elector) LeadingSince() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.term == nil || !time.Now().Before(e.term.deadline) {
		return time.Time{}
	}
	return e.term.since
}

// Fence checks, in q's transaction, that this instance still holds the
// lease in the epoch of its current term, and takes a share lock on the
// lease row, so that no other instance's acquisition commits before the
// transaction does. ErrNotLeader means the transaction must abort.
func (e *Elector) Fence(ctx context.Context, q db.Querier) error {
	e.mu.Lock()
	t := e.term
	leading := t != nil && time.Now().Before(t.deadline)
	e.mu.Unlock()
	if !leading {
		return ErrNotLeader
	}
	ok, err := store.On(q).FenceLease(ctx, store.LeaderLease, e.id, t.epoch)
	if err != nil {
		return fmt.Errorf("fence the leader lease: %w", err)
	}
	if !ok {
		return ErrNotLeader
	}
	return nil
}

// Leader reads who holds the lease: the holder, its epoch and when it
// acquired it (the database's clock). With no valid lease, holder is "".
func (e *Elector) Leader(ctx context.Context) (holder string, epoch int64, since time.Time, err error) {
	l, found, err := e.st.ReadLease(ctx, store.LeaderLease)
	if err != nil {
		return "", 0, time.Time{}, err
	}
	if !found || !l.Valid {
		return "", 0, time.Time{}, nil
	}
	return l.Holder, l.Epoch, l.AcquiredAt, nil
}

// Resign ends leadership for good (§7, an instance that stops): it stops
// the loops, waits for their runs to return (or ctx), and lets the lease
// expire now, so that another instance takes over at its next read. Run
// does not compete again afterwards. When no term runs, the lease of the
// last one is released all the same (a term that ended with Run's context
// leaves its lease valid until the TTL); the release is guarded by the
// holder and the epoch, so it changes nothing when another instance holds
// the lease.
func (e *Elector) Resign(ctx context.Context) error {
	e.mu.Lock()
	e.resigned = true
	t := e.term
	if t == nil {
		t = e.last
	}
	e.mu.Unlock()
	if t == nil {
		return nil
	}
	t.cancel()
	select {
	case <-t.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return e.release(ctx, t.epoch)
}

// release lets the lease of epoch expire now.
func (e *Elector) release(ctx context.Context, epoch int64) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	held, err := e.st.ReleaseLease(rctx, store.LeaderLease, e.id, epoch)
	if err != nil {
		return fmt.Errorf("release the leader lease: %w", err)
	}
	if held {
		e.log.Info("released the leader lease", "epoch", epoch)
	}
	return nil
}

// deleteStaleInstances is the loop LoopStaleInstances: a retention job,
// idempotent and unfenced, waiting at most 1 s for a lock.
func (e *Elector) deleteStaleInstances(ctx context.Context) error {
	return e.st.InTx(ctx, func(tx *store.Store) error {
		if err := tx.SetLocalTimeouts(ctx, time.Second, 30*time.Second); err != nil {
			return err
		}
		n, err := tx.DeleteStaleInstances(ctx, StaleInstanceAge)
		if err != nil {
			return fmt.Errorf("delete stale instances: %w", err)
		}
		if n > 0 {
			e.log.Info("deleted the rows of stale instances", "rows", n)
		}
		return nil
	})
}

// sleep waits for d or ctx; false when ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
