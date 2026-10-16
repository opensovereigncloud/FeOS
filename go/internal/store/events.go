// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// NodeStart is a row of node_starts: one start of feosd, whose row is the
// lock of its event ingest.
type NodeStart struct {
	Node        string
	StartID     uuid.UUID
	FirstSeen   time.Time
	LastSeq     int64 // the highest sequence number stored
	LastEventAt time.Time
	Dropped     int64 // the node's count of dropped events, as last reported
}

// LockNodeStart inserts the start's row if missing and locks it FOR
// UPDATE: the ingest of one start's events is serialised by it.
func (s *Store) LockNodeStart(ctx context.Context, node string, startID uuid.UUID) (NodeStart, error) {
	if _, err := s.q.Exec(ctx, "INSERT INTO node_starts (node, start_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		node, startID); err != nil {
		return NodeStart{}, err
	}
	var ns NodeStart
	var last *time.Time
	err := s.q.QueryRow(ctx, `SELECT node, start_id, first_seen, last_seq, last_event_at, dropped FROM node_starts
		WHERE node = $1 AND start_id = $2 FOR UPDATE`, node, startID).
		Scan(&ns.Node, &ns.StartID, &ns.FirstSeen, &ns.LastSeq, &last, &ns.Dropped)
	ns.LastEventAt = timeOf(last)
	return ns, err
}

// AdvanceNodeStart records an ingested batch: last_seq, dropped =
// greatest(dropped, the batch's), last_event_at = now().
func (s *Store) AdvanceNodeStart(ctx context.Context, node string, startID uuid.UUID, lastSeq, dropped int64) error {
	_, err := s.q.Exec(ctx, `UPDATE node_starts SET last_seq = $3, dropped = greatest(dropped, $4), last_event_at = now()
		WHERE node = $1 AND start_id = $2`, node, startID, lastSeq, dropped)
	return err
}

// DeleteOldNodeStarts removes the starts whose last event (or first sight,
// without events) is older than keep, never a node's current start. It
// returns how many.
func (s *Store) DeleteOldNodeStarts(ctx context.Context, keep time.Duration) (int64, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM node_starts s WHERE coalesce(s.last_event_at, s.first_seen) < now() - $1::interval
		AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.name = s.node AND n.start_id = s.start_id)`, interval(keep))
	return tag.RowsAffected(), err
}

// EventGap is a row of event_gaps: sequence numbers of a start that were
// neither stored nor counted as dropped by the node.
type EventGap struct {
	Node       string
	StartID    uuid.UUID
	FromSeq    int64
	ToSeq      int64
	DetectedAt time.Time
}

// InsertEventGap records a gap [from, to] of a start (detected_at =
// now()); a gap already recorded from the same number is kept.
func (s *Store) InsertEventGap(ctx context.Context, node string, startID uuid.UUID, from, to int64) error {
	_, err := s.q.Exec(ctx, `INSERT INTO event_gaps (node, start_id, from_seq, to_seq) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, node, startID, from, to)
	return err
}

// NodeStartKey names a start of a node.
type NodeStartKey struct {
	Node    string
	StartID uuid.UUID
}

// EventGaps returns the gaps of the given starts detected within [from,
// to], ordered by detection time.
func (s *Store) EventGaps(ctx context.Context, starts []NodeStartKey, from, to time.Time) ([]EventGap, error) {
	if len(starts) == 0 {
		return []EventGap{}, nil
	}
	nodes := make([]string, len(starts))
	ids := make([]uuid.UUID, len(starts))
	for i, k := range starts {
		nodes[i], ids[i] = k.Node, k.StartID
	}
	rows, err := s.q.Query(ctx, `SELECT g.node, g.start_id, g.from_seq, g.to_seq, g.detected_at
		FROM event_gaps g JOIN unnest($1::text[], $2::uuid[]) AS k(node, start_id)
			ON k.node = g.node AND k.start_id = g.start_id
		WHERE g.detected_at BETWEEN $3 AND $4 ORDER BY g.detected_at, g.node, g.from_seq`, nodes, ids, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EventGap, error) {
		var g EventGap
		err := r.Scan(&g.Node, &g.StartID, &g.FromSeq, &g.ToSeq, &g.DetectedAt)
		return g, err
	})
}

// DeleteOldEventGaps removes gaps detected more than keep ago. It returns
// how many.
func (s *Store) DeleteOldEventGaps(ctx context.Context, keep time.Duration) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM event_gaps WHERE detected_at < now() - $1::interval", interval(keep))
	return tag.RowsAffected(), err
}
