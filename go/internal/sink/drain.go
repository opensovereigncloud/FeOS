// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/sink/kafka"
)

// Until ADR 0026 §11 the hot stage was a row per (stream, ingested batch)
// in log_batches
// (migration 0006, ADR 0026 §4): the lines in data, encodeBatch's
// encoding, which PostgreSQL compresses (lz4) when it stores it.

// batchVersion is the first byte of log_batches.data.
const batchVersion = 1

// maxBatchBytes bounds the text of one row of log_batches: a stream's part
// of an Export with more is stored as several, so that the indexer's
// "at least one batch" stays a request Quickwit takes.
const maxBatchBytes = 2 << 20

// encodeBatch writes lines as log_batches.data: the version byte, the
// number of lines (uvarint), then each line: its offset and time
// (varints), its level (uvarint), a flags byte (1 partial, 2 cont), and
// its source, text and trace ID, each a uvarint length and its bytes.
func encodeBatch(lines []Line) []byte {
	size := 1 + binary.MaxVarintLen64
	for i := range lines {
		size += 3*binary.MaxVarintLen64 + 1 + 3*binary.MaxVarintLen32 + len(lines[i].Source) + len(lines[i].Text) +
			len(lines[i].Trace)
	}
	b := make([]byte, 0, size)
	b = append(b, batchVersion)
	b = binary.AppendUvarint(b, uint64(len(lines)))
	for i := range lines {
		l := &lines[i]
		b = binary.AppendVarint(b, l.Off)
		b = binary.AppendVarint(b, l.TS)
		b = binary.AppendUvarint(b, uint64(max(l.Level, 0)))
		var flags byte
		if l.Partial {
			flags |= 1
		}
		if l.Cont {
			flags |= 2
		}
		b = append(b, flags)
		for _, s := range []string{l.Source, l.Text, l.Trace} {
			b = binary.AppendUvarint(b, uint64(len(s)))
			b = append(b, s...)
		}
	}
	return b
}

var errBatch = errors.New("log batch: malformed")

// decodeBatch reads log_batches.data into the lines of stream.
func decodeBatch(data []byte, stream int64) ([]Line, error) {
	if len(data) == 0 || data[0] != batchVersion {
		return nil, fmt.Errorf("%w: version", errBatch)
	}
	b := data[1:]
	uvarint := func() (uint64, bool) {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			return 0, false
		}
		b = b[n:]
		return v, true
	}
	varint := func() (int64, bool) {
		v, n := binary.Varint(b)
		if n <= 0 {
			return 0, false
		}
		b = b[n:]
		return v, true
	}
	str := func() (string, bool) {
		n, ok := uvarint()
		if !ok || n > uint64(len(b)) {
			return "", false
		}
		s := b[:n]
		b = b[n:]
		switch string(s) { // the sources, without an allocation
		case "":
			return "", true
		case "stdout":
			return "stdout", true
		case "stderr":
			return "stderr", true
		}
		return string(s), true
	}
	n, ok := uvarint()
	// A line takes at least 7 bytes: no count beyond what data can hold.
	if !ok || n > uint64(len(b)/7+1) {
		return nil, fmt.Errorf("%w: count", errBatch)
	}
	out := make([]Line, n)
	for i := range out {
		l := &out[i]
		l.Stream = stream
		var lvl uint64
		var ok1, ok2, ok3 bool
		l.Off, ok1 = varint()
		l.TS, ok2 = varint()
		lvl, ok3 = uvarint()
		if !ok1 || !ok2 || !ok3 || lvl > math.MaxInt16 || len(b) == 0 {
			return nil, fmt.Errorf("%w: line %d", errBatch, i)
		}
		l.Level = int(lvl)
		l.Partial, l.Cont = b[0]&1 != 0, b[0]&2 != 0
		b = b[1:]
		var ok4, ok5, ok6 bool
		l.Source, ok4 = str()
		l.Text, ok5 = str()
		l.Trace, ok6 = str()
		if !ok4 || !ok5 || !ok6 {
			return nil, fmt.Errorf("%w: line %d", errBatch, i)
		}
	}
	if len(b) != 0 {
		return nil, fmt.Errorf("%w: trailing bytes", errBatch)
	}
	return out, nil
}

// drainEvery is how often the hot rows of before are looked for.
const drainEvery = time.Minute

