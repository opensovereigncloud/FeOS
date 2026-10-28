// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// startKeyed registers the call of the row this request just inserted,
// and starts it on a context of its own: the request's values without its
// cancellation, ended by a timer at the call's budget (never a deadline,
// which gRPC would send to the node as grpc-timeout, see withLimit) and
// by keyedState.bg. The call is cancellable from its registration on.
// The caller holds the per-key lock; the slot it reserved moves to the
// call.
func (r *Runner) startKeyed(reqCtx context.Context, sb auth.Sandbox, c *call, args json.RawMessage, ref callRef,
	row store.ToolCall) *keyedCall {
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(reqCtx))
	timer := time.AfterFunc(r.k.budget(c.timeout), func() { cancel(errCallBudget) })
	stopBg := context.AfterFunc(r.k.bg, func() { cancel(context.Cause(r.k.bg)) })
	kc := &keyedCall{
		id: row.ID, ref: ref, tool: row.Tool, hash: row.ArgsHash, instance: r.instanceID, started: row.StartedAt,
		ctx: ctx, cancel: cancel,
		stop: func() {
			timer.Stop()
			stopBg()
			cancel(nil)
		},
		beats: map[chan struct{}]struct{}{},
		done:  make(chan struct{}),
	}
	r.k.mu.Lock()
	r.k.calls[ref] = kc
	r.k.mu.Unlock()
	go r.runKeyed(kc, sb, c, args)
	return kc
}

// runKeyed runs the call kc to its end, writes its outcome into its row
// and hands it to its local waiters. It ends with kc.ctx, which ends with
// keyedState.bg, plus the final write (finishKeyed's bounds).
func (r *Runner) runKeyed(kc *keyedCall, sb auth.Sandbox, c *call, args json.RawMessage) {
	res, sent, err := r.callRun(kc.ctx, sb, c, args, kc.ref.key, kc.beat,
		func(timeout time.Duration) error { return r.gate(kc, timeout) })
	kc.stop()
	kc.mu.Lock()
	kc.ended = true
	kc.mu.Unlock()
	out := r.classify(res, sent, err)
	r.settle(kc, r.finishKeyed(kc, out))
}

// gate is the call kc's beforeSend: a stopping instance sends a call only
// if it can end before the shutdown grace; the first send commits sent in
// the row first. An error ends the call unsent.
func (r *Runner) gate(kc *keyedCall, timeout time.Duration) error {
	r.k.mu.Lock()
	dl := r.k.drainDeadline
	r.k.mu.Unlock()
	if !dl.IsZero() && time.Until(dl) < timeout+sendMargin {
		return fmt.Errorf("instance %s is stopping and the call (timeout %s) would not end before its shutdown grace: %w",
			r.instanceID, timeout, model.ErrStopping)
	}
	if kc.sentRecorded {
		return nil
	}
	ctx, cancel := context.WithTimeout(kc.ctx, bookkeepingTimeout)
	defer cancel()
	marked, err := r.st.MarkToolCallSent(ctx, kc.id)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRecord, err)
	}
	if !marked {
		// Another writer ended the row: the call must not be sent.
		return errCallLost
	}
	kc.sentRecorded = true
	return nil
}

