// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
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
	Node, Source                                                string
	FirstNs, LastNs                                             *int64
	EndedAt                                                     *time.Time
	Lines, Bytes                                                int64
	ChunkedLast, LastOff                                        int64
}

const streamCols = `id, sid, tenant, instance, container, environment, kind, object, replica, release, job, attempt, node,
	source, first_ns, last_ns, ended_at, lines, bytes, chunked_last, last_off`

func scanStream(r pgx.CollectableRow) (*streamRow, error) { return scanStreamWith(r) }

// scanStreamWith scans streamCols and then the columns after them into
// more.
func scanStreamWith(r pgx.CollectableRow, more ...any) (*streamRow, error) {
	var s streamRow
	err := r.Scan(append([]any{&s.ID, &s.SID, &s.Tenant, &s.Instance, &s.Container, &s.Environment, &s.Kind, &s.Object,
		&s.Replica, &s.Release, &s.Job, &s.Attempt, &s.Node, &s.Source, &s.FirstNs, &s.LastNs, &s.EndedAt,
		&s.Lines, &s.Bytes, &s.ChunkedLast, &s.LastOff}, more...)...)
	return &s, err
}

// prefixed is a list of columns, each with the table alias a.
func prefixed(a, cols string) string {
	var out []string
	for _, c := range strings.Split(cols, ",") {
		out = append(out, a+"."+strings.TrimSpace(c))
	}
	return strings.Join(out, ", ")
}

