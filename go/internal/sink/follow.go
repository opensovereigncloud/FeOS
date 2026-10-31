// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// followSink is where a follow writes: a line, with its stream the first
// time, and the heartbeat.
type followSink interface {
	Line(l Line, st *streamRow) error
	Heartbeat() error
}

// Follow answers /api/v1/logs?follow=1: the page a backward read of q
// gives, then every line stored after it, as the topics bring it, until
// ctx ends or a write fails (ADR 0026 §11). The topics' partitions the
// selector's streams may lie in (followPartitions) are read from where
// they ended before the page was read; a line the page gave is not given
// again, and a stream of the selector that starts meanwhile is followed
// too.
func (s *Store) Follow(ctx context.Context, tenant string, q Query, out followSink) error {
	q.Forward, q.Cursor = false, nil
	if !q.SinceSet {
		q.Since = q.Until - int64(time.Hour)
	}
	live, err := selectStreams(ctx, s.db.Pool, tenant, streamFilter{sel: q.Sel, live: true, limit: maxStreamsRead})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	recs := make(chan []kafka.Record)
	errc := make(chan error, len(s.classes()))
	for _, days := range s.classes() {
		id := s.indexID(days)
		n, err := s.ensure(ctx, id, days)
		if err != nil {
			return err
		}
		from, err := s.pipes.ends(ctx, id, followPartitions(tenant, q.Sel, live, n))
		if err != nil {
			return err
		}
		if len(from) == 0 {
			continue
		}
		f, err := s.pipes.log.Follow(id, from)
		if err != nil {
			return err
		}
		go func() {
			defer f.Close()
			for {
				rs, err := f.Next(ctx)
				if err != nil {
					errc <- err
					return
				}
				if ctx.Err() != nil {
					return
				}
				if len(rs) > 0 {
					select {
					case recs <- rs:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	res, err := s.Read(ctx, tenant, q)
	if err != nil {
		return err
	}
	type key struct {
		sid string
		off int64
	}
	given := map[key]bool{}
	known := map[string]*streamRow{} // by stream ID; nil: gone
	byID := map[int64]*streamRow{}
	for id, st := range res.streams {
		known[st.SID], byID[id] = st, st
	}
	slices.Reverse(res.lines)
	for _, l := range res.lines {
		st := res.streams[l.Stream]
		given[key{st.SID, l.Off}] = true
		if err := out.Line(l, st); err != nil {
			return err
		}
	}
	joins := followJoins{open: map[joinKey]*followJoin{}, cut: map[joinKey]bool{}}
	beat := time.NewTicker(s.cfg.Heartbeat)
	defer beat.Stop()
	lastWrite := time.Now()
	// A record can come before its stream's row: an ingest produces, then
	// commits. Its line waits for the row, looked for again every
	// streamRetry, at most streamWait.
	var waiting []waitingDoc
	for {
		var retry <-chan time.Time
		if len(waiting) > 0 {
			retry = time.After(streamRetry)
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return err
		case <-beat.C:
		case <-retry:
		case rs := <-recs:
			now := time.Now()
			for _, d := range docsOf(rs) {
				if !selects(tenant, q.Sel, &d) || given[key{d.Stream, d.Off}] {
					continue
				}
				given[key{d.Stream, d.Off}] = true
				waiting = append(waiting, waitingDoc{d: d, since: now})
			}
		}
		var lines []Line
		now := time.Now()
		unknown := map[string]bool{} // not found in this round
		keep := waiting[:0]
		for _, w := range waiting {
			var st *streamRow
			if !unknown[w.d.Stream] {
				var err error
				if st, err = s.followed(ctx, tenant, w.d.Stream, known, now.Sub(w.since) >= streamWait); err != nil {
					return err
				}
			}
			switch _, gone := known[w.d.Stream]; {
			case st != nil:
				byID[st.ID] = st
				lines = append(lines, lineOf(w.d, st.ID))
			case !gone:
				unknown[w.d.Stream] = true
				keep = append(keep, w)
			}
		}
		clear(waiting[len(keep):])
		waiting = keep
		for _, l := range joins.take(lines, time.Now()) {
			if !q.match(&l) {
				continue
			}
			if err := out.Line(l, byID[l.Stream]); err != nil {
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
		// A follow's set of given lines is bounded by what it wrote; a
		// long follow forgets those older than the topics could send again.
		if len(given) > 200000 {
			clear(given)
		}
	}
}

// followPartitions are the partitions of n a follow of sel reads: an
// environment's one; a job's: a tenant's job's one and those of its open
// streams (a run of an environment's job); a stream's or an instance's,
// those of its streams; every one for a job or an instance that has no
// stream yet, since its key is not known before.
func followPartitions(tenant string, sel Selector, live []*streamRow, n int32) []int32 {
	switch {
	case sel.Environment != "":
		return []int32{kafka.Partition(streamKey(tenant, sel.Environment, ""), n)}
	case sel.Job != "" && len(live) > 0:
		return append(partitionsOf(live, n), kafka.Partition(streamKey(tenant, "", sel.Job), n))
	case len(live) > 0:
		return partitionsOf(live, n)
	}
	all := make([]int32, n)
	for i := range all {
		all[i] = int32(i)
	}
	return all
}

// waitingDoc is a record a follow took whose stream it has not found yet.
type waitingDoc struct {
	d     quickwit.Doc
	since time.Time
}

// streamRetry is how often a follow looks for the stream of a line that
// waits for it, streamWait how long at most.
const (
	streamRetry = 100 * time.Millisecond
	streamWait  = 5 * time.Second
)

// followed is the stream of sid that a follow gives a line of: known, or
// read from the database (a stream that started during the follow); nil
// for one not found, which is gone once final (known[sid] nil).
func (s *Store) followed(ctx context.Context, tenant, sid string, known map[string]*streamRow, final bool) (*streamRow, error) {
	if st, ok := known[sid]; ok {
		return st, nil
	}
	sts, err := selectStreams(ctx, s.db.Pool, tenant, streamFilter{sel: Selector{Stream: sid}, limit: 1})
	if err != nil {
		return nil, fmt.Errorf("stream %s: %w", sid, err)
	}
	if len(sts) == 0 {
		if final {
			known[sid] = nil
		}
		return nil, nil
	}
	known[sid] = sts[0]
	return sts[0], nil
}

// selects reports whether a document is of tenant's streams of sel.
func selects(tenant string, sel Selector, d *quickwit.Doc) bool {
	eq := func(want, got string) bool { return want == "" || want == got }
	return d.Tenant == tenant && eq(sel.Environment, d.Environment) && eq(sel.Kind, d.Kind) && eq(sel.Object, d.Object) &&
		eq(sel.Instance, d.Instance) && eq(sel.Container, d.Container) && eq(sel.Job, d.Job) && eq(sel.Stream, d.Stream) &&
		eq(sel.Source, d.Source) && (sel.Replica == nil || *sel.Replica == d.Replica) &&
		(sel.Attempt == nil || *sel.Attempt == d.Attempt)
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
