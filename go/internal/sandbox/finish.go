// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/activity"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// afterCommit is what follows a committed end of an op: the tombstones to
// process now (best effort), and whether the sandbox is to be synced.
type afterCommit struct {
	tombstones []store.TombstoneKey
	sync       bool
}

// finish is §9 "Finish": one transaction, retried with backoff until it
// commits (or finds the op fenced out) for as long as the instance runs.
func (r *runner) finish(h *OpHandle, res callResult, obs observation) (OpResult, afterCommit) {
	return r.retry(h, func(ctx context.Context) (OpResult, afterCommit, error) {
		return r.finishOnce(ctx, h, res, obs)
	})
}

// rollback is §9 "Rollback": the op could not get a connection to its
// target after its begin, so this run sent nothing.
func (r *runner) rollback(h *OpHandle) OpResult {
	res, _ := r.retry(h, func(ctx context.Context) (OpResult, afterCommit, error) {
		return r.rollbackOnce(ctx, h)
	})
	return res
}

// retry runs an op's end transaction until it commits or the engine
// stops.
func (r *runner) retry(h *OpHandle, once func(ctx context.Context) (OpResult, afterCommit, error)) (OpResult, afterCommit) {
	e := r.e
	backoff := e.t.finishMin
	for {
		ctx, cancel := context.WithTimeout(e.bg, 30*time.Second)
		res, ac, err := once(ctx)
		cancel()
		if err == nil {
			return res, ac
		}
		if e.bg.Err() != nil {
			return OpResult{Err: fmt.Errorf("%s of sandbox %s not finished: the instance stops: %w", h.kind, h.id, err)}, afterCommit{}
		}
		e.rl.warn("finish", "cannot finish an op; retrying", "sandbox", h.id, "op", h.kind, "err", err)
		if !sleepCtx(e.bg, backoff) {
			return OpResult{Err: fmt.Errorf("%s of sandbox %s not finished: the instance stops: %w", h.kind, h.id, err)}, afterCommit{}
		}
		backoff = min(backoff*2, e.t.finishMax)
	}
}

func (r *runner) finishOnce(ctx context.Context, h *OpHandle, res callResult, obs observation) (OpResult, afterCommit, error) {
	e := r.e
	var result OpResult
	var ac afterCommit
	var events []activity.Event
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		result, ac, events = OpResult{}, afterCommit{}, nil
		row, err := tx.LockSandbox(ctx, h.id, store.ForUpdate)
		gone := errors.Is(err, model.ErrNotFound)
		if err != nil && !gone {
			return err
		}
		if gone || row.Incarnation != h.inc || row.OpID != h.opID {
			result, ac, events, err = r.fencedOut(ctx, tx, h, row, gone, res, obs)
			// The op's span ends without a state of its own.
			events = append(events, h.endEvent(store.Sandbox{Owner: h.row.Owner, ClientState: "fenced out"}, res))
			return err
		}
		c, err := e.newChange(ctx, tx, row)
		if err != nil {
			return err
		}
		applyOutcome(c, h, res, obs)
		if err := c.commit(ctx); err != nil {
			return err
		}
		w := c.written
		result = OpResult{State: w.ClientState, Node: w.Node, Description: w.StatusDescription}
		ac.tombstones = append(c.tombstoneKeys(), h.tombstones...)
		events = append(c.events, h.endEvent(w, res))
		return nil
	})
	if err != nil {
		return OpResult{}, afterCommit{}, err
	}
	e.record(events...)
	return result, ac, nil
}

