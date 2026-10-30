// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/sink/s3"
)

// maxChunksRead bounds the chunks one read lists; beyond them it answers
// partial.
const maxChunksRead = 10000

// streamRow is a stream as the reads know it.
type streamRow struct {
	ID                                                          int64
	SID, Tenant, Instance, Container, Environment, Kind, Object string
	Replica, Release                                            int
	Job                                                         string
	Attempt                                                     int
	Node                                                        string
	FirstNs, LastNs                                             *int64
	EndedAt                                                     *time.Time
	Lines, Bytes                                                int64
	ChunkedLast, LastOff                                        int64
}

const streamCols = `id, sid, tenant, instance, container, environment, kind, object, replica, release, job, attempt, node,
	first_ns, last_ns, ended_at, lines, bytes, chunked_last, last_off`

func scanStream(r pgx.CollectableRow) (*streamRow, error) {
	var s streamRow
	err := r.Scan(&s.ID, &s.SID, &s.Tenant, &s.Instance, &s.Container, &s.Environment, &s.Kind, &s.Object,
		&s.Replica, &s.Release, &s.Job, &s.Attempt, &s.Node, &s.FirstNs, &s.LastNs, &s.EndedAt,
		&s.Lines, &s.Bytes, &s.ChunkedLast, &s.LastOff)
	return &s, err
}

// streamFilter is a selection of a tenant's streams: the selector, a
// trace's, the streams with lines between since and until (Unix ns; nil:
// no bound), and with live only those open or ended in the last ten
// minutes (DB clock).
type streamFilter struct {
	sel          Selector
	trace        string
	since, until *int64
	live         bool
	order        string // ORDER BY
	limit        int
}

// selectStreams lists a tenant's streams that f selects; only the
// tenant's are ever seen.
func selectStreams(ctx context.Context, q db.Querier, tenant string, f streamFilter) ([]*streamRow, error) {
	args := []any{tenant}
	where := []string{"tenant = $1"}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	for _, c := range []struct{ col, v string }{
		{"environment", f.sel.Environment}, {"kind", f.sel.Kind}, {"object", f.sel.Object},
		{"instance", f.sel.Instance}, {"container", f.sel.Container}, {"job", f.sel.Job}, {"sid", f.sel.Stream},
	} {
		if c.v != "" {
			add(c.col+" = ?", c.v)
		}
	}
	if f.sel.Replica != nil {
		add("replica = ?", *f.sel.Replica)
	}
	if f.sel.Attempt != nil {
		add("attempt = ?", *f.sel.Attempt)
	}
	if f.trace != "" {
		add("id IN (SELECT stream_id FROM log_traces WHERE trace_id = ?)", f.trace)
	}
	if f.since != nil {
		add("last_ns >= ?", *f.since)
	}
	if f.until != nil {
		add("first_ns <= ?", *f.until)
	}
	if f.live {
		where = append(where, "(ended_at IS NULL OR ended_at >= now() - interval '10 minutes')")
	}
	order := f.order
	if order == "" {
		order = "id"
	}
	args = append(args, f.limit)
	rows, err := q.Query(ctx, "SELECT "+streamCols+" FROM log_streams WHERE "+strings.Join(where, " AND ")+
		" ORDER BY "+order+" LIMIT $"+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("streams: %w", err)
	}
	return pgx.CollectRows(rows, scanStream)
}

// chunkRef is a chunk as a read lists it.
type chunkRef struct {
	id, stream, firstNs, lastNs, bytes int64
	firstOff, lastOff                  int64
	key                                string
}

// scanResult is what a read found: its lines in the scan's order, the
// streams, where the next page goes on (nil: nowhere) and whether it
// stopped at a bound before its limit.
type scanResult struct {
	lines   []Line
	streams map[int64]*streamRow
	cursor  *pos
	partial bool
}

