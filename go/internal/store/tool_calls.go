// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// The states of a tool call (ADR 0023). A call is running from its insert
// until one writer ends it: done with its result, interrupted when it may
// have run in the sandbox without a recorded result, not_run when it
// cannot have run there.
const (
	ToolCallRunning     = "running"
	ToolCallDone        = "done"
	ToolCallInterrupted = "interrupted"
	ToolCallNotRun      = "not_run"
)

// ToolCall is a row of tool_calls: one run of a keyed call of a tool with a
// side effect, from its insert until an hour after it ended.
type ToolCall struct {
	// ID identifies the run: the ferun_call payload and the renewal
	// handle. A run that replaces a not_run row has a new ID.
	ID        uuid.UUID
	SandboxID podid.ID
	Key       string // the caller's idempotency key, unique per sandbox
	Tool      string
	ArgsHash  []byte // SHA-256 of the tool and its normalised arguments
	// InstanceID is the instance that runs (or ran) the call and renews
	// its lease.
	InstanceID string
	State      string
	// Sent: the call's request may have left for the guest. It is
	// committed before the send.
	Sent       bool
	LeaseUntil time.Time
	StartedAt  time.Time
	FinishedAt time.Time // zero while running
	ExitCode   int
	Output     []byte
	Truncated  bool
	IsError    bool
	Error      string
}

// NewToolCall is a run BeginToolCall records.
type NewToolCall struct {
	SandboxID  podid.ID
	Key        string
	Tool       string
	ArgsHash   []byte // 32 bytes
	InstanceID string
	Lease      time.Duration // the first lease, from now
}

// ToolCallOutcome is how FinishToolCall ends a run.
type ToolCallOutcome struct {
	State     string // done, interrupted or not_run
	ExitCode  int
	Output    []byte // at most 64 KiB
	Truncated bool
	IsError   bool
	Error     string // at most 8 KiB
}

const toolCallCols = `id, sandbox_id, call_key, tool, args_hash, instance_id, state, sent, lease_until, started_at,
	finished_at, exit_code, output, truncated, is_error, error`

func scanToolCall(row pgx.Row) (ToolCall, error) {
	var c ToolCall
	var finished *time.Time
	if err := row.Scan(&c.ID, &c.SandboxID, &c.Key, &c.Tool, &c.ArgsHash, &c.InstanceID, &c.State, &c.Sent,
		&c.LeaseUntil, &c.StartedAt, &finished, &c.ExitCode, &c.Output, &c.Truncated, &c.IsError, &c.Error); err != nil {
		return ToolCall{}, err
	}
	c.FinishedAt = timeOf(finished)
	return c, nil
}

// beginToolCallRetries is how often BeginToolCall inserts again when the
// row its insert conflicted with was deleted before it could be read.
const beginToolCallRetries = 3

// BeginToolCall records a new run of a keyed call: a running row with the
// lease c.Lease from now, not sent. A not_run row of the key with the same
// tool and argument hash is replaced by the new run (a new ID, every
// result column reset). It returns the new row and true, or, when the key
// has another row (running, done, interrupted, or not_run with another
// tool or hash), that row unchanged and false. model.ErrNotFound when the
// sandbox does not exist. It sends no notification.
func (s *Store) BeginToolCall(ctx context.Context, c NewToolCall) (ToolCall, bool, error) {
	for range 1 + beginToolCallRetries {
		row, err := scanToolCall(s.q.QueryRow(ctx, `INSERT INTO tool_calls (sandbox_id, call_key, tool, args_hash,
				instance_id, lease_until)
			VALUES ($1, $2, $3, $4, $5, now() + $6::interval)
			ON CONFLICT (sandbox_id, call_key) DO UPDATE SET id = gen_random_uuid(), instance_id = EXCLUDED.instance_id,
				state = 'running', sent = false, lease_until = EXCLUDED.lease_until, started_at = now(),
				finished_at = NULL, exit_code = 0, output = ''::bytea, truncated = false, is_error = false, error = ''
			WHERE tool_calls.state = 'not_run' AND tool_calls.tool = EXCLUDED.tool
				AND tool_calls.args_hash = EXCLUDED.args_hash
			RETURNING `+toolCallCols, c.SandboxID, c.Key, c.Tool, c.ArgsHash, c.InstanceID, interval(c.Lease)))
		switch {
		case err == nil:
			return row, true, nil
		case isForeignKeyViolation(err):
			return ToolCall{}, false, model.ErrNotFound
		case !errors.Is(err, pgx.ErrNoRows):
			return ToolCall{}, false, err
		}
		// The key has a row the insert may not replace.
		existing, err := s.GetToolCall(ctx, c.SandboxID, c.Key)
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, ErrNoRow) {
			return ToolCall{}, false, err
		}
		// The retention deleted the row in between: insert again.
	}
	return ToolCall{}, false, fmt.Errorf("tool call %s of sandbox %s: its row was deleted between the insert and the read "+
		"%d times: %w", c.Key, c.SandboxID, 1+beginToolCallRetries, ErrNoRow)
}

