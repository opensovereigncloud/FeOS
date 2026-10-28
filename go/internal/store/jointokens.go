// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// JoinToken is a row of join_tokens. Only the hash of what the node
// ("feos-join-<secret>") or the ingress ("feos-ingress-join-<secret>")
// sends is stored.
type JoinToken struct {
	Hash        string
	ID          uuid.UUID
	CreatedBy   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Kind        string    // model.JoinKindNode, model.JoinKindIngress: what it joins
	NodeName    string    // pinned name (of either kind); "" creates a new name only
	UsedAt      time.Time // zero: unused
	UsedName    string
	UsedKeyHash string
	UsedSerial  string
	UsedPeer    string
}

// Used reports whether the token was used.
func (t *JoinToken) Used() bool { return !t.UsedAt.IsZero() }

const joinTokenCols = `hash, id, created_by, created_at, expires_at, kind, node_name, used_at, used_name,
	used_key_hash, used_serial, used_peer`

func scanJoinToken(row pgx.Row) (JoinToken, error) {
	var t JoinToken
	var used *time.Time
	if err := row.Scan(&t.Hash, &t.ID, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.Kind, &t.NodeName, &used,
		&t.UsedName, &t.UsedKeyHash, &t.UsedSerial, &t.UsedPeer); err != nil {
		return JoinToken{}, err
	}
	t.UsedAt = timeOf(used)
	return t, nil
}

// CreateJoinToken stores a join token of kind (model.JoinKindNode or
// model.JoinKindIngress) by its hash, valid for ttl from now(), pinned to
// the node's or ingress's name unless name is "".
func (s *Store) CreateJoinToken(ctx context.Context, hash, createdBy, kind, name string, ttl time.Duration) (JoinToken, error) {
	return scanJoinToken(s.q.QueryRow(ctx, `INSERT INTO join_tokens (hash, created_by, kind, node_name, expires_at)
		VALUES ($1, $2, $3, $4, now() + $5::interval) RETURNING `+joinTokenCols, hash, createdBy, kind, name, interval(ttl)))
}

// LockJoinToken reads a join token by its hash and locks it FOR UPDATE;
// ErrNoRow if there is none.
func (s *Store) LockJoinToken(ctx context.Context, hash string) (JoinToken, error) {
	t, err := scanJoinToken(s.q.QueryRow(ctx, "SELECT "+joinTokenCols+" FROM join_tokens WHERE hash = $1 FOR UPDATE", hash))
	return t, noRow(err, ErrNoRow)
}

// MarkJoinTokenUsed records a token's first use: used_at = now(), the
// name, key hash, serial and peer of the join.
func (s *Store) MarkJoinTokenUsed(ctx context.Context, hash, name, keyHash, serial, peer string) error {
	tag, err := s.q.Exec(ctx, `UPDATE join_tokens SET used_at = now(), used_name = $2, used_key_hash = $3,
		used_serial = $4, used_peer = $5 WHERE hash = $1`, hash, name, keyHash, serial, peer)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNoRow
	}
	return err
}

// ListJoinTokens returns every join token, newest first.
func (s *Store) ListJoinTokens(ctx context.Context) ([]JoinToken, error) {
	rows, err := s.q.Query(ctx, "SELECT "+joinTokenCols+" FROM join_tokens ORDER BY created_at DESC, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (JoinToken, error) { return scanJoinToken(r) })
}

// DeleteJoinToken revokes a join token by its id. It reports whether there
// was one.
func (s *Store) DeleteJoinToken(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM join_tokens WHERE id = $1", id)
	return tag.RowsAffected() > 0, err
}

// DeleteStaleJoinTokens removes the tokens used or expired more than keep
// ago. It returns how many.
func (s *Store) DeleteStaleJoinTokens(ctx context.Context, keep time.Duration) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM join_tokens WHERE coalesce(used_at, expires_at) < now() - $1::interval", interval(keep))
	return tag.RowsAffected(), err
}
