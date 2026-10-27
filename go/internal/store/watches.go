// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// Watch is a row of watches: a filesystem watch's path set, its lease, its
// owner (the instance that holds the guest stream and the replay ring) and
// the owner's last acknowledgement.
type Watch struct {
	ID        uuid.UUID
	SandboxID podid.ID
	Container string
	Paths     []string
	Gen       int64     // the path set's generation
	ExpiresAt time.Time // the lease
	Owner     string
	// OwnerUntil is the owner's lease, renewed by the owner.
	OwnerUntil time.Time
	// Epoch grows with every change of owner; event ids carry it.
	Epoch     int64
	Result    json.RawMessage // the owner's last ack; nil before the first
	ResultGen int64           // the path-set generation Result answers
	CreatedAt time.Time
	// OwnerLive: owner_until > now() at the read.
	OwnerLive bool
}

const watchCols = `id, sandbox_id, container, paths, gen, expires_at, owner, owner_until, epoch, result, result_gen, created_at,
	coalesce(owner_until > now(), false)`

func scanWatch(row pgx.Row) (Watch, error) {
	var w Watch
	var ownerUntil *time.Time
	var result []byte
	if err := row.Scan(&w.ID, &w.SandboxID, &w.Container, &w.Paths, &w.Gen, &w.ExpiresAt, &w.Owner, &ownerUntil,
		&w.Epoch, &result, &w.ResultGen, &w.CreatedAt, &w.OwnerLive); err != nil {
		return Watch{}, err
	}
	w.OwnerUntil = timeOf(ownerUntil)
	w.Paths = nonNil(w.Paths)
	if result != nil {
		w.Result = json.RawMessage(result)
	}
	return w, nil
}

func collectWatches(rows pgx.Rows, err error) ([]Watch, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Watch, error) { return scanWatch(r) })
}

// NewWatch is what a POST of a watch sets.
type NewWatch struct {
	ID        uuid.UUID
	SandboxID podid.ID
	Container string // "" = main
	Paths     []string
}

// CreateWatch inserts a watch owned by owner (epoch 1, path-set generation
// 1) with a lease of lease and an owner lease of ownerLease from now;
// model.ErrNotFound when the sandbox does not exist.
func (s *Store) CreateWatch(ctx context.Context, n NewWatch, owner string, lease, ownerLease time.Duration) (Watch, error) {
	container := n.Container
	if container == "" {
		container = "main"
	}
	w, err := scanWatch(s.q.QueryRow(ctx, `INSERT INTO watches (id, sandbox_id, container, paths, gen, expires_at, owner,
			owner_until, epoch)
		VALUES ($1, $2, $3, $4, 1, now() + $5::interval, $6, now() + $7::interval, 1) RETURNING `+watchCols,
		n.ID, n.SandboxID, container, nonNil(n.Paths), interval(lease), owner, interval(ownerLease)))
	if isForeignKeyViolation(err) {
		return Watch{}, model.ErrNotFound
	}
	return w, err
}

// GetWatch reads a watch; ErrNoRow if there is none.
func (s *Store) GetWatch(ctx context.Context, id uuid.UUID) (Watch, error) {
	return s.LockWatch(ctx, id, NoLock)
}

// LockWatch reads a watch and locks its row; ErrNoRow if there is none.
func (s *Store) LockWatch(ctx context.Context, id uuid.UUID, lock RowLock) (Watch, error) {
	w, err := scanWatch(s.q.QueryRow(ctx, "SELECT "+watchCols+" FROM watches WHERE id = $1 "+string(lock), id))
	return w, noRow(err, ErrNoRow)
}

// ListWatchesOfSandbox returns a sandbox's watches, oldest first.
func (s *Store) ListWatchesOfSandbox(ctx context.Context, sandboxID podid.ID) ([]Watch, error) {
	return collectWatches(s.q.Query(ctx, "SELECT "+watchCols+" FROM watches WHERE sandbox_id = $1 ORDER BY created_at, id", sandboxID))
}

