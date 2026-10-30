// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// This file holds the typed queries of the sandbox engine (internal/sandbox)
// that the other domains' files do not have.

// IdleRecheck is the recheck of an idle pause's begin (§9 begin step 3), a
// statement of its own with a fresh snapshot: the sandbox runs without an
// op, idles at all and was last active more than its idle interval ago
// (its idle_after_s, else idle; 0 never), no failed idle pause backs it
// off, the ingress's fence does not keep it awake (ingressFence), and no
// attached service keeps it running (attachedAwake). A sandbox that does
// not exist fails it.
func (s *Store) IdleRecheck(ctx context.Context, id podid.ID, idle time.Duration) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, `SELECT client_state = 'running' AND op = '' AND `+idleOn(2)+`
			AND last_active_at < now() - `+idleAfter(2)+` AND `+policyDue(PolicyIdle)+` AND `+ingressFence+`
			AND NOT `+attachedAwake+`
		FROM sandboxes s WHERE id = $1`, id, interval(idle)).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// HibernateAfterRecheck is the recheck of a hibernate-after begin: the
// sandbox is paused without an op since more than after ago, and no
// failed hibernate-after backs it off.
func (s *Store) HibernateAfterRecheck(ctx context.Context, id podid.ID, after time.Duration) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, `SELECT client_state = 'paused' AND op = '' AND paused_at IS NOT NULL
			AND paused_at < now() - $2::interval AND `+policyDue(PolicyHibernateAfter)+`
		FROM sandboxes s WHERE id = $1`, id, interval(after)).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// RepairRecheck is the recheck of a repair's begin by the repair loop or
// Ensure (§9 "Repair"), a statement of its own with a fresh snapshot: the
// sandbox, without an op, is still stuck, creating with its pod PENDING
// since more than pendingAfter, or running with its guest unreachable
// since more than unreachableAfter. A sandbox that does not exist fails it.
func (s *Store) RepairRecheck(ctx context.Context, id podid.ID, pendingAfter, unreachableAfter time.Duration) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, `SELECT op = '' AND coalesce(
			(client_state = 'creating' AND pending_since < now() - $2::interval)
			OR (client_state = 'running' AND unreachable_since < now() - $3::interval), false)
		FROM sandboxes WHERE id = $1`, id, interval(pendingAfter), interval(unreachableAfter)).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// OpCursor is a place in the order of OpsOf: past the op of the sandbox
// ID, whose deadline is Deadline. The zero OpCursor is the start.
type OpCursor struct {
	Deadline time.Time
	ID       podid.ID
}

// Next is the cursor past the last of a page of OpsOf.
func (c OpCursor) Next(page []Sandbox) OpCursor {
	if len(page) == 0 {
		return c
	}
	last := page[len(page)-1]
	return OpCursor{Deadline: last.OpDeadline, ID: last.ID}
}

// OpsOf returns up to limit sandboxes with an op owned by instanceID whose
// op ID is none of except, past after, by op deadline and then ID.
func (s *Store) OpsOf(ctx context.Context, instanceID string, except []uuid.UUID, after OpCursor,
	limit int) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxCols+` FROM sandboxes
		WHERE op_owner = $1 AND op <> '' AND op_id <> ALL($2)
			AND ($3::timestamptz IS NULL OR (op_deadline, id) > ($3::timestamptz, $4::text))
		ORDER BY op_deadline, id LIMIT $5`,
		instanceID, nonNil(except), nullTime(after.Deadline), after.ID, limit))
}

// OlderThan reports whether t lies more than d before the database's now()
// (within a transaction, its start). A zero t is not older than anything.
func (s *Store) OlderThan(ctx context.Context, t time.Time, d time.Duration) (bool, error) {
	if t.IsZero() {
		return false, nil
	}
	var older bool
	err := s.q.QueryRow(ctx, "SELECT $1::timestamptz < now() - $2::interval", t, interval(d)).Scan(&older)
	return older, err
}

// DueTombstone reads a tombstone and whether its not_before has passed on
// the database clock; ErrNoRow if there is none.
func (s *Store) DueTombstone(ctx context.Context, k TombstoneKey) (Tombstone, bool, error) {
	var due bool
	t, err := scanTombstone(extraScan{s.q.QueryRow(ctx, "SELECT "+tombstoneCols+`, not_before <= now() FROM pod_tombstones
		WHERE pod_id = $1 AND node = $2 AND incarnation = $3`, k.PodID, k.Node, k.Incarnation), []any{&due}})
	if err != nil {
		return Tombstone{}, false, noRow(err, ErrNoRow)
	}
	return t, due, nil
}

// ClearRunIDs ends the open runs of the given sandboxes in the registry
// (an empty run_id): a write of the run span bookkeeping, no lifecycle
// write.
func (s *Store) ClearRunIDs(ctx context.Context, ids []podid.ID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.q.Exec(ctx, "UPDATE sandboxes SET run_id = '' WHERE id = ANY($1) AND run_id <> ''", ids)
	return err
}

// DrainingNode is a node being drained, as the drain loop sees it.
type DrainingNode struct {
	Node Node
	// Overdue: the drain began more than the drain timeout ago.
	Overdue bool
}

// DrainingNodes returns the nodes that are draining and not removed,
// ordered by name, each with whether its drain began more than timeout
// ago (on the database clock).
func (s *Store) DrainingNodes(ctx context.Context, timeout time.Duration) ([]DrainingNode, error) {
	rows, err := s.q.Query(ctx, "SELECT "+nodeCols+`,
			drain_started_at IS NOT NULL AND drain_started_at < now() - $1::interval
		FROM nodes WHERE draining AND status <> 'removed' ORDER BY name`, interval(timeout))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DrainingNode, error) {
		var dn DrainingNode
		targets, finish := nodeScanTargets(&dn.Node)
		if err := r.Scan(append(targets, &dn.Overdue)...); err != nil {
			return DrainingNode{}, err
		}
		finish()
		return dn, nil
	})
}

// CountUnevacuated counts the sandboxes whose node is node and that are
// not yet off it: any state but hibernated, lost and pending, or an op in
// flight. A drained node has none.
func (s *Store) CountUnevacuated(ctx context.Context, node string) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, `SELECT count(*) FROM sandboxes WHERE node = $1
		AND (client_state NOT IN ('hibernated', 'lost', 'pending') OR op <> '')`, node).Scan(&n)
	return n, err
}

// SetNodeDescriptionIfDraining writes a node's status description while it
// is draining, so that an undrain in between keeps its own.
func (s *Store) SetNodeDescriptionIfDraining(ctx context.Context, name, description string) error {
	_, err := s.q.Exec(ctx, `UPDATE nodes SET status_description = $2
		WHERE name = $1 AND draining AND status_description <> $2`, name, description)
	return err
}
