// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
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
// the stream's mark, and those of them at or below the mark from the
// index, so that no line is given twice or missed while the cold stage
// moves it.
func (s *Store) Follow(ctx context.Context, tenant string, q Query, out followSink) error {
	q.Forward, q.Cursor = false, nil
	if !q.SinceSet {
		q.Since = q.Until - int64(time.Hour)
	}
	live := streamFilter{sel: q.Sel, live: true, limit: maxStreamsRead}
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
	joins := followJoins{open: map[joinKey]*followJoin{}, cut: map[joinKey]bool{}}
	lastWrite := time.Now()
	t := time.NewTimer(s.cfg.FollowPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		lines, known, more, err := s.followPoll(ctx, tenant, live, after)
		if err != nil {
			return err
		}
		lines = joins.take(lines, time.Now())
		for i := range lines {
			l := lines[i]
			if !q.match(&l) {
				continue
			}
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

// joinKey is a stream's source, whose lines a partial one joins.
type joinKey struct {
	stream int64
	source string
}

type followJoin struct {
	j     *joiner
	since time.Time
}

// followJoins joins a follow's partial lines with their rest across polls
// (as joiner does for a read): an open one waits for its rest at most
// joinWait, then is given partial. A line that goes on with one the page
// gave is given on its own; the rest of one cut at maxJoined is dropped.
type followJoins struct {
	open map[joinKey]*followJoin
	cut  map[joinKey]bool
}

// joinWait is how long a follow holds a partial line for its rest.
const joinWait = 5 * time.Second

// take joins the lines of a poll, in each stream's offset order, and
// returns those complete, and those held past joinWait, in time order.
func (f *followJoins) take(lines []Line, now time.Time) []Line {
	var done []Line
	for i := range lines {
		l := lines[i]
		k := joinKey{l.Stream, l.Source}
		if l.Cont {
			if o := f.open[k]; o != nil {
				if o.j.add(&l) {
					done = append(done, *o.j.head)
					delete(f.open, k)
					f.cut[k] = l.Partial
				}
				continue
			}
			if f.cut[k] {
				f.cut[k] = l.Partial
				continue
			}
			done = append(done, l)
			continue
		}
		if o := f.open[k]; o != nil {
			o.j.add(&l)
			done = append(done, *o.j.head)
			delete(f.open, k)
		}
		delete(f.cut, k)
		if l.Partial {
			head := l
			f.open[k] = &followJoin{j: newJoiner(&head), since: now}
			continue
		}
		done = append(done, l)
	}
	for k, o := range f.open {
		if now.Sub(o.since) >= joinWait {
			o.j.finish(true)
			done = append(done, *o.j.head)
			delete(f.open, k)
		}
	}
	slices.SortStableFunc(done, func(a, b Line) int { return cmpPos(a.pos(), b.pos()) })
	return done
}

// followPoll reads the lines after each stream's place in after, in one
// snapshot with the streams' marks (the lines below a mark from the
// index), advances after, and returns them in
// each stream's offset order; more says a poll should follow at once.
func (s *Store) followPoll(ctx context.Context, tenant string, live streamFilter, after map[int64]int64) (
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
	var moved []*streamRow
	for _, st := range streams {
		known[st.ID] = st
		a, ok := after[st.ID]
		if !ok {
			a = -1
			after[st.ID] = a
		}
		ids, from = append(ids, st.ID), append(from, a)
		if st.ChunkedLast > a {
			moved = append(moved, st)
		}
	}
	var hot []Line
	if len(ids) > 0 {
		if hot, err = hotAfter(ctx, tx, ids, from, math.MaxInt64, nil, followBatch); err != nil {
			return nil, nil, false, fmt.Errorf("hot lines: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, err
	}
	var all []Line
	for _, st := range moved {
		lines, err := s.indexedLines(ctx, tenant, st, after[st.ID], st.ChunkedLast, "")
		if err != nil {
			return nil, nil, false, err
		}
		all = append(all, lines...)
	}
	all = append(all, hot...)
	for _, l := range all {
		after[l.Stream] = max(after[l.Stream], l.Off)
	}
	slices.SortStableFunc(all, func(a, b Line) int {
		if c := cmpInt(a.Stream, b.Stream); c != 0 {
			return c
		}
		return cmpInt(a.Off, b.Off)
	})
	return all, known, len(hot) >= followBatch, nil
}

// indexedLines reads a stream's lines with offsets after from, up to and
// including to, from the index, in offset order, each once; source, when
// set, picks the lines of one source (stdout or stderr).
func (s *Store) indexedLines(ctx context.Context, tenant string, st *streamRow, from, to int64, source string) ([]Line, error) {
	var out []Line
	for from < to {
		iq := quickwit.Query{Tenant: tenant, Stream: st.SID, IOStream: source, OffAfter: &from, OffTo: &to,
			Order: quickwit.ByOffset, Max: followBatch}
		hits, err := s.index.Search(ctx, s.indexPattern(), iq)
		if err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
		for _, h := range hits {
			if h.Off <= from {
				continue // the index's second copy
			}
			out = append(out, lineOf(h, st.ID))
			from = h.Off
		}
		if len(hits) < followBatch {
			break
		}
	}
	return out, nil
}