// beat is the call's progress: every chunk beats its waiters.
func (kc *keyedCall) beat(string) {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	for ch := range kc.beats {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// classify is the outcome of a call with an id that callRun answered
// with res, sent and err (ADR 0023):
//   - no error: done;
//   - cancelled by its caller: done with ErrCancelledByCaller, even unsent;
//   - cut because the instance stops (the drain, Run's end): interrupted
//     when sent, not_run (model.ErrStopping) when not;
//   - its row ended by another writer: interrupted when sent, not_run when
//     not; the final write is refused and the stored row is the answer;
//   - any other error: not_run when unsent, done with the error otherwise
//     (a timeout, a node error, the budget, a forced drain).
func (r *Runner) classify(res Result, sent bool, err error) outcome {
	switch {
	case err == nil:
		return outcome{res: res, state: CallDone}
	case errors.Is(err, ErrCancelledByCaller):
		return outcome{res: res, err: ErrCancelledByCaller, state: CallDone}
	case errors.Is(err, errStopping), errors.Is(err, errRunnerStopped):
		if sent {
			return outcome{res: res, state: CallInterrupted, err: fmt.Errorf("interrupted: instance %s stopped before the call "+
				"ended (its shutdown grace passed); what the call did in the sandbox is unknown: look before repeating it",
				r.instanceID)}
		}
		return outcome{res: res, state: CallNotRun, err: fmt.Errorf("instance %s is stopping: %w", r.instanceID, model.ErrStopping)}
	case errors.Is(err, errCallLost):
		if sent {
			return outcome{res: res, err: errCallLost, state: CallInterrupted}
		}
		return outcome{res: res, err: errCallLost, state: CallNotRun}
	case !sent:
		return outcome{res: res, err: err, state: CallNotRun}
	}
	return outcome{res: res, err: err, state: CallDone}
}

// finishKeyed writes the outcome out into the call's row and returns what
// its local waiters get: out, or the stored row when another writer ended
// it first (or, if that cannot be read, interrupted with errCallLost).
// Each attempt is bounded by bookkeepingTimeout; failed ones are retried
// with backoff for up to finishTimeout, and for at most cutWrite once
// keyedState.bg ended. When every attempt fails, the waiters get out and
// the row expires later with the verdict of its sent column.
func (r *Runner) finishKeyed(kc *keyedCall, out outcome) outcome {
	o := store.ToolCallOutcome{
		State: out.state, ExitCode: out.res.ExitCode, Output: []byte(out.res.Text),
		Truncated: out.res.Truncated, IsError: out.res.IsError, Error: sanitizeError(out.err),
	}
	deadline := time.Now().Add(finishTimeout)
	bgDone := r.k.bg.Done()
	if r.k.bg.Err() != nil {
		deadline, bgDone = earlier(deadline, time.Now().Add(cutWrite)), nil
	}
	backoff := 100 * time.Millisecond
	var lastErr error
	for {
		ctx, cancel := context.WithDeadline(context.Background(), earlier(deadline, time.Now().Add(bookkeepingTimeout)))
		finished, err := r.k.finish(ctx, kc.id, o)
		cancel()
		if err == nil {
			if finished {
				return out
			}
			return r.storedOutcome(kc, out, deadline)
		}
		lastErr = err
		if !time.Now().Before(deadline) {
			break
		}
		t := time.NewTimer(min(backoff, time.Until(deadline)))
		select {
		case <-t.C:
		case <-bgDone:
			t.Stop()
			deadline, bgDone = earlier(deadline, time.Now().Add(cutWrite)), nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		backoff = min(2*backoff, 5*time.Second)
	}
	r.log.Warn("the final write of a tool call with an id failed; its row expires", "call", kc.id,
		"sandbox", kc.ref.sandbox, "state", out.state, "err", lastErr)
	return out
}

// storedOutcome is the outcome of the call kc whose row another writer
// ended: the stored state and result, or interrupted with errCallLost
// (and the output so far) when the row cannot be read or is another run's.
func (r *Runner) storedOutcome(kc *keyedCall, out outcome, deadline time.Time) outcome {
	ctx, cancel := context.WithDeadline(context.Background(), earlier(deadline, time.Now().Add(bookkeepingTimeout)))
	row, err := r.st.GetToolCall(ctx, kc.ref.sandbox, kc.ref.key)
	cancel()
	if err != nil || row.ID != kc.id || row.State == store.ToolCallRunning {
		return outcome{res: out.res, err: errCallLost, state: CallInterrupted}
	}
	res, _, serr := stored(row)
	return outcome{res: res, err: serr, state: row.State}
}

// earlier is the earlier of a and b.
func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// settle hands the outcome out to the call's local waiters, removes the
// call and gives its slot back.
func (r *Runner) settle(kc *keyedCall, out outcome) {
	r.k.mu.Lock()
	kc.out = out
	close(kc.done)
	if r.k.calls[kc.ref] == kc {
		delete(r.k.calls, kc.ref)
	}
	if r.k.perSandbox[kc.ref.sandbox]--; r.k.perSandbox[kc.ref.sandbox] <= 0 {
		delete(r.k.perSandbox, kc.ref.sandbox)
	}
	r.k.mu.Unlock()
	r.k.wg.Done()
}

// sanitizeError is err's text as a row stores it: valid UTF-8 (invalid
// bytes become U+FFFD), without NUL, cut at maxCallError bytes on a rune
// boundary; "" for nil. A node's message reaches it verbatim.
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToValidUTF8(err.Error(), "�")
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) > maxCallError {
		cut := maxCallError
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

// storedError is the error of a stored error text; nil for "".
func storedError(s string) error {
	if s == "" {
		return nil
	}
	return errors.New(s)
}

// renewCalls renews the rows of this instance's calls with an id every
// third of the lease, until ctx ends.
func (r *Runner) renewCalls(ctx context.Context) {
	lease := r.lease()
	t := time.NewTicker(max(lease/3, time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.renewCallsOnce(ctx, lease)
		}
	}
}

// renewCallsOnce extends the leases of the calls with an id running here.
// A call whose row is no longer its running row (another writer ended
// it) is cancelled with errCallLost; its final write is then refused.
func (r *Runner) renewCallsOnce(ctx context.Context, lease time.Duration) {
	r.k.mu.Lock()
	byID := make(map[uuid.UUID]*keyedCall, len(r.k.calls))
	for _, kc := range r.k.calls {
		byID[kc.id] = kc
	}
	r.k.mu.Unlock()
	if len(byID) == 0 {
		return
	}
	ids := make([]uuid.UUID, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	rctx, cancel := context.WithTimeout(ctx, bookkeepingTimeout)
	renewed, err := r.st.RenewToolCalls(rctx, r.instanceID, ids, lease)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			r.rl.warn("renew", "renew the rows of tool calls with an id", "calls", len(ids), "err", err)
		}
		return
	}
	for _, id := range renewed {
		delete(byID, id)
	}
	for _, kc := range byID {
		r.lost(kc)
	}
}

// lost cancels the call kc, whose row the renewal did not find running,
// with errCallLost, and warns once. A call that already ended (its final
// write ended the row) is left alone.
func (r *Runner) lost(kc *keyedCall) {
	kc.mu.Lock()
	ended, warned := kc.ended, kc.lostWarned
	kc.lostWarned = true
	kc.mu.Unlock()
	if ended {
		return
	}
	kc.cancel(errCallLost)
	if !warned {
		r.log.Warn("the row of a tool call with an id was ended by another writer while the call ran; cancelling the call",
			"call", kc.id, "sandbox", kc.ref.sandbox)
	}
}
