// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PlacementCandidate is one node's verdict in a placement decision.
type PlacementCandidate struct {
	Node     string  `json:"node"`
	Feasible bool    `json:"feasible"`
	Score    float64 `json:"score"`
	Reason   string  `json:"reason"`
}

// Placement is a row of placements: the audit of one placement decision.
type Placement struct {
	ID         int64
	PodID      uuid.UUID
	ChosenNode string
	Candidates []PlacementCandidate
	Trigger    string // the op's trigger
	InstanceID string
	At         time.Time
}

// InsertPlacement records a placement decision (at = now()).
func (s *Store) InsertPlacement(ctx context.Context, p Placement) (Placement, error) {
	cands, err := json.Marshal(nonNil(p.Candidates))
	if err != nil {
		return Placement{}, err
	}
	err = s.q.QueryRow(ctx, `INSERT INTO placements (pod_id, chosen_node, candidates, trigger, instance_id)
		VALUES ($1, $2, $3, $4, $5) RETURNING id, at`, p.PodID, p.ChosenNode, string(cands), p.Trigger, p.InstanceID).
		Scan(&p.ID, &p.At)
	return p, err
}

// ListPlacements returns the newest limit placement decisions of a pod,
// newest first.
func (s *Store) ListPlacements(ctx context.Context, podID uuid.UUID, limit int) ([]Placement, error) {
	rows, err := s.q.Query(ctx, `SELECT id, pod_id, chosen_node, candidates, trigger, instance_id, at FROM placements
		WHERE pod_id = $1 ORDER BY at DESC, id DESC LIMIT $2`, podID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Placement, error) {
		var p Placement
		var cands []byte
		if err := r.Scan(&p.ID, &p.PodID, &p.ChosenNode, &cands, &p.Trigger, &p.InstanceID, &p.At); err != nil {
			return Placement{}, err
		}
		return p, json.Unmarshal(cands, &p.Candidates)
	})
}
