// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// Index is the cold stage (ADR 0026 §10): Quickwit (quickwit.Client) or a
// test's. Ingest answers once the documents are published; Search reads
// the indexes a pattern names.
type Index interface {
	EnsureIndex(ctx context.Context, id string, settings quickwit.IndexSettings) error
	Indexes(ctx context.Context, prefix string) ([]string, error)
	Ingest(ctx context.Context, id string, docs []quickwit.Doc) error
	Search(ctx context.Context, pattern string, q quickwit.Query) ([]quickwit.Doc, error)
	Delete(ctx context.Context, id string, q quickwit.Query) error
}

var _ Index = (*quickwit.Client)(nil)

// The cold stage's bounds: one round sends at most indexBatchBytes of
// text (Quickwit takes 10 MiB a request), a claim lasts claimLease (more
// than a publish takes), one round claims at most claimBatch streams, and
// indexWorkers rounds run at once in an instance.
const (
	indexBatchBytes = 6 << 20
	claimLease      = 3 * time.Minute
	claimBatch      = 32
	indexWorkers    = 4
	// indexBackoff is how long the streams of a round the index failed
	// wait.
	indexBackoff = 30 * time.Second
)

// indexID is the index of a retention in days, of the current mapping:
// <prefix>-v<version>-<days>d.
func (s *Store) indexID(days int) string { return quickwit.IndexID(s.cfg.IndexPrefix, days) }

// indexPattern names every log index of the sink, of every mapping
// version (<prefix>-<days>d is version 1).
func (s *Store) indexPattern() string { return s.cfg.IndexPrefix + "-*" }

func durationDays(d time.Duration) int { return int((d + 24*time.Hour - 1) / (24 * time.Hour)) }

// indexOf is the index of a stream: the default retention's, or the
// longest's for a stream that asks for more than the default (ADR 0026
// §10, an index per retention).
func (s *Store) indexOf(retentionDays *int) (string, int) {
	def, longest := durationDays(s.cfg.Retention), durationDays(s.cfg.RetentionMax)
	if retentionDays != nil && *retentionDays > def {
		return s.indexID(longest), longest
	}
	return s.indexID(def), def
}

// ensured remembers the indexes this process made sure of.
type ensured struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (s *Store) ensure(ctx context.Context, id string, days int) error {
	s.ensured.mu.Lock()
	ok := s.ensured.ids[id]
	s.ensured.mu.Unlock()
	if ok {
		return nil
	}
	settings := quickwit.IndexSettings{RetentionDays: days, CommitSecs: int(s.cfg.CommitTimeout / time.Second),
		Partitions: s.cfg.IndexPartitions}
	if err := s.index.EnsureIndex(ctx, id, settings); err != nil {
		return fmt.Errorf("index %s: %w", id, err)
	}
	s.ensured.mu.Lock()
	s.ensured.ids[id] = true
	s.ensured.mu.Unlock()
	return nil
}

// docOf is a line as its stream's index holds it.
func docOf(l Line, st *streamRow) quickwit.Doc {
	return quickwit.Doc{TS: l.TS, Tenant: st.Tenant, Stream: st.SID, Environment: st.Environment, Kind: st.Kind,
		Object: st.Object, Replica: st.Replica, Release: st.Release, Instance: st.Instance, Container: st.Container,
		Job: st.Job, Attempt: st.Attempt, Source: st.Source, IOStream: l.Source, Level: l.Level, TraceID: l.Trace,
		Off: l.Off, Partial: l.Partial, Cont: l.Cont, Text: l.Text}
}

// lineOf is a document as a line of the stream row id.
func lineOf(d quickwit.Doc, id int64) Line {
	lvl := d.Level
	if lvl == 0 {
		lvl = LevelUnknown
	}
	return Line{Stream: id, Off: d.Off, TS: d.TS, Source: d.IOStream, Level: lvl, Partial: d.Partial, Cont: d.Cont,
		Text: d.Text, Trace: d.TraceID}
}

