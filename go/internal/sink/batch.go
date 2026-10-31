// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
)

// The hot stage is a row per (stream, ingested batch) in log_batches
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

// insertBatches stores a stream's new lines (in offset order, each above
// what the stream has) as rows of log_batches, one unless their text is
// more than maxBatchBytes.
func insertBatches(ctx context.Context, tx db.Querier, stream int64, lines []Line) error {
	for len(lines) > 0 {
		n, size := 0, int64(0)
		for n < len(lines) && (n == 0 || size+int64(len(lines[n].Text)) <= maxBatchBytes) {
			size += int64(len(lines[n].Text))
			n++
		}
		part := lines[:n]
		lines = lines[n:]
		minTS, maxTS, levels := part[0].TS, part[0].TS, 0
		for i := range part {
			minTS, maxTS = min(minTS, part[i].TS), max(maxTS, part[i].TS)
			levels |= part[i].Level
		}
		if _, err := tx.Exec(ctx, `INSERT INTO log_batches (stream_id, first_off, last_off, min_ts, max_ts, lines, bytes,
				levels, data) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			stream, part[0].Off, part[len(part)-1].Off, minTS, maxTS, len(part), size, int16(levels),
			encodeBatch(part)); err != nil {
			return err
		}
	}
	return nil
}

// hotAfter reads in tx the hot lines of each stream ids[i] after offset
// after[i] and at most upTo, of source when it is not nil, in the order of
// (stream, offset), at most limit.
func hotAfter(ctx context.Context, tx db.Querier, ids, after []int64, upTo int64, source *string, limit int) ([]Line, error) {
	from := make(map[int64]int64, len(ids))
	for i, id := range ids {
		from[id] = after[i]
	}
	rows, err := tx.Query(ctx, `SELECT b.stream_id, b.data FROM log_batches b
		JOIN unnest($1::bigint[], $2::bigint[]) AS k(id, after) ON b.stream_id = k.id AND b.last_off > k.after
		WHERE b.first_off <= $3 ORDER BY b.stream_id, b.first_off`, ids, after, upTo)
	if err != nil {
		return nil, err
	}
	var out []Line
	for len(out) < limit && rows.Next() {
		var stream int64
		var data []byte
		if err := rows.Scan(&stream, &data); err != nil {
			rows.Close()
			return nil, err
		}
		ls, err := decodeBatch(data, stream)
		if err != nil {
			rows.Close()
			return nil, err
		}
		for _, l := range ls {
			if l.Off <= from[stream] || l.Off > upTo || (source != nil && l.Source != *source) {
				continue
			}
			out = append(out, l)
			if len(out) >= limit {
				break
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	legacy, err := legacyAfter(ctx, tx, ids, after, upTo, source, limit)
	if err != nil {
		return nil, err
	}
	if len(legacy) == 0 {
		return out, nil
	}
	// Both are the first lines of their table in this order: their union's
	// first limit are the first limit of both.
	out = append(out, legacy...)
	slices.SortFunc(out, func(a, b Line) int {
		if c := cmpInt(a.Stream, b.Stream); c != 0 {
			return c
		}
		return cmpInt(a.Off, b.Off)
	})
	return out[:min(len(out), limit)], nil
}

// The lines of log_lines, a row a line, which the instances of before
// write until they are gone (a rolling update): the reads take them as hot
// lines too, and the indexer drains them (hotLines). Transitional, to go in
// a later release with log_lines itself.

// legacyAfter is hotAfter's part in log_lines.
func legacyAfter(ctx context.Context, tx db.Querier, ids, after []int64, upTo int64, source *string, limit int) ([]Line, error) {
	rows, err := tx.Query(ctx, `SELECT l.stream_id, l.off, l.ts, l.source, l.level, l.partial, l.cont, l.text, l.trace
		FROM log_lines l JOIN unnest($1::bigint[], $2::bigint[]) AS k(id, after) ON l.stream_id = k.id AND l.off > k.after
		WHERE l.off <= $3 AND ($4::text IS NULL OR l.source = $4) ORDER BY l.stream_id, l.off LIMIT $5`,
		ids, after, upTo, source, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanLegacyLine)
}

// legacyLines reads a stream's lines of log_lines, oldest offset first, at
// most limit.
func legacyLines(ctx context.Context, q db.Querier, stream int64, limit int) ([]Line, error) {
	return legacyAfter(ctx, q, []int64{stream}, []int64{math.MinInt64}, math.MaxInt64, nil, limit)
}

func scanLegacyLine(r pgx.CollectableRow) (Line, error) {
	var l Line
	var lvl int16
	err := r.Scan(&l.Stream, &l.Off, &l.TS, &l.Source, &lvl, &l.Partial, &l.Cont, &l.Text, &l.Trace)
	l.Level = int(lvl)
	return l, err
}
