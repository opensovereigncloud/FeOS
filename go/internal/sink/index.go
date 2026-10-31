// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
	"github.com/gardenlinux/feos/go/internal/sink/s3"
)

// Index is the cold stage (ADR 0026 §10): Quickwit (quickwit.Client) or a
// test's. Ingest answers once the documents are published; Search reads
// the indexes a pattern names.
type Index interface {
	EnsureIndex(ctx context.Context, id string, retentionDays, commitSecs int) error
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
	// maxObjectBytes bounds a chunk of before (moveChunks) when read.
	maxObjectBytes = 64 << 20
)

// indexID is the index of a retention in days: <prefix>-<days>d.
func (s *Store) indexID(days int) string {
	return s.cfg.IndexPrefix + "-" + strconv.Itoa(days) + "d"
}

// indexPattern names every log index of the sink.
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
	if err := s.index.EnsureIndex(ctx, id, days, int(s.cfg.CommitTimeout/time.Second)); err != nil {
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
// ChunkInterval, and the move of the chunks of before, until ctx ends.
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
	if s.bucket != nil {
		wg.Add(1)
		go loop("chunks of before", s.moveChunks, time.Minute)
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
		lines, n, err := s.hotLines(ctx, c.st.ID, indexBatchBytes-size)
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

// hotLines reads a stream's hot lines, oldest offset first, until limit
// bytes of text (at least one line); it returns them and their bytes.
func (s *Store) hotLines(ctx context.Context, stream int64, limit int64) ([]Line, int64, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT off, ts, source, level, partial, cont, text, trace FROM log_lines
		WHERE stream_id = $1 ORDER BY off LIMIT 100000`, stream)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var lines []Line
	var size int64
	for rows.Next() {
		l := Line{Stream: stream}
		var lvl int16
		if err := rows.Scan(&l.Off, &l.TS, &l.Source, &lvl, &l.Partial, &l.Cont, &l.Text, &l.Trace); err != nil {
			return nil, 0, err
		}
		l.Level = int(lvl)
		if len(lines) > 0 && size+int64(len(l.Text)) > limit {
			break
		}
		lines = append(lines, l)
		size += int64(len(l.Text))
	}
	return lines, size, rows.Err()
}

// markIndexed moves a claimed stream's mark past the lines it indexed and
// deletes them, unless its claim went to another instance meanwhile (that
// one indexes them too: the reads drop the second copy).
func (s *Store) markIndexed(ctx context.Context, c *claimed) error {
	offs := make([]int64, len(c.lines))
	for i, l := range c.lines {
		offs[i] = l.Off
	}
	last := offs[len(offs)-1]
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
		if err := tx.QueryRow(ctx, `WITH d AS (DELETE FROM log_lines WHERE stream_id = $1 AND off = ANY($2)
				RETURNING octet_length(text) AS b)
			SELECT count(*), coalesce(sum(b), 0) FROM d`, c.st.ID, offs).Scan(&n, &b); err != nil {
			return fmt.Errorf("delete the lines: %w", err)
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

// chunkMoveLock lets one instance at a time move the chunks of before.
const chunkMoveLock int64 = 0x73696e6b_0003

// moveChunks moves the chunks the sink made before Quickwit (ADR 0026
// §10) into the index, a few a round: their lines are indexed, then the
// row and the object go. Only one instance moves at a time.
func (s *Store) moveChunks(ctx context.Context) (int, error) {
	conn, err := s.db.Pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, chunkMoveLock).Scan(&locked); err != nil || !locked {
		return 0, err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, chunkMoveLock) }()
	rows, err := conn.Query(ctx, `SELECT `+prefixed("s", streamCols)+`, s.retention_days, c.id, c.key
		FROM log_chunks c JOIN log_streams s ON s.id = c.stream_id ORDER BY c.id LIMIT 8`)
	if err != nil {
		return 0, fmt.Errorf("chunks: %w", err)
	}
	type chunk struct {
		id        int64
		key       string
		st        *streamRow
		retention *int
	}
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (chunk, error) {
		var c chunk
		st, err := scanStreamWith(r, &c.retention, &c.id, &c.key)
		c.st = st
		return c, err
	})
	if err != nil {
		return 0, fmt.Errorf("chunks: %w", err)
	}
	moved := 0
	for _, c := range cs {
		data, err := s.bucket.Get(ctx, c.key, maxObjectBytes)
		var lines []Line
		switch {
		case errors.Is(err, s3.ErrNotFound):
		case err != nil:
			return moved, fmt.Errorf("read chunk %s: %w", c.key, err)
		default:
			if lines, err = decodeChunk(data, c.st.ID, maxObjectBytes); err != nil {
				return moved, fmt.Errorf("chunk %s: %w", c.key, err)
			}
		}
		if len(lines) > 0 {
			id, days := s.indexOf(c.retention)
			if err := s.ensure(ctx, id, days); err != nil {
				return moved, err
			}
			docs := make([]quickwit.Doc, len(lines))
			for i, l := range lines {
				_, l.Trace = Extract(l.Text)
				docs[i] = docOf(l, c.st)
			}
			if err := s.index.Ingest(ctx, id, docs); err != nil {
				return moved, fmt.Errorf("index chunk %s: %w", c.key, err)
			}
		}
		if _, err := conn.Exec(ctx, `DELETE FROM log_chunks WHERE id = $1`, c.id); err != nil {
			return moved, err
		}
		if err := s.bucket.Delete(ctx, c.key); err != nil && !errors.Is(err, s3.ErrNotFound) {
			s.log.Warn("delete a moved chunk's object", "key", c.key, "err", err)
		}
		s.log.Info("moved a chunk into the index", "key", c.key, "lines", len(lines))
		moved++
	}
	return moved, nil
}

// chunkLine is a line of a chunk of before (gzip NDJSON, sink.md).
type chunkLine struct {
	O int64  `json:"o"`
	T int64  `json:"t"`
	S string `json:"s,omitempty"`
	L string `json:"l,omitempty"`
	P bool   `json:"p,omitempty"`
	C bool   `json:"c,omitempty"`
	X string `json:"x"`
}

// encodeChunk writes lines as a chunk of before (for the tests of the
// move).
func encodeChunk(lines []Line) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false)
	for _, l := range lines {
		if err := enc.Encode(chunkLine{O: l.Off, T: l.TS, S: l.Source, L: LevelName(l.Level), P: l.Partial, C: l.Cont, X: l.Text}); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeChunk reads a chunk of before into lines of stream, at most limit
// bytes uncompressed.
func decodeChunk(data []byte, stream int64, limit int64) ([]Line, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("chunk: %w", err)
	}
	sc := bufio.NewScanner(io.LimitReader(zr, limit))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var out []Line
	for sc.Scan() {
		var c chunkLine
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("chunk line %d: %w", len(out)+1, err)
		}
		lvl := levelBit(c.L)
		if lvl == 0 {
			lvl = LevelUnknown
		}
		out = append(out, Line{Stream: stream, Off: c.O, TS: c.T, Source: c.S, Level: lvl, Partial: c.P, Cont: c.C, Text: c.X})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("chunk: %w", err)
	}
	return out, nil
}
