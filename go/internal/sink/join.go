// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// maxJoined bounds a joined line's text; one cut there stays partial.
const maxJoined = 1 << 20

// joiner joins a partial line with the lines that go on with it (Cont), in
// the order of their offsets: up to and including the first that is not
// partial, at most maxJoined bytes.
type joiner struct {
	head *Line
	text strings.Builder
	done bool
}

func newJoiner(head *Line) *joiner {
	j := &joiner{head: head}
	j.text.WriteString(head.Text)
	return j
}

// add takes the next line of the head's stream and source; it reports
// whether the joined line is complete.
func (j *joiner) add(l *Line) bool {
	if j.done {
		return true
	}
	if !l.Cont {
		// The line ended without its rest: it stays partial.
		return j.finish(true)
	}
	j.text.WriteString(l.Text)
	if j.text.Len() >= maxJoined {
		return j.finish(true)
	}
	if !l.Partial {
		return j.finish(false)
	}
	return false
}

// finish ends the joined line; partial says it is not whole.
func (j *joiner) finish(partial bool) bool {
	t := j.text.String()
	if len(t) > maxJoined {
		cut := maxJoined
		for cut > 0 && !utf8.RuneStart(t[cut]) {
			cut--
		}
		t, partial = t[:cut], true
	}
	j.head.Text, j.head.Partial, j.done = t, partial, true
	return true
}

// joinParts completes each partial head of a read with the lines that go
// on with it, those at or below bound[stream] (the read's snapshot): from
// the index up to the stream's mark, then its hot lines, the mark and the
// hot lines read in one snapshot. A head whose rest is not there stays
// partial.
func (s *Store) joinParts(ctx context.Context, tenant string, heads []Line, streams map[int64]*streamRow,
	bound map[int64]int64) error {
	for i := range heads {
		h := &heads[i]
		if !h.Partial || h.Cont {
			continue
		}
		upTo, ok := bound[h.Stream]
		st := streams[h.Stream]
		if !ok || st == nil || upTo <= h.Off {
			continue
		}
		tx, err := s.db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return err
		}
		mark, hot, err := partsOf(ctx, tx, h, upTo)
		_ = tx.Rollback(context.WithoutCancel(ctx))
		if err != nil {
			return fmt.Errorf("the rest of a partial line: %w", err)
		}
		var cold []quickwit.Doc
		if to := min(mark, upTo); to > h.Off {
			from := h.Off
			cold, err = s.index.Search(ctx, s.indexPattern(), quickwit.Query{Tenant: tenant, Stream: st.SID,
				IOStream: h.Source, OffAfter: &from, OffTo: &to, Order: quickwit.ByOffset, Max: 2000})
			if err != nil {
				return fmt.Errorf("the rest of a partial line: index: %w", err)
			}
		}
		j := newJoiner(h)
		last := h.Off
		for k := range cold {
			if cold[k].Off <= last {
				continue // the index's second copy
			}
			last = cold[k].Off
			l := lineOf(cold[k], h.Stream)
			if j.add(&l) {
				break
			}
		}
		for k := range hot {
			if j.done || j.add(&hot[k]) {
				break
			}
		}
		if !j.done {
			j.finish(true)
		}
	}
	return nil
}

// partsOf reads the mark of h's stream and the hot lines of its source
// after h, at most up to upTo.
func partsOf(ctx context.Context, tx pgx.Tx, h *Line, upTo int64) (int64, []Line, error) {
	var mark int64
	if err := tx.QueryRow(ctx, `SELECT chunked_last FROM log_streams WHERE id = $1`, h.Stream).Scan(&mark); err != nil {
		return 0, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT off, partial, cont, text FROM log_lines
		WHERE stream_id = $1 AND source = $2 AND off > $3 AND off <= $4 ORDER BY off LIMIT 2000`,
		h.Stream, h.Source, h.Off, upTo)
	if err != nil {
		return 0, nil, err
	}
	hot, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Line, error) {
		l := Line{Stream: h.Stream, Source: h.Source}
		return l, r.Scan(&l.Off, &l.Partial, &l.Cont, &l.Text)
	})
	return mark, hot, err
}