// fencedOut is a finish that finds the op no longer the row's (finish
// step 2): it writes a tombstone for a pod a create, start, resume,
// restore or repair may have made for a row that is gone, of another
// incarnation, or placed elsewhere; or merges a checkpoint newer than the
// row's into a row hibernating or hibernated on the target. Nothing else.
func (r *runner) fencedOut(ctx context.Context, tx *store.Store, h *OpHandle, row store.Sandbox, gone bool,
	res callResult, obs observation) (OpResult, afterCommit, []activity.Event, error) {
	result := OpResult{fenced: true}
	if !gone {
		result.State, result.Node, result.Description = row.ClientState, row.Node, row.StatusDescription
	}
	var ac afterCommit
	var events []activity.Event
	makes := h.kind == OpCreate || h.kind == OpStart || h.kind == OpResume || h.kind == OpRestore || h.kind == OpRepair
	if makes && (gone || row.Incarnation != h.inc || (row.Node != h.target && row.OpTarget != h.target)) {
		t := store.Tombstone{PodID: h.id, Node: h.target, Incarnation: h.inc, Reason: fmt.Sprintf("fenced-out %s", h.kind)}
		if _, err := tx.UpsertTombstone(ctx, t); err != nil {
			return OpResult{}, ac, nil, err
		}
		ac.tombstones = []store.TombstoneKey{t.Key()}
		events = append(events, activity.Event{SandboxID: h.id, TenantID: h.row.Owner, Node: h.target,
			Kind: activity.KindTombstone, Name: fmt.Sprintf("tombstone on node %s: %s", h.target, t.Reason),
			Detail: map[string]any{"node": h.target, "incarnation": h.inc.String(), "reason": t.Reason}})
		return result, ac, events, nil
	}
	if gone || row.Incarnation != h.inc {
		return result, ac, nil, nil
	}
	cp := res.checkpoint
	if obs.ours(h.inc) && obs.state() == podv1.PodState_POD_STATE_HIBERNATED && obs.info.GetCheckpoint().GetHeadKey() != "" {
		cp = obs.info.GetCheckpoint()
	}
	if cp.GetHeadKey() != "" && int64(min(cp.GetEpoch(), 1<<63-1)) > row.CheckpointEpoch && row.Node == h.target &&
		(row.ClientState == model.StateHibernating || row.ClientState == model.StateHibernated) {
		c, err := r.e.newChange(ctx, tx, row)
		if err != nil {
			return OpResult{}, ac, nil, err
		}
		c.mergeCheckpoint(cp, h.target)
		written, err := tx.UpdateSandbox(ctx, row, c.after)
		if err != nil {
			return OpResult{}, ac, nil, err
		}
		result.State, result.Node, result.Description = written.ClientState, written.Node, written.StatusDescription
		ac.sync = true
	}
	return result, ac, events, nil
}

func (r *runner) rollbackOnce(ctx context.Context, h *OpHandle) (OpResult, afterCommit, error) {
	e := r.e
	unavailable := errorf(model.ErrNodeUnavailable, "node %s is not connected to instance %s", h.target, e.id)
	result := OpResult{Err: unavailable}
	var events []activity.Event
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		result, events = OpResult{Err: unavailable}, nil
		row, err := tx.LockSandbox(ctx, h.id, store.ForUpdate)
		gone := errors.Is(err, model.ErrNotFound)
		if err != nil && !gone {
			return err
		}
		if gone || row.Incarnation != h.inc || row.OpID != h.opID {
			result.fenced = true
			if !gone {
				result.State, result.Node, result.Description = row.ClientState, row.Node, row.StatusDescription
			}
			events = []activity.Event{h.endEvent(store.Sandbox{Owner: h.row.Owner, ClientState: "fenced out"},
				callResult{err: unavailable})}
			return nil
		}
		c, err := e.newChange(ctx, tx, row)
		if err != nil {
			return err
		}
		a := &c.after
		clearOp(a)
		if !h.takeover {
			a.ClientState, a.Node, a.LostFrom, a.Materialised = h.read.state, h.read.node, h.read.lostFrom, h.read.materialised
			a.StatusDescription = fmt.Sprintf("node %s is not connected to instance %s; nothing was sent", h.target, e.id)
			for _, k := range h.tombstones {
				if _, err := tx.DeleteTombstone(ctx, k); err != nil {
					return err
				}
			}
		} else {
			// The previous owner may have sent the call.
			a.ClientState = model.StateUnknown
			a.StatusDescription = fmt.Sprintf("node %s is not connected to instance %s; the outcome of %s is unknown",
				h.target, e.id, h.kind)
		}
		if err := c.commit(ctx); err != nil {
			return err
		}
		w := c.written
		result.State, result.Node, result.Description = w.ClientState, w.Node, w.StatusDescription
		events = append(c.events, h.endEvent(w, callResult{err: unavailable}))
		return nil
	})
	if err != nil {
		return OpResult{}, afterCommit{}, err
	}
	e.record(events...)
	return result, afterCommit{}, nil
}

