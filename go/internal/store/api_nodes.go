// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file holds the typed queries of the REST API's views (internal/api)
// that no other domain file has.

// NodeCertExpiries returns the not_after of each of the given certificate
// serials that exists: the expiry of every node's current certificate in
// the nodes view, in one statement.
func (s *Store) NodeCertExpiries(ctx context.Context, serials []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if len(serials) == 0 {
		return out, nil
	}
	rows, err := s.q.Query(ctx, "SELECT serial, not_after FROM node_certs WHERE serial = ANY($1)", serials)
	if err != nil {
		return nil, err
	}
	var serial string
	var notAfter time.Time
	_, err = pgx.ForEachRow(rows, []any{&serial, &notAfter}, func() error {
		out[serial] = notAfter
		return nil
	})
	return out, err
}
