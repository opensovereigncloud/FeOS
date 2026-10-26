// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/podid"
)

// Hold is a row of sandbox_holds: something that keeps a sandbox awake,
// across instances.
type Hold struct {
	SandboxID  podid.ID
	Holder     string // InstanceHolder or ProcessHolder
	Kind       model.HoldKind
	InstanceID string    // exec, forward: the instance that renews it
	LeaseUntil time.Time // a process without a time limit: Infinite
	Infinite   bool
}

// InstanceHolder is the holder of an instance's exec or forward hold:
// "<instance>:<kind>".
func InstanceHolder(instanceID string, kind model.HoldKind) string {
	return instanceID + ":" + string(kind)
}

// ProcessHolder is the holder of a background process's hold:
// "process:<id>", the id from process_start's handle.
func ProcessHolder(processID string) string {
	return "process:" + processID
}

// ProcessIDOf returns the process id of a ProcessHolder, and false for
// another holder.
func ProcessIDOf(holder string) (string, bool) {
	return strings.CutPrefix(holder, "process:")
}

// upsertHold writes a hold whose lease ends lease from now, or never for
// lease <= 0; model.ErrNotFound when the sandbox does not exist.
func (s *Store) upsertHold(ctx context.Context, h Hold, lease time.Duration) error {
	_, err := s.q.Exec(ctx, `INSERT INTO sandbox_holds (sandbox_id, holder, kind, instance_id, lease_until)
		VALUES ($1, $2, $3, $4, CASE WHEN $5::boolean THEN 'infinity'::timestamptz ELSE now() + $6::interval END)
		ON CONFLICT (sandbox_id, holder) DO UPDATE SET kind = excluded.kind, instance_id = excluded.instance_id,
			lease_until = excluded.lease_until`,
		h.SandboxID, h.Holder, string(h.Kind), h.InstanceID, lease <= 0, interval(lease))
	if isForeignKeyViolation(err) {
		return model.ErrNotFound
	}
	return err
}

// AdmitHold writes the instance's hold of kind (exec or forward) on a
// sandbox for lease and sets last_active_at = now(): a hold count going
// from 0 to 1. The caller locked the row FOR NO KEY UPDATE and checked it
// runs without an op.
func (s *Store) AdmitHold(ctx context.Context, id podid.ID, kind model.HoldKind, instanceID string, lease time.Duration) error {
	if err := s.RenewHold(ctx, id, kind, instanceID, lease); err != nil {
		return err
	}
	return s.TouchSandbox(ctx, id)
}

// RenewHold rewrites the instance's hold of kind with a lease from now: an
// upsert, so that it re-inserts a hold the leader already expired.
func (s *Store) RenewHold(ctx context.Context, id podid.ID, kind model.HoldKind, instanceID string, lease time.Duration) error {
	return s.upsertHold(ctx, Hold{SandboxID: id, Holder: InstanceHolder(instanceID, kind), Kind: kind, InstanceID: instanceID}, lease)
}

// ReleaseHold deletes the instance's hold of kind and sets last_active_at
// = now(): a hold count going from 1 to 0. Run both in one transaction, so
// that the idle loop never sees the sandbox without its hold and with an
// old last_active_at.
func (s *Store) ReleaseHold(ctx context.Context, id podid.ID, kind model.HoldKind, instanceID string) error {
	if _, err := s.q.Exec(ctx, "DELETE FROM sandbox_holds WHERE sandbox_id = $1 AND holder = $2",
		id, InstanceHolder(instanceID, kind)); err != nil {
		return err
	}
	return s.TouchSandbox(ctx, id)
}

// HoldProcess writes the hold of a background process: lease from now, or
// without a limit for lease <= 0 (a process started with timeout 0).
func (s *Store) HoldProcess(ctx context.Context, id podid.ID, processID string, lease time.Duration) error {
	return s.upsertHold(ctx, Hold{SandboxID: id, Holder: ProcessHolder(processID), Kind: model.HoldProcess}, lease)
}

// ReleaseProcessHold deletes the hold of a background process. It reports
// whether there was one.
func (s *Store) ReleaseProcessHold(ctx context.Context, id podid.ID, processID string) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM sandbox_holds WHERE sandbox_id = $1 AND holder = $2", id, ProcessHolder(processID))
	return tag.RowsAffected() > 0, err
}

