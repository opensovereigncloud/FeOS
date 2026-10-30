// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
)

// The sweep's bounds: the chunks one round deletes, and the objects
// deleted at once.
const (
	sweepBatch     = 1000
	deleteParallel = 8
)

// retentionNs is a stream's retention in SQL, in ns: its
// ferun.retention_days or $1, at most $2.
const retentionNs = `least(coalesce(s.retention_days::bigint * 86400000000000, $1::bigint), $2::bigint)`

// nowNs is the database's clock in Unix ns.
const nowNs = `(extract(epoch FROM now()) * 1000000000)::bigint`

// RunSweep runs the sweep every SweepInterval, the first at once, until
// ctx ends.
func (s *Store) RunSweep(ctx context.Context) error {
	for {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("retention sweep", "err", err)
		}
		t := time.NewTimer(s.cfg.SweepInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// Sweep deletes what is past its stream's retention (by the lines' times
// and the database's clock): chunks, each object first and its row after;
// hot lines; trace rows; then the streams with nothing left, the usage of
// days before yesterday, and the hot stage's empty partitions of past
// days, after making those ahead. Several instances may sweep at once.
func (s *Store) Sweep(ctx context.Context) error {
	def, maxRet := s.cfg.Retention.Nanoseconds(), s.cfg.RetentionMax.Nanoseconds()
	var errs []error
	for {
		rows, err := s.db.Pool.Query(ctx, `SELECT c.id, c.key FROM log_chunks c JOIN log_streams s ON s.id = c.stream_id
			WHERE c.last_ns < `+nowNs+` - `+retentionNs+` ORDER BY c.last_ns LIMIT $3`, def, maxRet, sweepBatch)
		if err != nil {
			return fmt.Errorf("expired chunks: %w", err)
		}
		type ck struct {
			id  int64
			key string
		}
		cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ck, error) {
			var c ck
			return c, r.Scan(&c.id, &c.key)
		})
		if err != nil {
			return fmt.Errorf("expired chunks: %w", err)
		}
		ids := make([]int64, len(cs))
		keys := make([]string, len(cs))
		for i, c := range cs {
			ids[i], keys[i] = c.id, c.key
		}
		n, err := s.deleteChunks(ctx, ids, keys)
		if err != nil {
			errs = append(errs, err)
		}
		if n > 0 {
			s.log.Info("deleted expired chunks", "chunks", n)
		}
		if err != nil || len(cs) < sweepBatch {
			break
		}
	}
	if _, err := s.db.Pool.Exec(ctx, `WITH d AS (DELETE FROM log_lines l USING log_streams s
			WHERE l.stream_id = s.id AND l.ts < `+nowNs+` - `+retentionNs+`
			RETURNING l.stream_id, octet_length(l.text) AS b),
		g AS (SELECT stream_id, count(*) AS n, sum(b) AS b FROM d GROUP BY stream_id)
		UPDATE log_streams s SET hot_lines = greatest(s.hot_lines - g.n, 0), hot_bytes = greatest(s.hot_bytes - g.b, 0)
		FROM g WHERE s.id = g.stream_id`, def, maxRet); err != nil {
		errs = append(errs, fmt.Errorf("expired hot lines: %w", err))
	}
	if _, err := s.db.Pool.Exec(ctx, `DELETE FROM log_traces t USING log_streams s
		WHERE t.stream_id = s.id AND t.ts < `+nowNs+` - `+retentionNs, def, maxRet); err != nil {
		errs = append(errs, fmt.Errorf("expired traces: %w", err))
	}
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM log_streams s
		WHERE s.hot_lines = 0 AND s.chunk_owner IS NULL
			AND NOT EXISTS (SELECT 1 FROM log_chunks c WHERE c.stream_id = s.id)
			AND NOT EXISTS (SELECT 1 FROM log_lines l WHERE l.stream_id = s.id)
			AND coalesce(s.last_ns, (extract(epoch FROM s.created_at) * 1000000000)::bigint) < `+nowNs+` - `+retentionNs,
		def, maxRet)
	if err != nil {
		errs = append(errs, fmt.Errorf("empty streams: %w", err))
	} else if tag.RowsAffected() > 0 {
		s.log.Info("deleted expired streams", "streams", tag.RowsAffected())
	}
	if _, err := s.db.Pool.Exec(ctx, `DELETE FROM log_usage WHERE day < (now() AT TIME ZONE 'utc')::date - 1`); err != nil {
		errs = append(errs, fmt.Errorf("usage: %w", err))
	}
	dropped, err := maintainPartitions(ctx, s.db)
	if len(dropped) > 0 {
		s.log.Info("dropped empty partitions", "partitions", dropped)
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("partitions: %w", err))
	}
	return errors.Join(errs...)
}

// deleteChunks deletes chunks: each object, then the rows of those whose
// object went. It returns how many rows it deleted.
func (s *Store) deleteChunks(ctx context.Context, ids []int64, keys []string) (int, error) {
	okIDs := make([]int64, 0, len(ids))
	ok := make([]bool, len(ids))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(deleteParallel)
	for i := range keys {
		g.Go(func() error {
			if err := s.bucket.Delete(gctx, keys[i]); err != nil {
				return err
			}
			ok[i] = true
			return nil
		})
	}
	err := g.Wait()
	for i, id := range ids {
		if ok[i] {
			okIDs = append(okIDs, id)
		}
	}
	if len(okIDs) > 0 {
		if _, derr := s.db.Pool.Exec(ctx, `DELETE FROM log_chunks WHERE id = ANY($1)`, okIDs); derr != nil {
			return 0, errors.Join(err, fmt.Errorf("delete chunk rows: %w", derr))
		}
	}
	if err != nil {
		return len(okIDs), fmt.Errorf("delete chunk objects: %w", err)
	}
	return len(okIDs), nil
}

// DeleteStreams deletes a tenant's streams of an environment or a job:
// their chunks (objects, then rows), then the streams with their lines and
// trace rows. It returns how many streams went.
func (s *Store) DeleteStreams(ctx context.Context, tenant, environment, job string) (int, error) {
	sel := Selector{Environment: environment, Job: job}
	streams, err := selectStreams(ctx, s.db.Pool, tenant, streamFilter{sel: sel, limit: maxStreamsRead})
	if err != nil {
		return 0, err
	}
	gone := 0
	for _, st := range streams {
		// A chunk made while its stream goes is deleted by its maker;
		// one recorded meanwhile keeps the stream (no cascade), and the
		// next round takes it.
		for range 3 {
			rows, err := s.db.Pool.Query(ctx, `SELECT id, key FROM log_chunks WHERE stream_id = $1`, st.ID)
			if err != nil {
				return gone, err
			}
			var ids []int64
			var keys []string
			for rows.Next() {
				var id int64
				var key string
				if err := rows.Scan(&id, &key); err != nil {
					rows.Close()
					return gone, err
				}
				ids, keys = append(ids, id), append(keys, key)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return gone, err
			}
			if _, err := s.deleteChunks(ctx, ids, keys); err != nil {
				return gone, err
			}
			// The stream's lock keeps the cold stage from recording a
			// chunk until it is gone; then it finds it gone.
			deleted := false
			err = s.db.Tx(ctx, func(tx pgx.Tx) error {
				deleted = false
				var one int
				err := tx.QueryRow(ctx, `SELECT 1 FROM log_streams WHERE id = $1 AND tenant = $2 FOR UPDATE`, st.ID, tenant).Scan(&one)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				if err != nil {
					return err
				}
				// A statement of its own, after the lock: it sees a chunk
				// committed while it waited.
				var chunks bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM log_chunks WHERE stream_id = $1)`, st.ID).Scan(&chunks); err != nil || chunks {
					return err
				}
				if _, err := tx.Exec(ctx, `DELETE FROM log_streams WHERE id = $1`, st.ID); err != nil {
					return err
				}
				deleted = true
				return nil
			})
			if err != nil {
				return gone, err
			}
			if deleted {
				gone++
				break
			}
		}
	}
	return gone, nil
}
