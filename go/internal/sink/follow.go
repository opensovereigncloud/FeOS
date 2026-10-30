// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// followBatch bounds the hot lines one poll of a follow reads.
const followBatch = 5000

// followSink is where a follow writes: a line, with its stream the first
// time, and the heartbeat.
type followSink interface {
	Line(l Line, st *streamRow) error
	Heartbeat() error
}

// Follow answers /api/v1/logs?follow=1: the page a backward read of q
// gives, then every line stored after it, polling every FollowPoll, until
// ctx ends or a write fails. Each stream is followed by its offsets: a
// poll reads, in one snapshot, the lines after the last one it gave and
// the stream's chunks made since, so that no line is given twice or
// missed while the cold stage moves it.
func (s *Store) Follow(ctx context.Context, tenant string, q Query, out followSink) error {
	q.Forward, q.Cursor = false, nil
	live := streamFilter{sel: q.Sel, trace: q.Trace, live: true, limit: maxStreamsRead}
	// Where each stream is: the open ones' before the page, then the
	// page's own, whose snapshot holds its lines; a line stored after it
	// has a later offset.
	streams, err := selectStreams(ctx, s.db.Pool, tenant, live)
	if err != nil {
		return err
	}
	after := map[int64]int64{}
	for _, st := range streams {
		after[st.ID] = st.LastOff
	}
	res, err := s.Read(ctx, tenant, q)
	if err != nil {
		return err
	}
	for id, st := range res.streams {
		after[id] = st.LastOff
	}
	slices.Reverse(res.lines)
	for _, l := range res.lines {
		if err := out.Line(l, res.streams[l.Stream]); err != nil {
			return err
		}
	}
	lastWrite := time.Now()
	t := time.NewTimer(s.cfg.FollowPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		lines, known, more, err := s.followPoll(ctx, tenant, live, q, after)
		if err != nil {
			return err
		}
		for _, l := range lines {
			if err := out.Line(l, known[l.Stream]); err != nil {
				return err
			}
			lastWrite = time.Now()
		}
		if time.Since(lastWrite) >= s.cfg.Heartbeat {
			if err := out.Heartbeat(); err != nil {
				return err
			}
			lastWrite = time.Now()
		}
		if more {
			t.Reset(0)
		} else {
			t.Reset(s.cfg.FollowPoll)
		}
	}
}

// followPoll reads the lines after each stream's place in after, in one
// snapshot with the chunks made since, advances after, and returns the
// lines that pass q's filters in time order; more says a poll should
// follow at once.
func (s *Store) followPoll(ctx context.Context, tenant string, live streamFilter, q Query, after map[int64]int64) (
	[]Line, map[int64]*streamRow, bool, error) {
	tx, err := s.db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	streams, err := selectStreams(ctx, tx, tenant, live)
	if err != nil {
		return nil, nil, false, err
	}
	known := map[int64]*streamRow{}
	var ids, from []int64
	var chunkIDs, chunkFrom []int64
	for _, st := range streams {
		known[st.ID] = st
		a, ok := after[st.ID]
		if !ok {
			a = -1
			after[st.ID] = a
		}
		ids, from = append(ids, st.ID), append(from, a)
		if st.ChunkedLast > a {
			chunkIDs, chunkFrom = append(chunkIDs, st.ID), append(chunkFrom, a)
		}
	}
	var chunks []chunkRef
	if len(chunkIDs) > 0 {
		rows, err := tx.Query(ctx, `SELECT c.id, c.stream_id, c.first_ns, c.last_ns, c.bytes, c.first_off, c.last_off, c.key
			FROM log_chunks c JOIN unnest($1::bigint[], $2::bigint[]) AS k(id, after) ON c.stream_id = k.id AND c.last_off > k.after
			ORDER BY c.stream_id, c.first_off`, chunkIDs, chunkFrom)
		if err != nil {
			return nil, nil, false, fmt.Errorf("chunks: %w", err)
		}
		chunks, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (chunkRef, error) {
			var c chunkRef
			return c, r.Scan(&c.id, &c.stream, &c.firstNs, &c.lastNs, &c.bytes, &c.firstOff, &c.lastOff, &c.key)
		})
		if err != nil {
			return nil, nil, false, fmt.Errorf("chunks: %w", err)
		}
	}
	var hot []Line
	if len(ids) > 0 {
		rows, err := tx.Query(ctx, `SELECT l.stream_id, l.off, l.ts, l.source, l.level, l.partial, l.text
			FROM log_lines l JOIN unnest($1::bigint[], $2::bigint[]) AS k(id, after) ON l.stream_id = k.id AND l.off > k.after
			ORDER BY l.stream_id, l.off LIMIT $3`, ids, from, followBatch)
		if err != nil {
			return nil, nil, false, fmt.Errorf("hot lines: %w", err)
		}
		for rows.Next() {
			var l Line
			var lvl int16
			if err := rows.Scan(&l.Stream, &l.Off, &l.TS, &l.Source, &lvl, &l.Partial, &l.Text); err != nil {
				rows.Close()
				return nil, nil, false, err
			}
			l.Level = int(lvl)
			hot = append(hot, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, false, fmt.Errorf("hot lines: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, err
	}
	var all []Line
	for _, c := range chunks {
		lines, err := s.readChunk(ctx, c)
		if err != nil {
			return nil, nil, false, err
		}
		for _, l := range lines {
			if l.Off > after[c.stream] && l.Off <= known[c.stream].ChunkedLast {
				all = append(all, l)
			}
		}
	}
	all = append(all, hot...)
	for _, l := range all {
		after[l.Stream] = max(after[l.Stream], l.Off)
	}
	out := all[:0]
	for i := range all {
		if q.match(&all[i]) {
			out = append(out, all[i])
		}
	}
	slices.SortStableFunc(out, func(a, b Line) int { return cmpPos(a.pos(), b.pos()) })
	return out, known, len(hot) >= followBatch, nil
}
