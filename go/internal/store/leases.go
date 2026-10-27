// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// LeaderLease is the name of the leader's lease.
const LeaderLease = "leader"

// LeaseRow is a row of leases. The epoch is the fencing token: it grows by
// one on every acquisition, also by the holder of an expired lease.
type LeaseRow struct {
	Name       string
	Holder     string
	Epoch      int64
	AcquiredAt time.Time
	RenewedAt  time.Time
	ExpiresAt  time.Time
	// Valid: expires_at > now() at the read.
	Valid bool
}

// ReadLease reads a lease without a lock. found is false when the row does
// not exist.
func (s *Store) ReadLease(ctx context.Context, name string) (l LeaseRow, found bool, err error) {
	err = s.q.QueryRow(ctx, `SELECT name, holder, epoch, acquired_at, renewed_at, expires_at, expires_at > now()
		FROM leases WHERE name = $1`, name).
		Scan(&l.Name, &l.Holder, &l.Epoch, &l.AcquiredAt, &l.RenewedAt, &l.ExpiresAt, &l.Valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeaseRow{}, false, nil
	}
	if err != nil {
		return LeaseRow{}, false, err
	}
	return l, true, nil
}

// AcquireLease takes the lease for holder for ttl when there is no row or
// it has expired, with the epoch one above the last (1 for a new row). ok
// is false when someone else holds it.
func (s *Store) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (epoch int64, ok bool, err error) {
	err = s.q.QueryRow(ctx, `INSERT INTO leases AS l (name, holder, epoch, acquired_at, renewed_at, expires_at)
		VALUES ($1, $2, 1, now(), now(), now() + $3::interval)
		ON CONFLICT (name) DO UPDATE
			SET holder = excluded.holder, epoch = l.epoch + 1, acquired_at = now(),
				renewed_at = now(), expires_at = excluded.expires_at
			WHERE l.expires_at < now()
		RETURNING epoch`, name, holder, interval(ttl)).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return epoch, true, nil
}

// RenewLease extends the lease of holder in epoch to now() + ttl, if it
// has not expired. It waits at most 3 s for the row lock (SET LOCAL
// lock_timeout, in the Store's transaction or one of its own). ok is false
// when the lease is lost; the leader then steps down at once, as on an
// error.
func (s *Store) RenewLease(ctx context.Context, name, holder string, epoch int64, ttl time.Duration) (expiresAt time.Time, ok bool, err error) {
	err = s.local(ctx, func(tx *Store) error {
		if _, err := tx.q.Exec(ctx, "SET LOCAL lock_timeout = '3s'"); err != nil {
			return err
		}
		err := tx.q.QueryRow(ctx, `UPDATE leases SET renewed_at = now(), expires_at = now() + $4::interval
			WHERE name = $1 AND holder = $2 AND epoch = $3 AND expires_at > now() RETURNING expires_at`,
			name, holder, epoch, interval(ttl)).Scan(&expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		ok = err == nil
		return err
	})
	if err != nil {
		return time.Time{}, false, err
	}
	return expiresAt, ok, nil
}

// FenceLease checks, in the Store's transaction, that holder still holds
// the lease in epoch, and takes a share lock on its row so that no other
// acquisition commits before the transaction does. false means the caller
// is no longer the leader and must abort the transaction.
func (s *Store) FenceLease(ctx context.Context, name, holder string, epoch int64) (bool, error) {
	var one int
	err := s.q.QueryRow(ctx, `SELECT 1 FROM leases WHERE name = $1 AND holder = $2 AND epoch = $3
		AND expires_at > now() FOR SHARE`, name, holder, epoch).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ReleaseLease lets the lease of holder in epoch expire now. It reports
// whether the holder still had it.
func (s *Store) ReleaseLease(ctx context.Context, name, holder string, epoch int64) (bool, error) {
	tag, err := s.q.Exec(ctx, "UPDATE leases SET expires_at = now() WHERE name = $1 AND holder = $2 AND epoch = $3",
		name, holder, epoch)
	return tag.RowsAffected() > 0, err
}
