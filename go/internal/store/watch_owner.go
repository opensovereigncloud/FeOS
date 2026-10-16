// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OwnedWatch names a watch an owner holds in an epoch.
type OwnedWatch struct {
	ID    uuid.UUID
	Epoch int64
}

// RenewOwnedWatches extends owner's lease, to ownerLease from now, on each
// of the watches it holds in the epoch it names, and returns the ids it
// renewed. A watch that is not renewed is no longer owner's in that epoch,
// or no longer exists: the owner lets go of it. Renewing by id, rather
// than every row naming the owner, keeps a row the owner has let go of
// from looking owned.
func (s *Store) RenewOwnedWatches(ctx context.Context, owner string, held []OwnedWatch,
	ownerLease time.Duration) (map[uuid.UUID]bool, error) {
	if len(held) == 0 {
		return map[uuid.UUID]bool{}, nil
	}
	ids := make([]uuid.UUID, len(held))
	epochs := make([]int64, len(held))
	for i, h := range held {
		ids[i], epochs[i] = h.ID, h.Epoch
	}
	rows, err := s.q.Query(ctx, `UPDATE watches w SET owner_until = now() + $4::interval
		FROM unnest($2::uuid[], $3::bigint[]) AS h(id, epoch)
		WHERE w.id = h.id AND w.epoch = h.epoch AND w.owner = $1
		RETURNING w.id`, owner, ids, epochs, interval(ownerLease))
	if err != nil {
		return nil, err
	}
	renewed, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(renewed))
	for _, id := range renewed {
		out[id] = true
	}
	return out, nil
}

// ReleaseWatchOwnership ends owner's lease on every watch it holds, so
// that another instance takes them over at the next request instead of
// waiting for the lease: a stopping instance's last word on its watches.
// It returns how many.
func (s *Store) ReleaseWatchOwnership(ctx context.Context, owner string) (int64, error) {
	tag, err := s.q.Exec(ctx, "UPDATE watches SET owner_until = now() WHERE owner = $1 AND owner_until > now()", owner)
	return tag.RowsAffected(), err
}
