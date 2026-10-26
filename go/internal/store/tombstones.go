// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ironcore-dev/feos/go/internal/podid"
)

// Tombstone is a row of pod_tombstones: delete the pod PodID from Node if
// it is of Incarnation (or has no incarnation label). While it exists,
// placement keeps the pod's ID off the node.
type Tombstone struct {
	PodID       podid.ID
	Node        string
	Incarnation uuid.UUID // uuid.Nil: a pod without the label
	Reason      string
	CreatedAt   time.Time
	// NotBefore: not processed before (a live op may still create the
	// pod).
	NotBefore     time.Time
	Attempts      int
	LastError     string
	LastAttemptAt time.Time
}

// TombstoneKey names a tombstone.
type TombstoneKey struct {
	PodID       podid.ID
	Node        string
	Incarnation uuid.UUID
}

// Key returns the tombstone's key.
func (t *Tombstone) Key() TombstoneKey {
	return TombstoneKey{PodID: t.PodID, Node: t.Node, Incarnation: t.Incarnation}
}

const tombstoneCols = "pod_id, node, incarnation, reason, created_at, not_before, attempts, last_error, last_attempt_at"

func scanTombstone(row pgx.Row) (Tombstone, error) {
	var t Tombstone
	var last *time.Time
	if err := row.Scan(&t.PodID, &t.Node, &t.Incarnation, &t.Reason, &t.CreatedAt, &t.NotBefore, &t.Attempts,
		&t.LastError, &last); err != nil {
		return Tombstone{}, err
	}
	t.LastAttemptAt = timeOf(last)
	return t, nil
}

func collectTombstones(rows pgx.Rows, err error) ([]Tombstone, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tombstone, error) { return scanTombstone(r) })
}

// UpsertTombstone writes a tombstone, in the transaction that decided the
// pod is to go. NotBefore zero means now(). An existing tombstone of the
// same key gets the new not_before (a fenced-out op refreshes its
// tombstone to now) and keeps its reason and attempts.
func (s *Store) UpsertTombstone(ctx context.Context, t Tombstone) (Tombstone, error) {
	return scanTombstone(s.q.QueryRow(ctx, `INSERT INTO pod_tombstones (pod_id, node, incarnation, reason, not_before)
		VALUES ($1, $2, $3, $4, coalesce($5, now()))
		ON CONFLICT (pod_id, node, incarnation) DO UPDATE SET not_before = excluded.not_before
		RETURNING `+tombstoneCols, t.PodID, t.Node, t.Incarnation, t.Reason, nullTime(t.NotBefore)))
}

// GetTombstone reads a tombstone; ErrNoRow if there is none.
func (s *Store) GetTombstone(ctx context.Context, k TombstoneKey) (Tombstone, error) {
	t, err := scanTombstone(s.q.QueryRow(ctx, "SELECT "+tombstoneCols+` FROM pod_tombstones
		WHERE pod_id = $1 AND node = $2 AND incarnation = $3`, k.PodID, k.Node, k.Incarnation))
	return t, noRow(err, ErrNoRow)
}

// DeleteTombstone removes a tombstone: the pod is gone, or the node holds
// another incarnation, or a rollback undoes the decision. It reports
// whether there was one.
func (s *Store) DeleteTombstone(ctx context.Context, k TombstoneKey) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM pod_tombstones WHERE pod_id = $1 AND node = $2 AND incarnation = $3",
		k.PodID, k.Node, k.Incarnation)
	return tag.RowsAffected() > 0, err
}

// TombstonesOfPod returns every tombstone of a pod ID, on any node.
func (s *Store) TombstonesOfPod(ctx context.Context, podID podid.ID) ([]Tombstone, error) {
	return collectTombstones(s.q.Query(ctx, "SELECT "+tombstoneCols+" FROM pod_tombstones WHERE pod_id = $1 ORDER BY node, incarnation", podID))
}

// TombstonedNodes returns the nodes a tombstone of podID keeps the pod off
// (placement's tombstone gate).
func (s *Store) TombstonedNodes(ctx context.Context, podID podid.ID) ([]string, error) {
	rows, err := s.q.Query(ctx, "SELECT DISTINCT node FROM pod_tombstones WHERE pod_id = $1 ORDER BY node", podID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// HasTombstone reports whether a tombstone of podID exists on node: the
// recheck of placement's tombstone gate under the node's lock.
func (s *Store) HasTombstone(ctx context.Context, podID podid.ID, node string) (bool, error) {
	var exists bool
	err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pod_tombstones WHERE pod_id = $1 AND node = $2)", podID, node).Scan(&exists)
	return exists, err
}

// DueTombstones returns up to limit tombstones on one of nodes whose
// not_before has passed, the oldest due first.
func (s *Store) DueTombstones(ctx context.Context, nodes []string, limit int) ([]Tombstone, error) {
	return collectTombstones(s.q.Query(ctx, "SELECT "+tombstoneCols+` FROM pod_tombstones
		WHERE node = ANY($1) AND not_before <= now() ORDER BY not_before, pod_id LIMIT $2`, nonNil(nodes), limit))
}

// NoteTombstoneAttempt records a failed processing: attempts + 1, the
// error, last_attempt_at = now().
func (s *Store) NoteTombstoneAttempt(ctx context.Context, k TombstoneKey, errText string) error {
	_, err := s.q.Exec(ctx, `UPDATE pod_tombstones SET attempts = attempts + 1, last_error = $4, last_attempt_at = now()
		WHERE pod_id = $1 AND node = $2 AND incarnation = $3`, k.PodID, k.Node, k.Incarnation, errText)
	return err
}

// DropRemovedNodeTombstones deletes up to limit tombstones on removed
// nodes, whose pods nobody can delete any more, and returns them.
func (s *Store) DropRemovedNodeTombstones(ctx context.Context, limit int) ([]Tombstone, error) {
	return collectTombstones(s.q.Query(ctx, `DELETE FROM pod_tombstones WHERE (pod_id, node, incarnation) IN (
			SELECT t.pod_id, t.node, t.incarnation FROM pod_tombstones t JOIN nodes n ON n.name = t.node
			WHERE n.status = 'removed' ORDER BY t.node, t.pod_id LIMIT $1 FOR UPDATE OF t SKIP LOCKED)
		RETURNING `+tombstoneCols, limit))
}
