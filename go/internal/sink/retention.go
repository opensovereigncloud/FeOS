// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
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
// and the database's clock): the hot batches whose newest line is, then
// the streams with nothing left in the database (the index drops their
// lines by its own retention, at least as long: ADR 0026 §10; a read finds
// no line of a stream that is gone), the usage of days before yesterday,
// and the hot stage's empty partitions of past days, after making those
// ahead. Several instances may sweep at once.
func (s *Store) Sweep(ctx context.Context) error {
	def, maxRet := s.cfg.Retention.Nanoseconds(), s.cfg.RetentionMax.Nanoseconds()
	var errs []error
	// A batch goes once its newest line is past retention.
	if _, err := s.db.Pool.Exec(ctx, `WITH d AS (DELETE FROM log_batches b USING log_streams s
			WHERE b.stream_id = s.id AND b.max_ts < `+nowNs+` - `+retentionNs+`
			RETURNING b.stream_id, b.lines, b.bytes),
		g AS (SELECT stream_id, sum(lines) AS n, sum(bytes) AS b FROM d GROUP BY stream_id)
		UPDATE log_streams s SET hot_lines = greatest(s.hot_lines - g.n, 0), hot_bytes = greatest(s.hot_bytes - g.b, 0)
		FROM g WHERE s.id = g.stream_id`, def, maxRet); err != nil {
		errs = append(errs, fmt.Errorf("expired hot batches: %w", err))
	}
	// Transitional: the lines of log_lines the instances of before wrote.
	if _, err := s.db.Pool.Exec(ctx, `WITH d AS (DELETE FROM log_lines l USING log_streams s
			WHERE l.stream_id = s.id AND l.ts < `+nowNs+` - `+retentionNs+`
			RETURNING l.stream_id, octet_length(l.text) AS b),
		g AS (SELECT stream_id, count(*) AS n, sum(b) AS b FROM d GROUP BY stream_id)
		UPDATE log_streams s SET hot_lines = greatest(s.hot_lines - g.n, 0), hot_bytes = greatest(s.hot_bytes - g.b, 0)
		FROM g WHERE s.id = g.stream_id`, def, maxRet); err != nil {
		errs = append(errs, fmt.Errorf("expired hot lines: %w", err))
	}
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM log_streams s
		WHERE s.hot_lines = 0 AND s.chunk_owner IS NULL
			AND NOT EXISTS (SELECT 1 FROM log_batches b WHERE b.stream_id = s.id)
			AND NOT EXISTS (SELECT 1 FROM log_lines l WHERE l.stream_id = s.id)
			AND coalesce(s.last_ns, (extract(epoch FROM s.created_at) * 1000000000)::bigint) < `+nowNs+` - `+retentionNs,
		def, maxRet)
	if err != nil {
		errs = append(errs, fmt.Errorf("expired streams: %w", err))
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

// DeleteStreams deletes a tenant's streams of an environment or a job:
// it leaves a delete task of them in every log index (Quickwit removes
// their lines when it rewrites their splits; until then no read finds
// them, their streams being gone), then deletes the streams with their
// hot lines, each under its lock, which keeps the cold stage from moving
// its mark meanwhile. It returns how many streams went.
func (s *Store) DeleteStreams(ctx context.Context, tenant, environment, job string) (int, error) {
	sel := Selector{Environment: environment, Job: job}
	streams, err := selectStreams(ctx, s.db.Pool, tenant, streamFilter{sel: sel, limit: maxStreamsRead})
	if err != nil {
		return 0, err
	}
	if len(streams) > 0 {
		ids, err := s.index.Indexes(ctx, s.cfg.IndexPrefix+"-")
		if err != nil {
			return 0, fmt.Errorf("index: %w", err)
		}
		for _, id := range ids {
			if err := s.index.Delete(ctx, id, quickwit.Query{Tenant: tenant, Environment: environment, Job: job}); err != nil {
				return 0, fmt.Errorf("index %s: delete: %w", id, err)
			}
		}
	}
	gone := 0
	for _, st := range streams {
		tag, err := s.db.Pool.Exec(ctx, `DELETE FROM log_streams WHERE id = $1 AND tenant = $2`, st.ID, tenant)
		if err != nil {
			return gone, err
		}
		gone += int(tag.RowsAffected())
	}
	return gone, nil
}