// Read answers a page of /api/v1/logs for tenant (not follow). It reads
// the streams, the hot lines and the chunks' list in one snapshot, so that
// a line is in exactly one of them; then it reads the chunks the hot lines
// leave room for, newest first backward, and stops at the limit, at
// ScanLimit bytes of lines read, or at the end.
func (s *Store) Read(ctx context.Context, tenant string, q Query) (scanResult, error) {
	select {
	case s.scans <- struct{}{}:
	case <-ctx.Done():
		return scanResult{}, ctx.Err()
	}
	defer func() { <-s.scans }()

	res := scanResult{streams: map[int64]*streamRow{}}
	var hot []Line
	var through *pos // everything up to it, in the scan's order, is seen; nil: all
	var chunks []chunkRef
	truncated := false
	var scanned int64

	tx, err := s.db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return scanResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	f := streamFilter{sel: q.Sel, trace: q.Trace, until: &q.Until, limit: maxStreamsRead}
	if q.SinceSet || !q.Forward {
		f.since = &q.Since
	}
	streams, err := selectStreams(ctx, tx, tenant, f)
	if err != nil {
		return scanResult{}, err
	}
	if len(streams) == 0 {
		return res, nil
	}
	ids := make([]int64, len(streams))
	for i, st := range streams {
		ids[i] = st.ID
		res.streams[st.ID] = st
		if !q.SinceSet && q.Forward && st.FirstNs != nil && (i == 0 || *st.FirstNs < q.Since) {
			q.Since = *st.FirstNs
		}
	}

	// The hot lines, in the scan's order, the filters applied here.
	dir, cmp := "DESC", "<"
	if q.Forward {
		dir, cmp = "ASC", ">"
	}
	sql := `SELECT stream_id, off, ts, source, level, partial, text FROM log_lines
		WHERE stream_id = ANY($1) AND ts >= $2 AND ts <= $3 AND (level & $4) <> 0`
	args := []any{ids, q.Since, q.Until, q.Levels}
	if q.Cursor != nil {
		sql += " AND (ts, stream_id, off) " + cmp + " ($5, $6, $7)"
		args = append(args, q.Cursor.TS, q.Cursor.Stream, q.Cursor.Off)
	}
	sql += " ORDER BY ts " + dir + ", stream_id " + dir + ", off " + dir
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return scanResult{}, fmt.Errorf("hot lines: %w", err)
	}
	for rows.Next() {
		var l Line
		var lvl int16
		if err := rows.Scan(&l.Stream, &l.Off, &l.TS, &l.Source, &lvl, &l.Partial, &l.Text); err != nil {
			rows.Close()
			return scanResult{}, err
		}
		l.Level = int(lvl)
		scanned += int64(len(l.Text))
		if q.match(&l) {
			hot = append(hot, l)
		}
		if len(hot) >= q.Limit || scanned >= s.cfg.ScanLimit {
			p := l.pos()
			through = &p
			res.partial = len(hot) < q.Limit
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return scanResult{}, fmt.Errorf("hot lines: %w", err)
	}

	// The chunks that may hold lines before through.
	sql = `SELECT c.id, c.stream_id, c.first_ns, c.last_ns, c.bytes, c.first_off, c.last_off, c.key FROM log_chunks c
		WHERE c.stream_id = ANY($1) AND c.last_ns >= $2 AND c.first_ns <= $3 AND (c.levels & $4) <> 0`
	args = []any{ids, q.Since, q.Until, q.Levels}
	bound := func(p pos) {
		args = append(args, p.TS)
		if q.Forward {
			sql += " AND c.last_ns >= $" + strconv.Itoa(len(args))
		} else {
			sql += " AND c.first_ns <= $" + strconv.Itoa(len(args))
		}
	}
	if q.Cursor != nil {
		bound(*q.Cursor)
	}
	if through != nil {
		args = append(args, through.TS)
		if q.Forward {
			sql += " AND c.first_ns <= $" + strconv.Itoa(len(args))
		} else {
			sql += " AND c.last_ns >= $" + strconv.Itoa(len(args))
		}
	}
	if q.Trace != "" {
		args = append(args, q.Trace)
		sql += ` AND EXISTS (SELECT 1 FROM log_traces t WHERE t.trace_id = $` + strconv.Itoa(len(args)) +
			` AND t.stream_id = c.stream_id AND t.ts BETWEEN c.first_ns AND c.last_ns)`
	}
	if q.Forward {
		sql += " ORDER BY c.first_ns, c.id"
	} else {
		sql += " ORDER BY c.last_ns DESC, c.id DESC"
	}
	args = append(args, maxChunksRead+1)
	sql += " LIMIT $" + strconv.Itoa(len(args))
	rows, err = tx.Query(ctx, sql, args...)
	if err != nil {
		return scanResult{}, fmt.Errorf("chunks: %w", err)
	}
	chunks, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (chunkRef, error) {
		var c chunkRef
		return c, r.Scan(&c.id, &c.stream, &c.firstNs, &c.lastNs, &c.bytes, &c.firstOff, &c.lastOff, &c.key)
	})
	if err != nil {
		return scanResult{}, fmt.Errorf("chunks: %w", err)
	}
	if len(chunks) > maxChunksRead {
		chunks, truncated = chunks[:maxChunksRead], true
	}
	if err := tx.Commit(ctx); err != nil {
		return scanResult{}, err
	}

	// The chunks, in the order their lines may start; the database is not
	// asked again.
	found := hot
	sortScan := func() {
		slices.SortFunc(found, func(a, b Line) int {
			c := cmpPos(a.pos(), b.pos())
			if !q.Forward {
				c = -c
			}
			return c
		})
		if len(found) > q.Limit {
			found = found[:q.Limit]
		}
	}
	done := true
	for _, c := range chunks {
		start := q.chunkStart(c)
		if len(found) >= q.Limit && q.before(found[q.Limit-1].pos(), start) {
			break
		}
		if scanned >= s.cfg.ScanLimit {
			through = earlier(&q, through, start)
			res.partial, done = true, false
			break
		}
		lines, err := s.readChunk(ctx, c)
		if err != nil {
			return scanResult{}, err
		}
		scanned += c.bytes
		for i := range lines {
			if q.inRange(&lines[i]) && q.match(&lines[i]) {
				found = append(found, lines[i])
			}
		}
		sortScan()
	}
	if truncated && done && len(chunks) > 0 {
		through = earlier(&q, through, q.chunkStart(chunks[len(chunks)-1]))
		res.partial = true
	}
	sortScan()
	if through != nil {
		n := 0
		for n < len(found) && !q.before(*through, found[n].pos()) {
			n++
		}
		found = found[:n]
	}
	res.lines = found
	switch {
	case len(found) >= q.Limit:
		p := found[len(found)-1].pos()
		res.cursor, res.partial = &p, false
	case through != nil:
		res.cursor = through
	}
	return res, nil
}

