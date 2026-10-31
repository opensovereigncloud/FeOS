// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
)

// Store is the sink's database and index: the write path, the cold
// stage, the reads and retention.
type Store struct {
	db      *db.DB
	index   Index
	ensured *ensured
	cfg     Config
	log     *slog.Logger
	// owner names this process in a stream's chunk claim.
	owner string
	// scans bounds the reads that scan at once.
	scans chan struct{}
}

// maxScans is how many reads scan at once; more wait.
const maxScans = 8

// NewStore returns the store over d and index.
func NewStore(d *db.DB, index Index, cfg Config, log *slog.Logger) *Store {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return &Store{db: d, index: index, ensured: &ensured{ids: map[string]bool{}}, cfg: cfg, log: log,
		owner: cfg.Name + "/" + hex.EncodeToString(b), scans: make(chan struct{}, maxScans)}
}

// StreamKey is a stream's key as the node names it.
type StreamKey struct {
	Pod, Container, LogID string
}

func (k StreamKey) less(o StreamKey) bool {
	if k.Pod != o.Pod {
		return k.Pod < o.Pod
	}
	if k.Container != o.Container {
		return k.Container < o.Container
	}
	return k.LogID < o.LogID
}

// Labels are a stream's labels, set by ferund through the node (sink.md,
// The OTLP shape) or by the ingress; Attrs are all its resource
// attributes. Node is "" for an ingress's stream; Source is
// SourceIngress or SourceContainer.
type Labels struct {
	Tenant, Node, Instance, Environment, Kind, Object, Job string
	Replica, Release, Attempt                              int
	// RetentionDays is ferun.retention_days, 0 for none.
	RetentionDays int
	Source        string
	Attrs         map[string]string
}

// Line is one stored line. Stream is the stream's row ID; Level a bit;
// Partial says the next line of its source goes on with it, Cont that it
// goes on with the last one; Trace its trace ID, the record's own or the
// one its text names.
type Line struct {
	Stream        int64
	Off, TS       int64
	Source        string
	Level         int
	Partial, Cont bool
	Text          string
	Trace         string
}

// StreamBatch is one stream's part of an Export: its key, labels and
// lines, and Next, the offset after its last line when the node said it
// (log.next_offset), else -1.
type StreamBatch struct {
	Key    StreamKey
	Labels Labels
	Lines  []Line
	Next   int64
}

// ErrQuota is Ingest's refusal of a tenant over its quota; the other
// tenants' lines of the batch are stored.
var ErrQuota = errors.New("over the tenant's log quota")

// newSID returns a new stream ID, s_ and 16 hex digits.
func newSID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "s_" + hex.EncodeToString(b), nil
}

