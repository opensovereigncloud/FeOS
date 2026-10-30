// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
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
// the stream's chunks, then its hot lines, read in one snapshot. A head
// whose rest is not there stays partial.
func (s *Store) joinParts(ctx context.Context, heads []Line, bound map[int64]int64) error {
	for i := range heads {
		h := &heads[i]
		if !h.Partial || h.Cont {
			continue
		}
		upTo, ok := bound[h.Stream]
		if !ok || upTo <= h.Off {
			continue
		}
		tx, err := s.db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return err
		}
		chunks, hot, err := partsOf(ctx, tx, h, upTo)
		_ = tx.Rollback(context.WithoutCancel(ctx))
		if err != nil {
			return fmt.Errorf("the rest of a partial line: %w", err)
		}
		j := newJoiner(h)
		for _, c := range chunks {
			lines, err := s.readChunk(ctx, c)
			if err != nil {
				return err
			}
			for k := range lines {
				l := &lines[k]
				if l.Source == h.Source && l.Off > h.Off && l.Off <= upTo && j.add(l) {
					break
				}
			}
			if j.done {
				break
			}
		}
		for k := range hot {
			if j.add(&hot[k]) {
				break
			}
		}
		if !j.done {
			j.finish(true)
		}
	}
	return nil
}

// partsOf lists the chunks of h's stream after h and reads the hot lines
// of its source after it, at most up to upTo.
func partsOf(ctx context.Context, tx pgx.Tx, h *Line, upTo int64) ([]chunkRef, []Line, error) {
	rows, err := tx.Query(ctx, `SELECT id, stream_id, first_ns, last_ns, bytes, first_off, last_off, key FROM log_chunks
		WHERE stream_id = $1 AND last_off > $2 AND first_off <= $3 ORDER BY first_off LIMIT 64`, h.Stream, h.Off, upTo)
	if err != nil {
		return nil, nil, err
	}
	chunks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (chunkRef, error) {
		var c chunkRef
		return c, r.Scan(&c.id, &c.stream, &c.firstNs, &c.lastNs, &c.bytes, &c.firstOff, &c.lastOff, &c.key)
	})
	if err != nil {
		return nil, nil, err
	}
	rows, err = tx.Query(ctx, `SELECT off, partial, cont, text FROM log_lines
		WHERE stream_id = $1 AND source = $2 AND off > $3 AND off <= $4 ORDER BY off LIMIT 2000`,
		h.Stream, h.Source, h.Off, upTo)
	if err != nil {
		return nil, nil, err
	}
	hot, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Line, error) {
		l := Line{Stream: h.Stream, Source: h.Source}
		return l, r.Scan(&l.Off, &l.Partial, &l.Cont, &l.Text)
	})
	return chunks, hot, err
}