// GetToolCall reads the row of a sandbox's call key; ErrNoRow if there is
// none.
func (s *Store) GetToolCall(ctx context.Context, id podid.ID, key string) (ToolCall, error) {
	c, err := scanToolCall(s.q.QueryRow(ctx,
		"SELECT "+toolCallCols+" FROM tool_calls WHERE sandbox_id = $1 AND call_key = $2", id, key))
	return c, noRow(err, ErrNoRow)
}

// MarkToolCallSent sets sent on a running call, before its request leaves
// for the guest. false: the call is no longer running (another writer
// ended it), and must not be sent. It sends no notification.
func (s *Store) MarkToolCallSent(ctx context.Context, id uuid.UUID) (bool, error) {
	var marked bool
	err := s.q.QueryRow(ctx, "UPDATE tool_calls SET sent = true WHERE id = $1 AND state = 'running' RETURNING true", id).
		Scan(&marked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return marked, err
}

// FinishToolCall ends a running call with o, in one transaction that sends
// ferun_call with the id. An o.State other than done, interrupted or
// not_run is refused before anything is written. false: the call was no
// longer running (another writer ended it first), and nothing changed.
func (s *Store) FinishToolCall(ctx context.Context, id uuid.UUID, o ToolCallOutcome) (bool, error) {
	switch o.State {
	case ToolCallDone, ToolCallInterrupted, ToolCallNotRun:
	default:
		return false, fmt.Errorf("tool call %s: %q is not a final state (done, interrupted, not_run)", id, o.State)
	}
	var finished bool
	err := s.InTx(ctx, func(tx *Store) error {
		tag, err := tx.q.Exec(ctx, `UPDATE tool_calls SET state = $2, finished_at = now(), exit_code = $3, output = $4,
				truncated = $5, is_error = $6, error = $7
			WHERE id = $1 AND state = 'running'`, id, o.State, o.ExitCode, nonNil(o.Output), o.Truncated, o.IsError, o.Error)
		if err != nil {
			return err
		}
		finished = tag.RowsAffected() == 1
		if !finished {
			return nil
		}
		return tx.Notify(ctx, chanCall, id.String())
	})
	if err != nil {
		return false, err
	}
	return finished, nil
}

// AbandonToolCall ends a sandbox's call key as not_run with the error
// reason, in one transaction that sends ferun_call with the id: only while
// the call is running, instanceID's, and not sent. false: the row is not
// such a call (another instance's, sent, ended, or absent), and nothing
// changed.
func (s *Store) AbandonToolCall(ctx context.Context, id podid.ID, key, instanceID, reason string) (bool, error) {
	var abandoned bool
	err := s.InTx(ctx, func(tx *Store) error {
		rows, err := tx.q.Query(ctx, `UPDATE tool_calls SET state = 'not_run', finished_at = now(), error = $4
			WHERE sandbox_id = $1 AND call_key = $2 AND instance_id = $3 AND state = 'running' AND NOT sent
			RETURNING id`, id, key, instanceID, reason)
		ids, err := tx.notifyEndedCalls(ctx, rows, err)
		abandoned = len(ids) == 1
		return err
	})
	if err != nil {
		return false, err
	}
	return abandoned, nil
}

// RenewToolCalls extends the leases of instanceID's running calls among ids
// to lease from now. It returns the ids it renewed: an id it leaves out is
// no longer the instance's running call (another writer ended it). It
// sends no notification.
func (s *Store) RenewToolCalls(ctx context.Context, instanceID string, ids []uuid.UUID, lease time.Duration) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.q.Query(ctx, `UPDATE tool_calls SET lease_until = now() + $3::interval
		WHERE id = ANY($2) AND instance_id = $1 AND state = 'running' RETURNING id`,
		instanceID, nonNil(ids), interval(lease))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// The errors of the writers that end another run's call: interrupted when
// the call was sent, not_run when it was not. They are SQL expressions over
// the row's instance_id.
const (
	expiredSentError = `'interrupted: instance ' || instance_id || ' that ran the call stopped renewing it before the call ` +
		`ended (it crashed, or lost its database); what the call did in the sandbox is unknown: look before repeating it'`
	expiredUnsentError = `'not run: instance ' || instance_id || ' that took the call stopped renewing it before the call ` +
		`reached the sandbox; repeat it with the same id'`
	claimedSentError = `'interrupted: instance ' || instance_id || ' started again; its earlier run had the call and ended ` +
		`without its stop; what the call did in the sandbox is unknown: look before repeating it'`
	claimedUnsentError = `'not run: instance ' || instance_id || ' started again before the call reached the sandbox; ` +
		`repeat it with the same id'`
)

// endCallSet is the SET of a writer that ends another run's call:
// interrupted with sentError when it was sent, not_run with unsentError
// when it was not.
func endCallSet(sentError, unsentError string) string {
	return `state = CASE WHEN sent THEN 'interrupted' ELSE 'not_run' END, finished_at = now(),
		error = CASE WHEN sent THEN ` + sentError + ` ELSE ` + unsentError + ` END`
}

// The SETs of the expiry of a lease (the leader, a waiter) and of a claim
// of the instance ID (ClaimInstance).
var (
	expireCallSet = endCallSet(expiredSentError, expiredUnsentError)
	claimCallSet  = endCallSet(claimedSentError, claimedUnsentError)
)

// ExpireToolCalls ends up to limit running calls whose lease ran out more
// than margin ago, oldest lease first, skipping rows locked by others:
// interrupted when sent ("interrupted: instance <id> that ran the call
// stopped renewing it …"), not_run when not ("not run: instance <id> that
// took the call stopped renewing it …"). It runs in one transaction that
// sends ferun_call for the ids, and returns them. The leader runs it in
// its fenced transaction with margin 0.
func (s *Store) ExpireToolCalls(ctx context.Context, margin time.Duration, limit int) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.InTx(ctx, func(tx *Store) error {
		rows, err := tx.q.Query(ctx, `UPDATE tool_calls SET `+expireCallSet+`
			WHERE id IN (SELECT id FROM tool_calls WHERE state = 'running' AND lease_until < now() - $1::interval
				ORDER BY lease_until LIMIT $2 FOR UPDATE SKIP LOCKED)
			RETURNING id`, interval(margin), limit)
		ids, err = tx.notifyEndedCalls(ctx, rows, err)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// ExpireToolCall ends one running call whose lease ran out more than
// margin ago, as ExpireToolCalls does, in one transaction that sends
// ferun_call with the id: a waiter's verdict on the call it waits on. It
// returns the state it wrote (ToolCallInterrupted or ToolCallNotRun), or
// "" when the call is not running or its lease ran out less than margin
// ago, and nothing changed.
func (s *Store) ExpireToolCall(ctx context.Context, id uuid.UUID, margin time.Duration) (string, error) {
	var state string
	err := s.InTx(ctx, func(tx *Store) error {
		state = ""
		err := tx.q.QueryRow(ctx, `UPDATE tool_calls SET `+expireCallSet+`
			WHERE id = $1 AND state = 'running' AND lease_until < now() - $2::interval RETURNING state`,
			id, interval(margin)).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return tx.notifyCalls(ctx, []uuid.UUID{id})
	})
	if err != nil {
		return "", err
	}
	return state, nil
}

// EndInstanceToolCalls ends every running call of instanceID, which an
// earlier run of the instance had when it ended without its stop
// (ClaimInstance): interrupted when sent ("interrupted: instance <id>
// started again; …"), not_run when not ("not run: instance <id> started
// again …"). It runs in one transaction that sends ferun_call for the ids,
// and returns how many it ended.
func (s *Store) EndInstanceToolCalls(ctx context.Context, instanceID string) (int64, error) {
	var n int64
	err := s.InTx(ctx, func(tx *Store) error {
		rows, err := tx.q.Query(ctx, `UPDATE tool_calls SET `+claimCallSet+`
			WHERE instance_id = $1 AND state = 'running' RETURNING id`, instanceID)
		ids, err := tx.notifyEndedCalls(ctx, rows, err)
		n = int64(len(ids))
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteFinishedToolCalls deletes up to limit ended calls (done,
// interrupted, not_run) that ended more than retention ago, oldest first
// (the leader: an hour). It returns how many. It sends no notification.
func (s *Store) DeleteFinishedToolCalls(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM tool_calls WHERE id IN (SELECT id FROM tool_calls
		WHERE state <> 'running' AND finished_at < now() - $1::interval ORDER BY finished_at LIMIT $2)`,
		interval(retention), limit)
	return tag.RowsAffected(), err
}

// notifyEndedCalls collects the ids an UPDATE … RETURNING id of tool_calls
// ended and sends ferun_call for them.
func (s *Store) notifyEndedCalls(ctx context.Context, rows pgx.Rows, err error) ([]uuid.UUID, error) {
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	return ids, s.notifyCalls(ctx, ids)
}