// earlier is whichever of a (nil: none) and b comes first in the scan's
// order.
func earlier(q *Query, a *pos, b pos) *pos {
	if a != nil && q.before(*a, b) {
		return a
	}
	return &b
}

// readChunk fetches and decodes a chunk; one whose object is gone (a
// retention that deleted it and not yet its row) has no lines.
func (s *Store) readChunk(ctx context.Context, c chunkRef) ([]Line, error) {
	data, err := s.bucket.Get(ctx, c.key, maxObjectBytes)
	if errors.Is(err, s3.ErrNotFound) {
		s.log.Warn("a chunk's object is gone", "key", c.key)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read chunk: %w", err)
	}
	return decodeChunk(data, c.stream, maxObjectBytes)
}

// Streams answers /api/v1/log-streams: a tenant's streams of the selector
// with lines since since, the latest first, at most 500.
func (s *Store) Streams(ctx context.Context, tenant string, sel Selector, since int64) ([]*streamRow, error) {
	return selectStreams(ctx, s.db.Pool, tenant, streamFilter{sel: sel, since: &since,
		order: "last_ns DESC NULLS LAST, id DESC", limit: maxStreamsList})
}

// LineJSON is a line of /api/v1/logs.
type LineJSON struct {
	TS      int64     `json:"ts,string"`
	Time    time.Time `json:"time"`
	Stream  string    `json:"stream"`
	Offset  int64     `json:"offset,string"`
	Source  string    `json:"source"`
	Level   string    `json:"level"`
	Partial bool      `json:"partial"`
	Text    string    `json:"text"`
}

// StreamJSON is a stream of /api/v1/logs and /api/v1/log-streams.
type StreamJSON struct {
	ID          string     `json:"id"`
	Tenant      string     `json:"tenant"`
	Instance    string     `json:"instance"`
	Container   string     `json:"container"`
	Environment string     `json:"environment"`
	Kind        string     `json:"kind"`
	Object      string     `json:"object"`
	Replica     int        `json:"replica"`
	Release     int        `json:"release"`
	Job         string     `json:"job"`
	Attempt     int        `json:"attempt"`
	Node        string     `json:"node"`
	FirstTime   *time.Time `json:"firstTime"`
	LastTime    *time.Time `json:"lastTime"`
	EndedAt     *time.Time `json:"endedAt"`
	Lines       int64      `json:"lines"`
	Bytes       int64      `json:"bytes"`
}

func nsTime(ns *int64) *time.Time {
	if ns == nil {
		return nil
	}
	t := time.Unix(0, *ns).UTC()
	return &t
}

func (st *streamRow) json() StreamJSON {
	var ended *time.Time
	if st.EndedAt != nil {
		t := st.EndedAt.UTC()
		ended = &t
	}
	return StreamJSON{
		ID: st.SID, Tenant: st.Tenant, Instance: st.Instance, Container: st.Container, Environment: st.Environment,
		Kind: st.Kind, Object: st.Object, Replica: st.Replica, Release: st.Release, Job: st.Job, Attempt: st.Attempt,
		Node: st.Node, FirstTime: nsTime(st.FirstNs), LastTime: nsTime(st.LastNs), EndedAt: ended,
		Lines: st.Lines, Bytes: st.Bytes,
	}
}

func lineJSON(l Line, sid string) LineJSON {
	return LineJSON{TS: l.TS, Time: time.Unix(0, l.TS).UTC(), Stream: sid, Offset: l.Off, Source: l.Source,
		Level: LevelName(l.Level), Partial: l.Partial, Text: l.Text}
}

// PageJSON is the answer of /api/v1/logs.
type PageJSON struct {
	Lines   []LineJSON            `json:"lines"`
	Streams map[string]StreamJSON `json:"streams"`
	Cursor  string                `json:"cursor"`
	Partial bool                  `json:"partial"`
}

// page renders a read's result: its lines oldest first.
func (r scanResult) page(forward bool) PageJSON {
	p := PageJSON{Lines: make([]LineJSON, 0, len(r.lines)), Streams: map[string]StreamJSON{}, Partial: r.partial}
	for _, l := range r.lines {
		st := r.streams[l.Stream]
		p.Lines = append(p.Lines, lineJSON(l, st.SID))
		if _, ok := p.Streams[st.SID]; !ok {
			p.Streams[st.SID] = st.json()
		}
	}
	if !forward {
		slices.Reverse(p.Lines)
	}
	if r.cursor != nil {
		p.Cursor = encodeCursor(forward, *r.cursor)
	}
	return p
}