// streamFilter is a selection of a tenant's streams: the selector, the
// streams with lines between since and until (Unix ns; nil: no bound), and with live only those open or ended in the last ten
// minutes (DB clock).
type streamFilter struct {
	sel          Selector
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
		{"source", f.sel.Source},
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
// the streams and the hot lines in one snapshot, then the index (ADR 0026
// §10) for the lines at or below each stream's mark, which are not hot:
// a line is in exactly one of them, and one the index holds twice is
// taken once. It stops at the limit, at ScanLimit bytes of lines read, or
// at the end.
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
	var scanned int64

	tx, err := s.db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return scanResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	f := streamFilter{sel: q.Sel, until: &q.Until, limit: maxStreamsRead}
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
	sql := `SELECT stream_id, off, ts, source, level, partial, text, trace FROM log_lines
		WHERE stream_id = ANY($1) AND ts >= $2 AND ts <= $3 AND (level & $4) <> 0 AND NOT cont`
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
		if err := rows.Scan(&l.Stream, &l.Off, &l.TS, &l.Source, &lvl, &l.Partial, &l.Text, &l.Trace); err != nil {
			rows.Close()
			return scanResult{}, err
		}
		l.Level = int(lvl)
		scanned += int64(len(l.Text))
		if l.Partial || q.match(&l) {
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
	if err := tx.Commit(ctx); err != nil {
		return scanResult{}, err
	}

	cold, stop, err := s.readIndex(ctx, tenant, &q, res.streams, through, scanned)
	if err != nil {
		return scanResult{}, err
	}
	if stop != nil {
		through, res.partial = earlier(&q, through, *stop), true
	}
	found := append(hot, cold...)
	slices.SortFunc(found, func(a, b Line) int {
		c := cmpPos(a.pos(), b.pos())
		if !q.Forward {
			c = -c
		}
		return c
	})
	if through != nil {
		n := 0
		for n < len(found) && !q.before(*through, found[n].pos()) {
			n++
		}
		found = found[:n]
	}
	if len(found) > q.Limit {
		found = found[:q.Limit]
	}
	switch {
	case len(found) >= q.Limit:
		p := found[len(found)-1].pos()
		res.cursor, res.partial = &p, false
	case through != nil:
		res.cursor = through
	}
	// A partial line is answered joined with its rest, as far as the
	// snapshot has it, and filtered as a whole: a page may then hold fewer
	// lines than its limit, and its cursor goes on after the last line it
	// considered.
	upTo := map[int64]int64{}
	for id, st := range res.streams {
		upTo[id] = st.LastOff
	}
	if err := s.joinParts(ctx, tenant, found, res.streams, upTo); err != nil {
		return scanResult{}, err
	}
	res.lines = found[:0]
	for i := range found {
		if q.match(&found[i]) {
			res.lines = append(res.lines, found[i])
		}
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

// indexQuery is the part of q the index answers: the tenant, the
// selector's labels, the time range, levels, trace and words (a partial
// line passes the words: they may be in its rest).
func indexQuery(tenant string, q *Query) quickwit.Query {
	iq := quickwit.Query{Tenant: tenant, Environment: q.Sel.Environment, Kind: q.Sel.Kind, Object: q.Sel.Object,
		Instance: q.Sel.Instance, Container: q.Sel.Container, Job: q.Sel.Job, Stream: q.Sel.Stream,
		Source: q.Sel.Source, Replica: q.Sel.Replica, Attempt: q.Sel.Attempt, SinceNs: q.Since, UntilNs: q.Until,
		Levels: q.Levels, Trace: q.Trace, Words: q.Words, NotWords: q.NotWords, OrPartial: true}
	if q.Levels == LevelAll {
		iq.Levels = 0
	}
	iq.Order = quickwit.NewestFirst
	if q.Forward {
		iq.Order = quickwit.OldestFirst
	}
	return iq
}

// readIndex reads the index's lines of a read in the scan's order, in
// pages, after q's cursor and not past through: those of the streams
// read, at or below their marks, each (stream, offset) once, that pass
// the filters (a partial one passes, to be joined). It stops with enough
// lines for the page, at the end, or at ScanLimit bytes of lines
// (counting scanned, the hot ones'), returning then where it stopped.
func (s *Store) readIndex(ctx context.Context, tenant string, q *Query, streams map[int64]*streamRow, through *pos,
	scanned int64) ([]Line, *pos, error) {
	bySID := make(map[string]*streamRow, len(streams))
	for _, st := range streams {
		bySID[st.SID] = st
	}
	iq := indexQuery(tenant, q)
	if c := q.Cursor; c != nil {
		if q.Forward {
			iq.SinceNs = max(iq.SinceNs, c.TS)
		} else {
			iq.UntilNs = min(iq.UntilNs, c.TS)
		}
	}
	if through != nil {
		if q.Forward {
			iq.UntilNs = min(iq.UntilNs, through.TS)
		} else {
			iq.SinceNs = max(iq.SinceNs, through.TS)
		}
	}
	iq.Max = min(quickwit.MaxHits, max(2*q.Limit, 500))
	type key struct {
		stream int64
		off    int64
	}
	seen := map[key]bool{}
	var out []Line
	for {
		if iq.UntilNs > 0 && iq.SinceNs > iq.UntilNs {
			return out, nil, nil
		}
		hits, err := s.index.Search(ctx, s.indexPattern(), iq)
		if err != nil {
			return nil, nil, fmt.Errorf("index: %w", err)
		}
		fresh := 0
		for _, h := range hits {
			st := bySID[h.Stream]
			if st == nil || h.Off > st.ChunkedLast || h.Cont {
				continue
			}
			k := key{st.ID, h.Off}
			if seen[k] {
				continue
			}
			seen[k] = true
			fresh++
			scanned += int64(len(h.Text))
			l := lineOf(h, st.ID)
			if !q.inRange(&l) || (through != nil && q.before(*through, l.pos())) {
				continue
			}
			if l.Partial || q.match(&l) {
				out = append(out, l)
			}
		}
		if len(hits) < iq.Max || len(out) >= q.Limit {
			return out, nil, nil
		}
		last := hits[len(hits)-1]
		if scanned >= s.cfg.ScanLimit {
			p := pos{TS: last.TS, Stream: math.MinInt64, Off: math.MinInt64}
			if !q.Forward {
				p = pos{TS: last.TS, Stream: math.MaxInt64, Off: math.MaxInt64}
			}
			return out, &p, nil
		}
		// The next page starts at the last one's time, which it reads
		// again (seen); a page of one time only goes past it.
		switch {
		case q.Forward && fresh == 0:
			iq.SinceNs = last.TS + 1
		case q.Forward:
			iq.SinceNs = last.TS
		case fresh == 0:
			iq.UntilNs = last.TS - 1
		default:
			iq.UntilNs = last.TS
		}
	}
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
	Source      string     `json:"source"`
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
		Node: st.Node, Source: st.Source, FirstTime: nsTime(st.FirstNs), LastTime: nsTime(st.LastNs), EndedAt: ended,
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