// ListWatchesOwnedBy returns the watches owner holds, oldest first.
func (s *Store) ListWatchesOwnedBy(ctx context.Context, owner string) ([]Watch, error) {
	return collectWatches(s.q.Query(ctx, "SELECT "+watchCols+" FROM watches WHERE owner = $1 ORDER BY created_at, id", owner))
}

// SetWatchPaths replaces a watch's path set: gen + 1, the lease renewed
// for lease from now; ferun_watch tells the owner. ErrNoRow if the watch
// does not exist.
func (s *Store) SetWatchPaths(ctx context.Context, id uuid.UUID, paths []string, lease time.Duration) (Watch, error) {
	w, err := scanWatch(s.q.QueryRow(ctx, `UPDATE watches SET paths = $2, gen = gen + 1, expires_at = now() + $3::interval
		WHERE id = $1 RETURNING `+watchCols, id, nonNil(paths), interval(lease)))
	if err != nil {
		return Watch{}, noRow(err, ErrNoRow)
	}
	return w, s.Notify(ctx, chanWatch, id.String())
}

// TakeWatchOwnership makes owner the watch's owner: epoch + 1, an owner
// lease of ownerLease and a lease of lease from now; ferun_watch tells the
// old owner. ErrNoRow if the watch does not exist.
func (s *Store) TakeWatchOwnership(ctx context.Context, id uuid.UUID, owner string, lease, ownerLease time.Duration) (Watch, error) {
	w, err := scanWatch(s.q.QueryRow(ctx, `UPDATE watches SET owner = $2, epoch = epoch + 1,
			owner_until = now() + $4::interval, expires_at = greatest(expires_at, now() + $3::interval)
		WHERE id = $1 RETURNING `+watchCols, id, owner, interval(lease), interval(ownerLease)))
	if err != nil {
		return Watch{}, noRow(err, ErrNoRow)
	}
	return w, s.Notify(ctx, chanWatch, id.String())
}

// RenewWatchOwnership extends the owner leases of every watch owner holds
// to ownerLease from now. It returns how many.
func (s *Store) RenewWatchOwnership(ctx context.Context, owner string, ownerLease time.Duration) (int64, error) {
	tag, err := s.q.Exec(ctx, "UPDATE watches SET owner_until = now() + $2::interval WHERE owner = $1", owner, interval(ownerLease))
	return tag.RowsAffected(), err
}

// RenewWatchLease extends a watch's lease to lease from now (a PUT, a
// delivered keepalive, a stream opening or closing). It reports whether the
// watch exists.
func (s *Store) RenewWatchLease(ctx context.Context, id uuid.UUID, lease time.Duration) (bool, error) {
	tag, err := s.q.Exec(ctx, "UPDATE watches SET expires_at = greatest(expires_at, now() + $2::interval) WHERE id = $1",
		id, interval(lease))
	return tag.RowsAffected() > 0, err
}

// SetWatchResult stores the owner's ack of the path set of generation gen,
// when owner still owns the watch in epoch; ferun_watch tells the waiting
// PUTs. It reports whether it stored it.
func (s *Store) SetWatchResult(ctx context.Context, id uuid.UUID, owner string, epoch, gen int64, result json.RawMessage) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE watches SET result = $5, result_gen = $4
		WHERE id = $1 AND owner = $2 AND epoch = $3 AND result_gen <= $4`, id, owner, epoch, gen, nullJSON(result))
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	return true, s.Notify(ctx, chanWatch, id.String())
}

// DeleteWatch removes a watch; ferun_watch tells its owner. It reports
// whether there was one.
func (s *Store) DeleteWatch(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM watches WHERE id = $1", id)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	return true, s.Notify(ctx, chanWatch, id.String())
}

// DeleteExpiredWatches removes up to limit watches whose lease has passed
// and notifies their ids. It returns them.
func (s *Store) DeleteExpiredWatches(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, `DELETE FROM watches WHERE id IN (
			SELECT id FROM watches WHERE expires_at < now() ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING id`, limit)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := s.Notify(ctx, chanWatch, id.String()); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// CountWatches counts a sandbox's watches.
func (s *Store) CountWatches(ctx context.Context, sandboxID podid.ID) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, "SELECT count(*) FROM watches WHERE sandbox_id = $1", sandboxID).Scan(&n)
	return n, err
}