// RunIndex runs indexWorkers loops that hand the hot lines to the index,
// each again at once after a round that found work, else after
// ChunkInterval, until ctx ends.
func (s *Store) RunIndex(ctx context.Context) error {
	var wg sync.WaitGroup
	loop := func(name string, round func(context.Context) (int, error), wait time.Duration) {
		defer wg.Done()
		for {
			n, err := round(ctx)
			if err != nil && ctx.Err() == nil {
				s.log.Error(name, "err", err)
			}
			d := wait
			if n > 0 && err == nil {
				d = 0
			}
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
	for range indexWorkers {
		wg.Add(1)
		go loop("cold stage", s.indexRound, s.cfg.ChunkInterval)
	}
	wg.Wait()
	return nil
}

// claimed is a stream the cold stage claimed, with what its documents
// need.
type claimed struct {
	st        *streamRow
	retention *int
	lines     []Line
	// batches are the first offsets of the batches the lines are of,
	// legacy the offsets of those of log_lines (transitional).
	batches, legacy []int64
}

// indexRound claims the streams with hot lines (FOR UPDATE SKIP LOCKED,
// a lease of the database's clock), sends their lines, oldest offset
// first and at most indexBatchBytes, to their indexes, and once Quickwit
// published them moves each stream's mark past them and deletes them, in
// one transaction a stream. It returns how many streams it moved. A
// failure of the index holds the claims for indexBackoff; a batch sent
// again is indexed again, which the reads drop (ADR 0026 §10).
func (s *Store) indexRound(ctx context.Context) (int, error) {
	rows, err := s.db.Pool.Query(ctx, `UPDATE log_streams s SET chunk_owner = $1,
			chunk_lease = now() + make_interval(secs => $2), chunk_claimed_at = now()
		WHERE s.id IN (SELECT id FROM log_streams
			WHERE hot_lines > 0 AND (chunk_lease IS NULL OR chunk_lease < now())
			ORDER BY hot_since LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING `+prefixed("s", streamCols)+`, s.retention_days`,
		s.owner, claimLease.Seconds(), claimBatch)
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*claimed, error) {
		var c claimed
		st, err := scanStreamWith(r, &c.retention)
		c.st = st
		return &c, err
	})
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	if len(cs) == 0 {
		return 0, nil
	}
	// The claims a round leaves: released when it moved its lines (or
	// took none of a stream's), held for indexBackoff when the index
	// failed, which is the retry's backoff.
	failed := false
	defer func() { s.release(cs, failed) }()
	var size int64
	byIndex := map[string][]quickwit.Doc{}
	days := map[string]int{}
	for _, c := range cs {
		if size >= indexBatchBytes {
			break
		}
		lines, n, err := s.hotLines(ctx, c, indexBatchBytes-size)
		if err != nil {
			return 0, err
		}
		size += n
		c.lines = lines
		id, d := s.indexOf(c.retention)
		days[id] = d
		for _, l := range lines {
			byIndex[id] = append(byIndex[id], docOf(l, c.st))
		}
	}
	for id, docs := range byIndex {
		if err := s.ensure(ctx, id, days[id]); err != nil {
			return 0, err
		}
		ictx, cancel := context.WithTimeout(ctx, claimLease-30*time.Second)
		err := s.index.Ingest(ictx, id, docs)
		cancel()
		if err != nil {
			failed = true
			return 0, fmt.Errorf("index %d lines into %s: %w", len(docs), id, err)
		}
	}
	moved := 0
	var errs []error
	for _, c := range cs {
		if len(c.lines) == 0 {
			continue
		}
		if err := s.markIndexed(ctx, c); err != nil {
			errs = append(errs, fmt.Errorf("stream %s: %w", c.st.SID, err))
			continue
		}
		moved++
	}
	return moved, errors.Join(errs...)
}

// maxIndexLines bounds the lines one stream gives a round.
const maxIndexLines = 100000