// DeleteProcessHolds deletes every process hold of a sandbox: a
// transition that ends its processes. It returns how many.
func (s *Store) DeleteProcessHolds(ctx context.Context, id podid.ID) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM sandbox_holds WHERE sandbox_id = $1 AND kind = 'process'", id)
	return tag.RowsAffected(), err
}

// HasLiveHold reports whether the sandbox has a hold whose lease lies
// ahead, of one of kinds (any kind when none is given).
func (s *Store) HasLiveHold(ctx context.Context, id podid.ID, kinds ...model.HoldKind) (bool, error) {
	ks := make([]string, len(kinds))
	for i, k := range kinds {
		ks[i] = string(k)
	}
	var live bool
	err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sandbox_holds WHERE sandbox_id = $1 AND lease_until > now()
		AND (cardinality($2::text[]) = 0 OR kind = ANY($2)))`, id, ks).Scan(&live)
	return live, err
}

func scanHold(row pgx.Row) (Hold, error) {
	var h Hold
	var kind string
	var until pgtype.Timestamptz
	if err := row.Scan(&h.SandboxID, &h.Holder, &kind, &h.InstanceID, &until); err != nil {
		return Hold{}, err
	}
	h.Kind = model.HoldKind(kind)
	h.LeaseUntil, h.Infinite = until.Time, until.InfinityModifier == pgtype.Infinity
	return h, nil
}

// ListHolds returns a sandbox's holds, live or not, ordered by holder.
func (s *Store) ListHolds(ctx context.Context, id podid.ID) ([]Hold, error) {
	rows, err := s.q.Query(ctx, `SELECT sandbox_id, holder, kind, instance_id, lease_until FROM sandbox_holds
		WHERE sandbox_id = $1 ORDER BY holder`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Hold, error) { return scanHold(r) })
}

// DeleteExpiredHolds deletes the holds whose lease has passed. It returns
// how many.
func (s *Store) DeleteExpiredHolds(ctx context.Context) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM sandbox_holds WHERE lease_until < now()")
	return tag.RowsAffected(), err
}

// ProcessHeld is a running sandbox kept awake only by process holds.
type ProcessHeld struct {
	Sandbox    Sandbox
	ProcessIDs []string
}

// ProcessHeldSandboxes returns up to limit running sandboxes without an op
// on one of nodes whose only live holds are process holds and whose
// last_active_at is older than idle, with the ids of those processes: the
// candidates whose ended processes the process-holds loop releases.
func (s *Store) ProcessHeldSandboxes(ctx context.Context, idle time.Duration, nodes []string, limit int) ([]ProcessHeld, error) {
	rows, err := s.q.Query(ctx, "SELECT "+sandboxColumns("s")+`,
			array(SELECT substr(h.holder, 9) FROM sandbox_holds h
				WHERE h.sandbox_id = s.id AND h.kind = 'process' AND h.lease_until > now() ORDER BY h.holder)
		FROM sandboxes s
		WHERE s.client_state = 'running' AND s.op = '' AND s.node = ANY($2) AND s.last_active_at < now() - $1::interval
			AND EXISTS (SELECT 1 FROM sandbox_holds h WHERE h.sandbox_id = s.id AND h.kind = 'process' AND h.lease_until > now())
			AND NOT EXISTS (SELECT 1 FROM sandbox_holds h WHERE h.sandbox_id = s.id AND h.kind <> 'process' AND h.lease_until > now())
		ORDER BY s.last_active_at LIMIT $3`, interval(idle), nonNil(nodes), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProcessHeld, error) {
		var p ProcessHeld
		var ids []string
		row := extraScan{r, []any{&ids}}
		sb, err := scanSandbox(row)
		p.Sandbox, p.ProcessIDs = sb, nonNil(ids)
		return p, err
	})
}

// extraScan scans a row whose first columns are read by another scan
// function and whose last ones go to extra.
type extraScan struct {
	row   pgx.Row
	extra []any
}

func (e extraScan) Scan(dest ...any) error {
	return e.row.Scan(append(dest, e.extra...)...)
}