// clearOp clears the op columns of a row.
func clearOp(a *store.Sandbox) {
	a.Op, a.OpID, a.OpOwner, a.OpDeadline, a.OpTarget, a.OpFromState = "", uuid.Nil, "", time.Time{}, "", ""
}

// applyOutcome is the outcome table of §9: what the op's target holds of
// the pod after the op's calls decides the sandbox's state. Call errors
// are never mapped directly; the observation after them decides.
func applyOutcome(c *change, h *OpHandle, res callResult, obs observation) {
	a := &c.after
	clearOp(a)
	target := h.target
	switch {
	case !obs.answered:
		a.ClientState = model.StateUnknown
		a.StatusDescription = fmt.Sprintf("%s on node %s: outcome unknown (%s)", h.kind, target,
			errText(res.err, "the node did not answer"))
	case obs.ours(h.inc):
		applyOurs(c, h, res, obs.info)
	case obs.stale(h.inc):
		inc := podIncarnation(obs.info)
		c.tombstone(target, inc, fmt.Sprintf("stale record of incarnation %s", inc))
		c.notFound(target, fmt.Sprintf("%s on node %s found a stale pod of incarnation %s there; it is deleted first",
			h.kind, target, inc))
	default:
		why := ""
		if res.err != nil {
			why = fmt.Sprintf("%s on node %s failed: %s", h.kind, target, callText(res.err))
			if h.kind == OpCreate && placementFailure(res.err) && !a.HasCheckpoint() && !a.Materialised {
				a.AvoidNodes = appendUnique(a.AvoidNodes, target)
				why += "; placed elsewhere on the next use"
			}
		}
		c.notFound(target, why)
	}
}

// applyOurs applies the state of the op's own pod on its target.
func applyOurs(c *change, h *OpHandle, res callResult, info *podv1.PodInfo) {
	a := &c.after
	a.Node, a.Materialised, a.LostFrom = h.target, true, ""
	// A repair restarts the pod and ends its processes, unless it found the
	// pod healthy and made no call; a takeover cannot know what the first
	// owner did.
	if h.kind == OpRepair && (!res.skipped || h.takeover) {
		c.endsProcesses = true
	}
	switch info.GetState() {
	case podv1.PodState_POD_STATE_RUNNING:
		a.ClientState = model.StateRunning
		a.AvoidNodes = []string{}
		a.PendingSince, a.UnreachableSince = time.Time{}, time.Time{}
		switch h.kind {
		case OpCreate, OpStart, OpResume, OpRestore, OpRepair:
			a.LastActiveAt = c.now
		}
		a.StatusDescription = ""
		if h.kind == OpPause {
			a.StatusDescription = "pause failed: " + errText(res.err, "the pod still runs")
		}
		c.openRun = runOrigin(h.kind, res)
		if coldBoot(h.kind, res) {
			c.endsProcesses = true
		}
	case podv1.PodState_POD_STATE_PENDING:
		a.ClientState = model.StateCreating
		if a.PendingSince.IsZero() {
			a.PendingSince = c.now
		}
		a.StatusDescription = fmt.Sprintf("still starting on node %s after %s", h.target,
			time.Since(h.started).Round(time.Second))
	case podv1.PodState_POD_STATE_SUSPENDED:
		a.ClientState = model.StatePaused
		a.PendingSince = time.Time{}
		if h.kind == OpPause || a.PausedAt.IsZero() {
			a.PausedAt = c.now
		}
		a.StatusDescription = ""
		if h.kind == OpHibernate {
			a.StatusDescription = "hibernation failed: " + errText(res.err, "the pod stays suspended")
		}
		c.closeRun = "paused"
		if h.trigger == TriggerIdle {
			c.closeRun = "idle"
		}
	case podv1.PodState_POD_STATE_HIBERNATED:
		a.ClientState = model.StateHibernated
		a.PendingSince = time.Time{}
		cp := info.GetCheckpoint()
		if cp.GetHeadKey() == "" {
			cp = res.checkpoint
		}
		c.mergeCheckpoint(cp, h.target)
		a.StatusDescription = ""
		c.closeRun = "hibernated"
	case podv1.PodState_POD_STATE_STOPPED:
		a.ClientState = model.StateStopped
		a.PendingSince = time.Time{}
		a.StatusDescription = info.GetReason()
		c.closeRun = "stopped"
	case podv1.PodState_POD_STATE_FAILED:
		a.ClientState = model.StateFailed
		a.PendingSince = time.Time{}
		a.StatusDescription = info.GetReason()
		c.closeRun = "failed"
	default:
		a.ClientState = model.StateUnknown
		a.StatusDescription = fmt.Sprintf("%s on node %s: the pod is in state %s", h.kind, h.target, info.GetState())
	}
}

