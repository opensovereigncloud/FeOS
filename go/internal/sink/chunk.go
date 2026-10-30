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
	"time"

	"github.com/jackc/pgx/v5"
)

// The cold stage's bounds: a chunk holds at most maxChunkBytes of text
// (what is left is the next chunk), an object read is at most
// maxObjectBytes, a claim lasts claimLease, and one round claims at most
// claimBatch streams.
const (
	maxChunkBytes  = 16 << 20
	maxObjectBytes = 64 << 20
	claimLease     = 2 * time.Minute
	claimBatch     = 8
)

// chunkLine is a line of a chunk's NDJSON (sink.md, Stages).
type chunkLine struct {
	O int64  `json:"o"`
	T int64  `json:"t"`
	S string `json:"s,omitempty"`
	L string `json:"l,omitempty"`
	P bool   `json:"p,omitempty"`
	X string `json:"x"`
}

// encodeChunk writes lines as gzip NDJSON.
func encodeChunk(lines []Line) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false)
	for _, l := range lines {
		if err := enc.Encode(chunkLine{O: l.Off, T: l.TS, S: l.Source, L: LevelName(l.Level), P: l.Partial, X: l.Text}); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeChunk reads gzip NDJSON into lines of stream, at most limit bytes
// uncompressed.
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
		out = append(out, Line{Stream: stream, Off: c.O, TS: c.T, Source: c.S, Level: lvl, Partial: c.P, Text: c.X})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("chunk: %w", err)
	}
	return out, nil
}

// chunkKey is the object key of a stream's chunk.
func (s *Store) chunkKey(tenant, sid string, firstOff int64) string {
	return s.cfg.keyPrefix() + "logs/" + tenant + "/" + sid + "/" + strconv.FormatInt(firstOff, 10) + ".ndjson.gz"
}

// claimed is a stream the cold stage claimed.
type claimed struct {
	id          int64
	sid, tenant string
}

// RunChunks makes chunks of the streams due every ChunkInterval, at once
// again after a round that found work, until ctx ends.
func (s *Store) RunChunks(ctx context.Context) error {
	for {
		n, err := s.chunkRound(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Error("cold stage", "err", err)
		}
		wait := s.cfg.ChunkInterval
		if n > 0 && err == nil {
			wait = 0
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// chunkRound claims the streams due (at least ChunkBytes of hot text, the
// oldest hot line ChunkAge old, or ended) with FOR UPDATE SKIP LOCKED and a
// lease of the database's clock, and makes a chunk of each. It returns how
// many it made.
func (s *Store) chunkRound(ctx context.Context) (int, error) {
	rows, err := s.db.Pool.Query(ctx, `UPDATE log_streams s SET chunk_owner = $1,
			chunk_lease = now() + make_interval(secs => $2), chunk_claimed_at = now()
		WHERE s.id IN (SELECT id FROM log_streams
			WHERE hot_lines > 0 AND (chunk_lease IS NULL OR chunk_lease < now())
				AND (hot_bytes >= $3 OR hot_since <= now() - make_interval(secs => $4) OR ended_at IS NOT NULL)
			ORDER BY hot_since LIMIT $5 FOR UPDATE SKIP LOCKED)
		RETURNING s.id, s.sid, s.tenant`,
		s.owner, claimLease.Seconds(), s.cfg.ChunkBytes, s.cfg.ChunkAge.Seconds(), claimBatch)
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (claimed, error) {
		var c claimed
		return c, r.Scan(&c.id, &c.sid, &c.tenant)
	})
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	var errs []error
	made := 0
	for _, c := range cs {
		if err := s.makeChunk(ctx, c); err != nil {
			errs = append(errs, fmt.Errorf("stream %s: %w", c.sid, err))
			continue
		}
		made++
	}
	return made, errors.Join(errs...)
}

// makeChunk writes a claimed stream's hot lines, oldest offset first and
// at most maxChunkBytes, to the bucket, then in one transaction records
// the chunk, deletes those lines and releases the claim. A claim lost to
// another instance commits nothing (its chunk has the same key); a stream
// deleted meanwhile has its object deleted. A failure leaves the claim to
// run out, which is the retry's backoff.
func (s *Store) makeChunk(ctx context.Context, c claimed) error {
	rows, err := s.db.Pool.Query(ctx, `SELECT off, ts, source, level, partial, text FROM log_lines
		WHERE stream_id = $1 ORDER BY off LIMIT 200000`, c.id)
	if err != nil {
		return err
	}
	var lines []Line
	var size int64
	for rows.Next() {
		l := Line{Stream: c.id}
		var lvl int16
		if err := rows.Scan(&l.Off, &l.TS, &l.Source, &lvl, &l.Partial, &l.Text); err != nil {
			rows.Close()
			return err
		}
		l.Level = int(lvl)
		lines = append(lines, l)
		size += int64(len(l.Text))
		if size >= maxChunkBytes {
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(lines) == 0 {
		_, err := s.db.Pool.Exec(ctx, `UPDATE log_streams SET chunk_owner = NULL, chunk_lease = NULL, hot_lines = 0, hot_bytes = 0,
			hot_since = NULL WHERE id = $1 AND chunk_owner = $2
			AND NOT EXISTS (SELECT 1 FROM log_lines WHERE stream_id = $1)`, c.id, s.owner)
		return err
	}
	first, last := lines[0], lines[len(lines)-1]
	minTS, maxTS, levels := first.TS, first.TS, 0
	offs := make([]int64, len(lines))
	for i, l := range lines {
		minTS, maxTS = min(minTS, l.TS), max(maxTS, l.TS)
		levels |= l.Level
		offs[i] = l.Off
	}
	data, err := encodeChunk(lines)
	if err != nil {
		return err
	}
	key := s.chunkKey(c.tenant, c.sid, first.Off)
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	err = s.bucket.Put(pctx, key, data, "application/gzip")
	cancel()
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	gone, lost := false, false
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		gone, lost = false, false
		var owner *string
		err := tx.QueryRow(ctx, `SELECT chunk_owner FROM log_streams WHERE id = $1 FOR UPDATE`, c.id).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			gone = true
			return nil
		}
		if err != nil {
			return err
		}
		if owner == nil || *owner != s.owner {
			lost = true
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO log_chunks (stream_id, first_off, last_off, first_ns, last_ns, lines, bytes, levels, key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			c.id, first.Off, last.Off, minTS, maxTS, len(lines), size, levels, key); err != nil {
			return fmt.Errorf("record the chunk: %w", err)
		}
		var n, b int64
		if err := tx.QueryRow(ctx, `WITH d AS (DELETE FROM log_lines WHERE stream_id = $1 AND off = ANY($2)
				RETURNING octet_length(text) AS b)
			SELECT count(*), coalesce(sum(b), 0) FROM d`, c.id, offs).Scan(&n, &b); err != nil {
			return fmt.Errorf("delete the lines: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE log_streams SET hot_lines = greatest(hot_lines - $2, 0), hot_bytes = greatest(hot_bytes - $3, 0),
				hot_since = CASE WHEN hot_lines - $2 > 0 THEN chunk_claimed_at END,
				chunked_last = greatest(chunked_last, $4), chunk_owner = NULL, chunk_lease = NULL
			WHERE id = $1`, c.id, n, b, last.Off)
		return err
	})
	if err != nil {
		return err
	}
	if gone {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		return s.bucket.Delete(dctx, key)
	}
	if lost {
		s.log.Warn("lost a chunk's claim to another instance", "stream", c.sid, "key", key)
	}
	return nil
}
