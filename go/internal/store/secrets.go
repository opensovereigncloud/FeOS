// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ClusterSecret reads a secret shared by every instance (db.SecretMCPSession
// keys MCP session ids); ErrNoRow if it does not exist.
func (s *Store) ClusterSecret(ctx context.Context, name string) ([]byte, error) {
	var v []byte
	err := s.q.QueryRow(ctx, "SELECT value FROM cluster_secrets WHERE name = $1", name).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoRow
	}
	return v, err
}
