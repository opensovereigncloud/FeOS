// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The leader's loops of the calls with an id (ADR 0023 D5, D14), and the
// ferun_leader hint that wakes both: a writer that wants them to run now
// sends db.Notify(ctx, q, db.ChanLeader, HintCalls).
const (
	LoopCallsExpiry    = "tool-calls-expiry"
	LoopCallsRetention = "tool-calls-retention"

	HintCalls = "tool-calls"
)

// The loops' intervals.
const (
	callsExpiryEvery    = 5 * time.Second
	callsRetentionEvery = time.Minute
)

// The limits of the loops' runs.
const (
	// expiryBatch is the most rows one fenced expiry transaction ends.
	expiryBatch = 256
	// retentionBatch is the most rows one retention delete removes, and
	// retentionBatches the most deletes of one run: what is left waits for
	// the next.
	retentionBatch   = 1000
	retentionBatches = 10
	// The fenced transaction's lock and statement timeouts, as fleet's:
	// the share lock on the lease row never holds the leader's renew up
	// for long.
	fencedLockTimeout      = time.Second
	fencedStatementTimeout = 3 * time.Second
)

// Loops returns the leader loops of the calls with an id, for the
// elector's Register: the expiry of the rows whose instance stopped
// renewing them (every 5 s) and the deletion of the records that ended
// more than the call retention ago (every minute). nil on a Runner without
// a database or without a Leader (SetCallConfig, which comes first).
func (r *Runner) Loops() []model.Loop {
	if l, _, _ := r.loopConfig(); r.d == nil || l == nil {
		return nil
	}
	return []model.Loop{
		{Name: LoopCallsExpiry, Interval: callsExpiryEvery, Run: r.expireCalls},
		{Name: LoopCallsRetention, Interval: callsRetentionEvery, Run: r.deleteFinished},
	}
}

// loopConfig is what the loops read of the CallConfig.
func (r *Runner) loopConfig() (l Leader, lease, retention time.Duration) {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	return r.k.leader, r.k.lease, r.k.retention
}

// callsHealthy reports whether the leader may end the rows whose lease ran
// out: it has led for at least a call lease and its database has not
// failed within one, so that after a failover or an outage of the database
// every instance has had a full lease to renew its rows. It is fleet's
// guard of the node expiry (healthy, fleet/loops.go:68-81).
func (r *Runner) callsHealthy() bool {
	l, lease, _ := r.loopConfig()
	if l == nil || r.d == nil {
		return false
	}
	since := l.LeadingSince()
	if since.IsZero() || time.Since(since) < lease {
		return false
	}
	return r.mayExpire(lease)
}

// expireCalls is the leader's expiry of the calls with an id (ADR 0023
// D5): while callsHealthy, every running row whose lease ran out ends,
// interrupted when it was sent and not_run when not, in fenced
// transactions of at most expiryBatch rows that send ferun_call, until one
// comes back short. A leader that lost the lease (leader.ErrNotLeader)
// ends the run quietly; the next leader's run takes over.
func (r *Runner) expireCalls(ctx context.Context) error {
	if !r.callsHealthy() {
		return nil
	}
	l, _, _ := r.loopConfig()
	for {
		var ids []uuid.UUID
		err := r.st.InTx(ctx, func(tx *store.Store) error {
			ids = nil
			if err := tx.SetLocalTimeouts(ctx, fencedLockTimeout, fencedStatementTimeout); err != nil {
				return err
			}
			if err := l.Fence(ctx, tx.Q()); err != nil {
				return err
			}
			var err error
			ids, err = tx.ExpireToolCalls(ctx, 0, expiryBatch)
			return err
		})
		if errors.Is(err, leader.ErrNotLeader) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tool call expiry: %w", err)
		}
		if len(ids) > 0 {
			r.log.Info("tool calls ended: their instance stopped renewing them", "calls", len(ids))
		}
		if len(ids) < expiryBatch {
			return nil
		}
	}
}

// deleteFinished is the retention of the calls with an id (ADR 0023 D14):
// it deletes the records (done, interrupted, not_run) that ended more than
// the call retention ago, in up to retentionBatches deletes of at most
// retentionBatch rows, until one comes back short. Unfenced: two leaders
// that run it at once delete the same rows.
func (r *Runner) deleteFinished(ctx context.Context) error {
	_, _, retention := r.loopConfig()
	var total int64
	defer func() {
		if total > 0 {
			r.log.Info("retention deleted old rows", "rows", total, "of", "tool calls")
		}
	}()
	for range retentionBatches {
		n, err := r.st.DeleteFinishedToolCalls(ctx, retention, retentionBatch)
		if err != nil {
			return fmt.Errorf("retention of tool calls: %w", err)
		}
		total += n
		if n < retentionBatch {
			return nil
		}
	}
	return nil
}