// drainHot produces what the sink of before left in its hot tables —
// log_batches, log_lines and trace_spans (ADR 0026 §10) — to the topics
// once and deletes it, every drainEvery until ctx ends: while instances of
// before still run (a rolling update) they write there. A stream an
// instance of before holds a claim on is left to it. Transitional: it goes
// with the tables in the next release.
func (p *Program) drainHot(ctx context.Context) error {
	for {
		n, err := p.store.drainHot(ctx)
		if err == nil {
			var m int
			m, err = p.traces.drainHot(ctx)
			n += m
		}
		if err != nil && ctx.Err() == nil {
			p.log.Error("hot rows of before", "err", err)
		} else if n > 0 {
			p.log.Info("moved hot rows of before to the topics", "lines_and_spans", n)
		}
		t := time.NewTimer(drainEvery)
		if n > 0 && err == nil {
			t.Reset(0)
		}
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// drainHot moves the hot lines of up to 64 streams no instance of before
// claims: their rows' lines produced to the streams' topics, then the rows
// deleted, a stream at a time under its lock. It returns the lines moved.
func (s *Store) drainHot(ctx context.Context) (int, error) {
	rows, err := s.db.Pool.Query(ctx, `SELECT DISTINCT stream_id FROM (
			SELECT stream_id FROM log_batches UNION ALL SELECT stream_id FROM log_lines) h
		JOIN log_streams s ON s.id = h.stream_id
		WHERE s.chunk_owner IS NULL OR s.chunk_lease < now() LIMIT 64`)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, id := range ids {
		err := s.db.Tx(ctx, func(tx pgx.Tx) error {
			var keptDays *int
			st, err := scanStreamWith(tx.QueryRow(ctx, `SELECT `+streamCols+`, retention_days FROM log_streams
				WHERE id = $1 FOR UPDATE`, id), &keptDays)
			if err != nil {
				return err
			}
			var lines []Line
			brows, err := tx.Query(ctx, `SELECT data FROM log_batches WHERE stream_id = $1`, id)
			if err != nil {
				return err
			}
			datas, err := pgx.CollectRows(brows, pgx.RowTo[[]byte])
			if err != nil {
				return err
			}
			for _, d := range datas {
				ls, err := decodeBatch(d, id)
				if err != nil {
					return err
				}
				lines = append(lines, ls...)
			}
			lrows, err := tx.Query(ctx, `SELECT off, ts, source, level, partial, cont, text, trace FROM log_lines
				WHERE stream_id = $1`, id)
			if err != nil {
				return err
			}
			legacy, err := pgx.CollectRows(lrows, func(r pgx.CollectableRow) (Line, error) {
				l := Line{Stream: id}
				var lvl int16
				err := r.Scan(&l.Off, &l.TS, &l.Source, &lvl, &l.Partial, &l.Cont, &l.Text, &l.Trace)
				l.Level = int(lvl)
				return l, err
			})
			if err != nil {
				return err
			}
			lines = append(lines, legacy...)
			topic, days := s.indexOf(keptDays)
			n, err := s.ensure(ctx, topic, days)
			if err != nil {
				return err
			}
			recs := make([]kafka.Record, 0, len(lines))
			for _, l := range lines {
				r, err := recordOf(l, st, n)
				if err != nil {
					return err
				}
				recs = append(recs, r)
			}
			if err := s.pipes.produce(ctx, topic, recs); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM log_batches WHERE stream_id = $1`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM log_lines WHERE stream_id = $1`, id); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE log_streams SET hot_lines = 0, hot_bytes = 0, hot_since = NULL,
				chunked_last = greatest(chunked_last, last_off) WHERE id = $1`, id)
			moved += len(lines)
			return err
		})
		if err != nil {
			return moved, fmt.Errorf("stream %d: %w", id, err)
		}
	}
	return moved, nil
}

// drainHot moves up to 5000 spans of trace_spans no instance of before
// claims to the trace topic and deletes them. It returns the spans moved.
func (t *Traces) drainHot(ctx context.Context) (int, error) {
	moved := 0
	err := t.db.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, trace_id, doc::text FROM trace_spans
			WHERE claim_owner IS NULL OR claim_lease < now() ORDER BY id LIMIT 5000 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		var ids []int64
		var recs []kafka.Record
		id, n, err := t.ensure(ctx)
		if err != nil {
			rows.Close()
			return err
		}
		for rows.Next() {
			var rid int64
			var traceID, doc string
			if err := rows.Scan(&rid, &traceID, &doc); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, rid)
			recs = append(recs, spanRecord(traceID, []byte(doc), n))
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(ids) == 0 {
			return err
		}
		if err := t.pipes.produce(ctx, id, recs); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM trace_spans WHERE id = ANY($1)`, ids)
		moved = len(ids)
		return err
	})
	return moved, err
}
