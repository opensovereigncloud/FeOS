// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SandboxesElsewhere returns those of ids whose sandbox row exists but
// names neither node as its node nor as its op's target: pods a node
// reported in its events that the registry places on another node, or on
// none. Fleet requests a reconcile of the reporting node for them (§8).
func (s *Store) SandboxesElsewhere(ctx context.Context, node string, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.q.Query(ctx, `SELECT id FROM sandboxes WHERE id = ANY($1::uuid[])
		AND node IS DISTINCT FROM $2 AND op_target <> $2 ORDER BY id`, ids, node)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// podCredentialsLock is the first key of the advisory locks that
// LockPodCredentials takes ("FERC"). The two-key form has a key space of its
// own, apart from the one-key locks of migrations and partitions.
const podCredentialsLock int32 = 0x46455243

// LockPodCredentials serialises the credential requests of one pod until
// the caller's transaction ends, across instances: counting the pod's
// certificates within the rate limit's window and inserting a new one run
// one transaction at a time per pod, so that concurrent requests never
// exceed the limit. It takes a transaction-scoped advisory lock instead of
// a stronger lock on the sandbox row, which the request only reads FOR
// SHARE, so that it never holds up the row's other readers.
func (s *Store) LockPodCredentials(ctx context.Context, podID uuid.UUID) error {
	_, err := s.q.Exec(ctx, "SELECT pg_advisory_xact_lock($1, hashtext($2::uuid::text))", podCredentialsLock, podID)
	return err
}
