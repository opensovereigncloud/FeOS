// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

func TestBatchCodec(t *testing.T) {
	in := []Line{
		{Off: 0, TS: base, Source: "stdout", Level: LevelInfo, Text: "INFO a", Trace: traceA},
		{Off: 10, TS: base - 5, Source: "stderr", Level: LevelError, Partial: true, Text: "ERROR \xff part"},
		{Off: 1 << 40, TS: 0, Source: "", Level: LevelUnknown, Cont: true, Partial: true, Text: ""},
		{Off: 1<<40 + 1, TS: -1, Source: "other", Level: 0, Cont: true, Text: strings.Repeat("ü", 70000)},
	}
	data := encodeBatch(in)
	out, err := decodeBatch(data, 7)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		in[i].Stream = 7
	}
	if !slices.Equal(in, out) {
		t.Errorf("decoded %v", out)
	}
	if out, err := decodeBatch(encodeBatch(nil), 1); err != nil || len(out) != 0 {
		t.Errorf("empty: %v, %v", out, err)
	}
	// Anything cut or changed is an error, never a panic.
	for n := range len(data) - 1 {
		if _, err := decodeBatch(data[:n], 1); !errors.Is(err, errBatch) {
			t.Fatalf("cut at %d: %v", n, err)
		}
	}
	for _, bad := range [][]byte{nil, {2, 0}, {batchVersion, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01},
		append(append([]byte(nil), data...), 0)} {
		if _, err := decodeBatch(bad, 1); !errors.Is(err, errBatch) {
			t.Errorf("%x: %v", bad, err)
		}
	}
}

// writeLegacy writes lines into log_lines as an instance of before did:
// rows and the stream's counters, under its lock.
func writeLegacy(t *testing.T, s *Store, key StreamKey, ls []Line, open int16) {
	t.Helper()
	ctx := context.Background()
	for _, l := range ls {
		if _, err := s.db.Pool.Exec(ctx, `WITH st AS (SELECT id FROM log_streams WHERE pod = $1 AND container = $2 AND log_id = $3 FOR UPDATE),
			i AS (INSERT INTO log_lines (stream_id, off, ts, source, level, partial, cont, text, trace)
				SELECT id, $4, $5, $6, $7, $8, $9, $10, '' FROM st)
			UPDATE log_streams SET lines = lines + 1, bytes = bytes + octet_length($10), hot_lines = hot_lines + 1,
				hot_bytes = hot_bytes + octet_length($10), hot_since = coalesce(hot_since, now()),
				first_ns = least(first_ns, $5), last_ns = greatest(last_ns, $5), next_offset = greatest(next_offset, $4), last_off = greatest(last_off, $4),
				open_sources = $11
			WHERE id = (SELECT id FROM st)`, key.Pod, key.Container, key.LogID, l.Off, l.TS, l.Source, int16(l.Level),
			l.Partial, l.Cont, l.Text, open); err != nil {
			t.Fatal(err)
		}
	}
}

// writeBatchOfBefore writes lines (in offset order) as one row of
// log_batches, as an instance of before did, with the stream's counters.
func writeBatchOfBefore(t *testing.T, s *Store, key StreamKey, ls []Line, open int16) {
	t.Helper()
	var bytes, levels int64
	for _, l := range ls {
		bytes += int64(len(l.Text))
		levels |= int64(l.Level)
	}
	first, last := ls[0], ls[len(ls)-1]
	if _, err := s.db.Pool.Exec(context.Background(), `WITH st AS (SELECT id FROM log_streams WHERE pod = $1 AND container = $2 AND log_id = $3 FOR UPDATE),
			i AS (INSERT INTO log_batches (stream_id, first_off, last_off, min_ts, max_ts, lines, bytes, levels, data)
				SELECT id, $4, $5, $6, $7, $8, $9, $10, $11 FROM st)
			UPDATE log_streams SET lines = lines + $8, bytes = bytes + $9, hot_lines = hot_lines + $8, hot_bytes = hot_bytes + $9,
				hot_since = coalesce(hot_since, now()), first_ns = least(first_ns, $6), last_ns = greatest(last_ns, $7),
				next_offset = greatest(next_offset, $5), last_off = greatest(last_off, $5), open_sources = $12
			WHERE id = (SELECT id FROM st)`, key.Pod, key.Container, key.LogID, first.Off, last.Off, first.TS, last.TS,
		len(ls), bytes, levels, encodeBatch(ls), open); err != nil {
		t.Fatal(err)
	}
}