// placementFailure reports whether a create's error, with no pod after
// it, is the node's and not the pod's (§9 outcome table): no address
// (RESOURCE_EXHAUSTED), the network (FAILED_PRECONDITION), a TAP prefix
// another pod has (ALREADY_EXISTS). Any other error, INTERNAL among them,
// may pass; avoiding the node for it would leave a single-node system
// unplaceable, since only a run clears avoid_nodes.
func placementFailure(err error) bool {
	switch grpcCode(err) {
	case codes.ResourceExhausted, codes.FailedPrecondition, codes.AlreadyExists:
		return true
	}
	return false
}

// runOrigin is the origin of the run an op opens.
func runOrigin(kind OpKind, res callResult) string {
	switch kind {
	case OpCreate:
		return "image"
	case OpStart, OpRepair:
		return "disk"
	case OpResume:
		return "snapshot"
	case OpRestore:
		if res.restored && !res.memoryRestored && res.reason != "" {
			return "coldboot: " + res.reason
		}
		return "checkpoint"
	}
	return "adopted"
}

// coldBoot reports whether the op's pod came up booted anew, without its
// memory: a start always, a restore that says so.
func coldBoot(kind OpKind, res callResult) bool {
	switch kind {
	case OpStart:
		return true
	case OpRestore:
		return res.restored && !res.memoryRestored && res.reason != ""
	}
	return false
}

func errText(err error, otherwise string) string {
	if err == nil {
		return otherwise
	}
	return callText(err)
}

func appendUnique(ss []string, s string) []string {
	for _, x := range ss {
		if x == s {
			return ss
		}
	}
	return append(append([]string{}, ss...), s)
}

// endEvent is the activity row of the op's end.
func (h *OpHandle) endEvent(w store.Sandbox, res callResult) activity.Event {
	d := time.Since(h.started).Nanoseconds()
	parent := w.RunID
	if parent == "" {
		parent = h.parent
	}
	detail := map[string]any{"op": string(h.kind), "state": w.ClientState, "node": h.target}
	if w.StatusDescription != "" {
		detail["description"] = w.StatusDescription
	}
	if res.err != nil {
		detail["error"] = callText(res.err)
	}
	return activity.Event{SandboxID: h.id, TenantID: w.Owner, Node: h.target, Kind: activity.KindLifecycleEnd,
		SpanID: h.span, ParentSpanID: parent, DurationNs: &d,
		Name: fmt.Sprintf("%s on node %s: %s", h.kind, h.target, w.ClientState), Detail: detail}
}
