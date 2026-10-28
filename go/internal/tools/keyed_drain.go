// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"time"
)

// The drain of the calls with an id (ADR 0023 D10): a stopping instance
// lets the calls it runs end, sends no call that would not end before its
// shutdown grace, and releases the requests that wait for other
// instances' calls, so that they do not hold its HTTP shutdown.

// errReleased is the cause with which the drain ends a request's wait for
// another instance's call (untilRelease).
var errReleased = errors.New("the instance is stopping")

// BeginDrain is the start of stop step 2 for the calls with an id, right
// after the readiness delay; deadline is the shutdown grace's end. From
// then on:
//   - a new call with an id is refused, not_run (model.ErrStopping), unless
//     its key names a call that ran or runs (attached to or answered);
//   - a request that waits for another instance's call is answered at
//     once that the call still runs (*AskAgainError, state running), and
//     WaitCall answers running at once;
//   - a call is sent into the guest only if its timeout and sendMargin end
//     before deadline; otherwise it ends not_run, and its caller sends it
//     again to another instance.
//
// The calls already sent run on; Drain waits for them. A zero deadline
// leaves the send gate open. BeginDrain may be called more than once; the
// first deadline counts.
func (r *Runner) BeginDrain(deadline time.Time) {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	if r.k.draining {
		return
	}
	r.k.draining = true
	r.k.drainDeadline = deadline
	close(r.k.release)
}

// Drain is stop step 2b for the calls with an id, after the HTTP shutdown
// and before the engine's stop: it waits for the calls with an id still
// running on this instance (those whose callers left too) and for the
// joins that reserved a slot, until ctx ends. Then it cuts them
// (errStopping): a call that was sent ends interrupted, with its output so
// far; one that was not, not_run. It waits up to cutWrite more for their
// final writes.
//
// Drain begins the drain (BeginDrain) with ctx's deadline, or now when ctx
// has none, if it has not begun. It may be called more than once.
func (r *Runner) Drain(ctx context.Context) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now()
	}
	r.BeginDrain(deadline)
	// No slot is reserved once draining is set (reserve), so no wg.Add
	// races this Wait.
	settled := make(chan struct{})
	go func() {
		r.k.wg.Wait()
		close(settled)
	}()
	select {
	case <-settled:
		return
	case <-ctx.Done():
	}
	select {
	case <-settled:
		return
	default:
	}
	r.k.mu.Lock()
	n := len(r.k.calls)
	r.k.mu.Unlock()
	r.log.Warn("tool calls still running at the shutdown grace; cutting them", "calls", n)
	r.k.bgCancel(errStopping)
	t := time.NewTimer(cutWrite)
	defer t.Stop()
	select {
	case <-settled:
	case <-t.C:
		r.log.Warn("tool calls still not ended after they were cut; their rows expire", "wait", cutWrite)
	}
}

// untilRelease returns a child of ctx that the drain's start (BeginDrain)
// cancels with errReleased; stop releases it. It holds a goroutine until
// either happens.
func (k *keyedState) untilRelease(ctx context.Context) (rctx context.Context, stop func()) {
	rctx, cancel := context.WithCancelCause(ctx)
	go func() {
		select {
		case <-k.release:
			cancel(errReleased)
		case <-rctx.Done():
		}
	}()
	return rctx, func() { cancel(nil) }
}
