// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// ExpireNodesBatch is node expiry in batches (§7): it marks down at most
// limit of the joined or ready nodes whose last telemetry (or, without
// any, their join) is older than ttl, in the order of their names: status
// down, down_since = now(), "heartbeat expired". It returns their names,
// sorted; fewer than limit means that no other expired node was free. A
// node whose row another transaction holds (its telemetry being written)
// is skipped rather than waited for. The leader's fenced transactions call
// it, so that each touches at most limit rows and its share lock on the
// lease row never holds the renew up for long.
func (s *Store) ExpireNodesBatch(ctx context.Context, ttl time.Duration, limit int) ([]string, error) {
	rows, err := s.q.Query(ctx, `UPDATE nodes n SET status = 'down', down_since = now(),
			status_description = 'heartbeat expired'
		WHERE n.name IN (
			SELECT e.name FROM nodes e
			WHERE e.status IN ('joined', 'ready')
				AND coalesce((SELECT t.last_seen FROM node_telemetry t WHERE t.node = e.name), e.joined_at) < now() - $1::interval
			ORDER BY e.name LIMIT $2
			FOR UPDATE OF e SKIP LOCKED)
		RETURNING n.name`, interval(ttl), limit)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	slices.Sort(names)
	return names, err
}