// hotLines reads a claimed stream's hot batches, oldest offset first,
// whole, until limit bytes of text or maxIndexLines (at least one batch);
// it returns their lines and bytes and notes the batches in c. The lines
// an instance of before left in log_lines go with them, in the order of
// their offsets (transitional: the drain of log_lines).
func (s *Store) hotLines(ctx context.Context, c *claimed, limit int64) ([]Line, int64, error) {
	stream := c.st.ID
	c.batches, c.legacy = nil, nil
	rows, err := s.db.Pool.Query(ctx, `SELECT first_off, last_off, lines, bytes FROM log_batches
		WHERE stream_id = $1 ORDER BY first_off LIMIT 10000`, stream)
	if err != nil {
		return nil, 0, err
	}
	type unit struct {
		first, last, lines, bytes int64
		legacy                    *Line
	}
	units, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (unit, error) {
		var u unit
		err := r.Scan(&u.first, &u.last, &u.lines, &u.bytes)
		return u, err
	})
	if err != nil {
		return nil, 0, err
	}
	// Each list is the first of its table; past the last of a full one,
	// the other's are not taken, so what is taken is the stream's first.
	bound := int64(math.MaxInt64)
	if len(units) == 10000 {
		bound = units[len(units)-1].last
	}
	legacy, err := legacyLines(ctx, s.db.Pool, stream, maxIndexLines)
	if err != nil {
		return nil, 0, err
	}
	if len(legacy) == maxIndexLines {
		bound = min(bound, legacy[len(legacy)-1].Off)
	}
	for i := range legacy {
		l := &legacy[i]
		units = append(units, unit{first: l.Off, last: l.Off, lines: 1, bytes: int64(len(l.Text)), legacy: l})
	}
	slices.SortFunc(units, func(a, b unit) int { return cmpInt(a.first, b.first) })
	var lines []Line
	var size, count int64
	for _, u := range units {
		if u.first > bound || (len(c.batches)+len(c.legacy) > 0 && (size+u.bytes > limit || count+u.lines > maxIndexLines)) {
			break
		}
		size, count = size+u.bytes, count+u.lines
		if u.legacy != nil {
			c.legacy = append(c.legacy, u.first)
			lines = append(lines, *u.legacy)
		} else {
			c.batches = append(c.batches, u.first)
		}
	}
	if len(c.batches) > 0 {
		rows, err := s.db.Pool.Query(ctx, `SELECT data FROM log_batches WHERE stream_id = $1 AND first_off = ANY($2)`,
			stream, c.batches)
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			var data []byte
			if err := rows.Scan(&data); err != nil {
				rows.Close()
				return nil, 0, err
			}
			ls, err := decodeBatch(data, stream)
			if err != nil {
				rows.Close()
				return nil, 0, err
			}
			lines = append(lines, ls...)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, 0, err
		}
	}
	slices.SortFunc(lines, func(a, b Line) int { return cmpInt(a.Off, b.Off) })
	return lines, size, nil
}

// markIndexed moves a claimed stream's mark past the lines it indexed and
// deletes their batches, unless its claim went to another instance
// meanwhile (that one indexes them too: the reads drop the second copy).
func (s *Store) markIndexed(ctx context.Context, c *claimed) error {
	last := c.lines[len(c.lines)-1].Off
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var owner *string
		err := tx.QueryRow(ctx, `SELECT chunk_owner FROM log_streams WHERE id = $1 FOR UPDATE`, c.st.ID).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if owner == nil || *owner != s.owner {
			s.log.Warn("lost a stream's claim to another instance", "stream", c.st.SID)
			return nil
		}
		var n, b int64
		if err := tx.QueryRow(ctx, `WITH d AS (DELETE FROM log_batches WHERE stream_id = $1 AND first_off = ANY($2)
				RETURNING lines, bytes)
			SELECT coalesce(sum(lines), 0), coalesce(sum(bytes), 0) FROM d`, c.st.ID, c.batches).Scan(&n, &b); err != nil {
			return fmt.Errorf("delete the batches: %w", err)
		}
		if len(c.legacy) > 0 {
			var ln, lb int64
			if err := tx.QueryRow(ctx, `WITH d AS (DELETE FROM log_lines WHERE stream_id = $1 AND off = ANY($2)
					RETURNING octet_length(text) AS b)
				SELECT count(*), coalesce(sum(b), 0) FROM d`, c.st.ID, c.legacy).Scan(&ln, &lb); err != nil {
				return fmt.Errorf("delete the lines: %w", err)
			}
			n, b = n+ln, b+lb
		}
		_, err = tx.Exec(ctx, `UPDATE log_streams SET hot_lines = greatest(hot_lines - $2, 0), hot_bytes = greatest(hot_bytes - $3, 0),
				hot_since = CASE WHEN hot_lines - $2 > 0 THEN chunk_claimed_at END,
				chunked_last = greatest(chunked_last, $4), chunk_owner = NULL, chunk_lease = NULL
			WHERE id = $1`, c.st.ID, n, b, last)
		return err
	})
}

// release gives up the claims a round still holds, or with backoff holds
// them for indexBackoff from now.
func (s *Store) release(cs []*claimed, backoff bool) {
	ids := make([]int64, len(cs))
	for i, c := range cs {
		ids[i] = c.st.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sql := `UPDATE log_streams SET chunk_owner = NULL, chunk_lease = NULL WHERE id = ANY($1) AND chunk_owner = $2`
	if backoff {
		sql = `UPDATE log_streams SET chunk_lease = now() + make_interval(secs => $3) WHERE id = ANY($1) AND chunk_owner = $2`
	}
	args := []any{ids, s.owner}
	if backoff {
		args = append(args, indexBackoff.Seconds())
	}
	if _, err := s.db.Pool.Exec(ctx, sql, args...); err != nil {
		s.log.Warn("release claims", "err", err)
	}
}