// Ingest stores a batch in one transaction: each stream's row (its
// labels the first write's), its lines once per offset (a line at or
// below the stream's mark, or already hot, is taken as stored), the
// stream's counters and times, and the tenants' usage of
// the day. A stream of a tenant over its quota is left out and Ingest
// returns ErrQuota (wrapped) after the commit.
func (s *Store) Ingest(ctx context.Context, batch []StreamBatch) error {
	sort.Slice(batch, func(i, j int) bool { return batch[i].Key.less(batch[j].Key) })
	var over []string
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		over = over[:0]
		refused := map[string]bool{}
		if s.cfg.Quota > 0 {
			for _, b := range batch {
				t := b.Labels.Tenant
				if _, seen := refused[t]; seen {
					continue
				}
				var used int64
				err := tx.QueryRow(ctx, `SELECT bytes FROM log_usage WHERE tenant = $1 AND day = (now() AT TIME ZONE 'utc')::date`,
					t).Scan(&used)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				refused[t] = used >= s.cfg.Quota
				if refused[t] {
					over = append(over, t)
				}
			}
		}
		usage := map[string]int64{}
		for i := range batch {
			b := &batch[i]
			if refused[b.Labels.Tenant] {
				continue
			}
			n, err := s.ingestStream(ctx, tx, b)
			if err != nil {
				return err
			}
			usage[b.Labels.Tenant] += n
		}
		for t, n := range usage {
			if n == 0 {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO log_usage (tenant, day, bytes) VALUES ($1, (now() AT TIME ZONE 'utc')::date, $2)
				ON CONFLICT (tenant, day) DO UPDATE SET bytes = log_usage.bytes + EXCLUDED.bytes`, t, n); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(over) > 0 {
		return fmt.Errorf("%w: %v", ErrQuota, over)
	}
	return nil
}

// ingestStream stores one stream's part and returns the bytes of text it
// stored anew.
func (s *Store) ingestStream(ctx context.Context, tx pgx.Tx, b *StreamBatch) (int64, error) {
	sid, err := newSID()
	if err != nil {
		return 0, err
	}
	l := b.Labels
	var retention *int
	if l.RetentionDays > 0 {
		retention = &l.RetentionDays
	}
	source := l.Source
	if source == "" {
		source = SourceContainer
	}
	if _, err := tx.Exec(ctx, `INSERT INTO log_streams (sid, pod, container, log_id, tenant, node, instance, environment,
			kind, object, replica, release, job, attempt, retention_days, labels, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (pod, container, log_id) DO NOTHING`,
		sid, b.Key.Pod, b.Key.Container, b.Key.LogID, l.Tenant, l.Node, l.Instance, l.Environment,
		l.Kind, l.Object, l.Replica, l.Release, l.Job, l.Attempt, retention, l.Attrs, source); err != nil {
		return 0, fmt.Errorf("stream: %w", err)
	}
	var id, chunkedLast, lastOff int64
	var open int16
	if err := tx.QueryRow(ctx, `SELECT id, chunked_last, last_off, open_sources FROM log_streams
		WHERE pod = $1 AND container = $2 AND log_id = $3 FOR UPDATE`,
		b.Key.Pod, b.Key.Container, b.Key.LogID).Scan(&id, &chunkedLast, &lastOff, &open); err != nil {
		return 0, fmt.Errorf("lock stream: %w", err)
	}
	// A node sends a stream's lines in the order of their offsets, so a
	// line at or below the last stored is stored; a line goes on with the
	// last of its source when that is partial.
	slices.SortStableFunc(b.Lines, func(x, y Line) int { return cmpInt(x.Off, y.Off) })
	next, last := b.Next, int64(-1)
	from := max(chunkedLast, lastOff)
	var offs, tss []int64
	var sources, texts, traces []string
	var levels []int16
	var partials, conts []bool
	for _, ln := range b.Lines {
		next = max(next, ln.Off)
		last = max(last, ln.Off)
		if ln.Off <= from {
			continue
		}
		from = ln.Off
		bit := sourceBit(ln.Source)
		conts = append(conts, open&bit != 0)
		if ln.Partial {
			open |= bit
		} else {
			open &^= bit
		}
		offs = append(offs, ln.Off)
		tss = append(tss, ln.TS)
		sources = append(sources, ln.Source)
		levels = append(levels, int16(ln.Level))
		partials = append(partials, ln.Partial)
		texts = append(texts, ln.Text)
		traces = append(traces, ln.Trace)
	}
	var n, bytes int64
	var minTS, maxTS *int64
	if len(offs) > 0 {
		rows, err := tx.Query(ctx, `INSERT INTO log_lines (stream_id, off, ts, source, level, partial, cont, text, trace)
			SELECT $1, x.off, x.ts, x.source, x.level, x.partial, x.cont, x.text, x.trace
			FROM unnest($2::bigint[], $3::bigint[], $4::text[], $5::smallint[], $6::boolean[], $7::boolean[], $8::text[],
				$9::text[]) AS x(off, ts, source, level, partial, cont, text, trace)
			WHERE NOT EXISTS (SELECT 1 FROM log_lines l WHERE l.stream_id = $1 AND l.off = x.off)
			ON CONFLICT DO NOTHING
			RETURNING ts, octet_length(text)`, id, offs, tss, sources, levels, partials, conts, texts, traces)
		if err != nil {
			return 0, fmt.Errorf("lines: %w", err)
		}
		for rows.Next() {
			var ts, size int64
			if err := rows.Scan(&ts, &size); err != nil {
				rows.Close()
				return 0, err
			}
			n++
			bytes += size
			if minTS == nil || ts < *minTS {
				minTS = &ts
			}
			if maxTS == nil || ts > *maxTS {
				maxTS = &ts
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("lines: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE log_streams SET
			lines = lines + $2, bytes = bytes + $3, hot_lines = hot_lines + $2, hot_bytes = hot_bytes + $3,
			hot_since = CASE WHEN $2 > 0 THEN coalesce(hot_since, now()) ELSE hot_since END,
			first_ns = least(first_ns, $4), last_ns = greatest(last_ns, $5),
			next_offset = greatest(next_offset, $6), last_off = greatest(last_off, $7), open_sources = $8
		WHERE id = $1`, id, n, bytes, minTS, maxTS, next, last, open); err != nil {
		return 0, fmt.Errorf("stream counters: %w", err)
	}
	return bytes, nil
}

// sourceBit is a source's bit in log_streams.open_sources.
func sourceBit(source string) int16 {
	switch source {
	case "stdout":
		return 1
	case "stderr":
		return 2
	}
	return 4
}

// Positions returns each stream's next offset, 0 for one not known, in
// the order of keys.
func (s *Store) Positions(ctx context.Context, keys []StreamKey) ([]uint64, error) {
	pods, conts, ids := splitKeys(keys)
	rows, err := s.db.Pool.Query(ctx, `SELECT s.pod, s.container, s.log_id, s.next_offset FROM log_streams s
		JOIN unnest($1::text[], $2::text[], $3::text[]) AS k(pod, container, log_id)
			ON s.pod = k.pod AND s.container = k.container AND s.log_id = k.log_id`, pods, conts, ids)
	if err != nil {
		return nil, err
	}
	got := map[StreamKey]int64{}
	for rows.Next() {
		var k StreamKey
		var next int64
		if err := rows.Scan(&k.Pod, &k.Container, &k.LogID, &next); err != nil {
			rows.Close()
			return nil, err
		}
		got[k] = next
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]uint64, len(keys))
	for i, k := range keys {
		out[i] = uint64(max(got[k], 0))
	}
	return out, nil
}

// EndStreams marks the streams ended at endNs (Unix ns; 0: now, DB
// clock); an end once set stays. Streams not known are ignored.
func (s *Store) EndStreams(ctx context.Context, keys []StreamKey, endNs int64) error {
	pods, conts, ids := splitKeys(keys)
	_, err := s.db.Pool.Exec(ctx, `UPDATE log_streams s SET ended_at = coalesce(s.ended_at,
			CASE WHEN $4::bigint = 0 THEN now() ELSE to_timestamp($4::bigint / 1e9) END)
		FROM unnest($1::text[], $2::text[], $3::text[]) AS k(pod, container, log_id)
		WHERE s.pod = k.pod AND s.container = k.container AND s.log_id = k.log_id`, pods, conts, ids, endNs)
	return err
}

func splitKeys(keys []StreamKey) (pods, conts, ids []string) {
	for _, k := range keys {
		pods, conts, ids = append(pods, k.Pod), append(conts, k.Container), append(ids, k.LogID)
	}
	return pods, conts, ids
}

// dbNow is the database's clock.
func (s *Store) dbNow(ctx context.Context) (time.Time, error) {
	var now time.Time
	err := s.db.Pool.QueryRow(ctx, "SELECT now()").Scan(&now)
	return now, err
}