// What instances of before left in log_batches and log_lines is produced
// to the topics once and deleted; a stream such an instance's indexer
// still claims is left to it until its lease runs out.
func TestDrainHot(t *testing.T) {
	s, idx, lg := newTestStore(t, testConfig())
	ctx := context.Background()
	ms := int64(time.Millisecond)
	key, only, claimed := StreamKey{"p", "c", "1"}, StreamKey{"q", "c", "1"}, StreamKey{"r", "c", "1"}
	l := envLabels("acme", "e", 0)
	ln := func(off int64, i int, text string, partial, cont bool) Line {
		lvl, _ := Extract(text)
		return Line{Off: off, TS: base + int64(i)*ms, Source: "stdout", Level: lvl, Partial: partial, Cont: cont, Text: text}
	}
	// This instance stores a batch, one of before two lines, the second
	// partial, and this one its rest and a line.
	if err := s.Ingest(ctx, batch(key, l, []Line{ln(0, 0, "l0", false, false), ln(10, 1, "l1", false, false)})); err != nil {
		t.Fatal(err)
	}
	writeBatchOfBefore(t, s, key, []Line{ln(20, 2, "old2", false, false), ln(30, 3, "p1 ", true, false)}, 1)
	if err := s.Ingest(ctx, batch(key, l, []Line{ln(40, 4, "p2", false, false), ln(50, 5, "l5", false, false)})); err != nil {
		t.Fatal(err)
	}
	// A stream only an instance of before wrote, into log_lines, and one
	// whose lines an indexer of before has claimed.
	for i, k := range []StreamKey{only, claimed} {
		if err := s.Ingest(ctx, batch(k, envLabels("acme", "e", i+1), nil)); err != nil {
			t.Fatal(err)
		}
	}
	writeLegacy(t, s, only, []Line{ln(0, 6, "only", false, false)}, 0)
	writeLegacy(t, s, claimed, []Line{ln(0, 7, "claimed", false, false)}, 0)
	if _, err := s.db.Pool.Exec(ctx, `UPDATE log_streams SET chunk_owner = 'sink-before', chunk_lease = now() + interval '1 minute'
		WHERE pod = 'r'`); err != nil {
		t.Fatal(err)
	}

	n, err := s.drainHot(ctx)
	if err != nil || n != 3 {
		t.Fatalf("drained %d: %v", n, err)
	}
	if n, err := s.drainHot(ctx); err != nil || n != 0 {
		t.Errorf("drained again %d: %v", n, err)
	}
	hot := func() string {
		t.Helper()
		var batches, legacy, hotLines, mark int64
		if err := s.db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM log_batches), (SELECT count(*) FROM log_lines),
			sum(hot_lines), (SELECT chunked_last FROM log_streams WHERE pod = 'p') FROM log_streams`).
			Scan(&batches, &legacy, &hotLines, &mark); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%d batches, %d lines, %d hot, mark %d", batches, legacy, hotLines, mark)
	}
	if got := hot(); got != "0 batches, 1 lines, 1 hot, mark 50" || lg.count(logTopic) != 7 {
		t.Errorf("after the drain: %s, %d records", got, lg.count(logTopic))
	}
	// The drained lines lie in their streams' partitions.
	for _, r := range lg.records(logTopic) {
		if string(r.Key) != "acme/e/" || r.Partition != kafka.Partition("acme/e/", int32(testConfig().KafkaPartitions)) {
			t.Errorf("record %q in %d", r.Key, r.Partition)
		}
	}
	// A line of before the node sends again is not stored again.
	if err := s.Ingest(ctx, batch(key, l, []Line{ln(30, 3, "p1 ", true, false)})); err != nil || lg.count(logTopic) != 7 {
		t.Errorf("sent again: %d records, %v", lg.count(logTopic), err)
	}

	read := func(when, want string) {
		t.Helper()
		for _, dir := range []string{"backward", "forward"} {
			v := url.Values{"environment": {"e"}, "direction": {dir}, "since": {fmt.Sprint(base - 1)}, "limit": {"2"}}
			if got := strings.Join(pageAll(t, s, v), "|"); got != want {
				t.Errorf("%s, %s: %s", when, dir, got)
			}
		}
	}
	read("tail", "l0|l1|old2|p1 p2|l5|only")
	idx.sync(lg)
	read("index", "l0|l1|old2|p1 p2|l5|only")

	// The claim runs out: the stream is drained too.
	if _, err := s.db.Pool.Exec(ctx, `UPDATE log_streams SET chunk_lease = now() - interval '1 second' WHERE pod = 'r'`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.drainHot(ctx); err != nil || n != 1 {
		t.Fatalf("drained after the lease %d: %v", n, err)
	}
	if got := hot(); got != "0 batches, 0 lines, 0 hot, mark 50" {
		t.Errorf("after the lease: %s", got)
	}
	read("after the lease", "l0|l1|old2|p1 p2|l5|only|claimed")

	// A failed produce keeps the rows for the next round.
	writeLegacy(t, s, only, []Line{ln(10, 8, "late", false, false)}, 0)
	lg.failProduce = errors.New("down")
	if _, err := s.drainHot(ctx); err == nil {
		t.Error("no error")
	}
	lg.failProduce = nil
	if n, err := s.drainHot(ctx); err != nil || n != 1 {
		t.Errorf("drained after the failure %d: %v", n, err)
	}
}

// spansOfBefore is how many rows trace_spans holds.
func spansOfBefore(t *testing.T, tr *Traces) int {
	t.Helper()
	var n int
	if err := tr.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM trace_spans`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The spans instances of before left in trace_spans are produced to the
// trace topic, keyed by their trace IDs, and deleted; those an indexer of
// before claims wait for its lease.
func TestTraceDrainHot(t *testing.T) {
	tr, _, lg := newTestTraces(t, testConfig())
	ctx := context.Background()
	start := time.Now().Add(-time.Minute).UnixNano()
	spans, _, err := parseTraceExport(traceRequest(podAttrs("acme", "e", "api"),
		testSpan(traceA, "000000000000000a", "", "GET /", start, time.Millisecond),
		testSpan(traceA, "000000000000000b", "000000000000000a", "query", start+1, time.Millisecond),
		testSpan(traceB, "000000000000000c", "", "POST /", start+2, time.Millisecond)), ingestPeer{node: "node-1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range spans {
		var owner *string
		if i == 1 {
			o := "sink-before"
			owner = &o
		}
		if _, err := tr.db.Pool.Exec(ctx, `INSERT INTO trace_spans (tenant, trace_id, span_id, start_ns, doc, bytes, claim_owner, claim_lease)
			VALUES ($1, $2, $3, $4, $5, $6, $7, CASE WHEN $7::text IS NULL THEN NULL ELSE now() + interval '1 minute' END)`,
			s.doc.Tenant, s.doc.TraceID, s.doc.SpanID, s.doc.StartNs, string(s.raw), len(s.raw), owner); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := tr.drainHot(ctx); err != nil || n != 2 {
		t.Fatalf("drained %d: %v", n, err)
	}
	id, _ := tr.indexID()
	parts := int32(testConfig().KafkaPartitions)
	for _, r := range lg.records(id) {
		var d quickwit.SpanDoc
		if err := json.Unmarshal(r.Value, &d); err != nil || string(r.Key) != d.TraceID || r.Partition != kafka.Partition(d.TraceID, parts) {
			t.Errorf("record %q in %d: %v", r.Key, r.Partition, err)
		}
	}
	got, err := tr.Trace(ctx, quickwit.SpanQuery{Tenant: "acme", TraceID: traceA})
	if err != nil || spanIDs(got) != "000a " || spansOfBefore(t, tr) != 1 {
		t.Errorf("before the lease: %s, %d left, %v", spanIDs(got), spansOfBefore(t, tr), err)
	}
	if _, err := tr.db.Pool.Exec(ctx, `UPDATE trace_spans SET claim_lease = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if n, err := tr.drainHot(ctx); err != nil || n != 1 {
		t.Fatalf("drained after the lease %d: %v", n, err)
	}
	got, err = tr.Trace(ctx, quickwit.SpanQuery{Tenant: "acme", TraceID: traceA})
	if err != nil || spanIDs(got) != "000a 000b " || spansOfBefore(t, tr) != 0 {
		t.Errorf("after the lease: %s, %d left, %v", spanIDs(got), spansOfBefore(t, tr), err)
	}
}
