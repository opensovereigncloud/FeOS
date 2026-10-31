// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
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

// pageAll reads every page of v in its direction, oldest first.
func pageAll(t *testing.T, s *Store, v url.Values) []string {
	t.Helper()
	forward := v.Get("direction") == "forward"
	var got []string
	for pages := 0; ; pages++ {
		if pages > 200 {
			t.Fatal("too many pages")
		}
		r := mustRead(t, s, "acme", v)
		if forward {
			got = append(got, texts(r, true)...)
		} else {
			got = append(texts(r, false), got...)
		}
		if r.cursor == nil {
			return got
		}
		v.Set("cursor", encodeCursor(forward, *r.cursor))
	}
}

func TestHotBatchesInterleaved(t *testing.T) {
	s, _ := newTestStore(t, testConfig())
	ctx := context.Background()
	ms := int64(time.Millisecond)
	type want struct {
		ts     int64
		pod    string
		off    int64
		text   string
		errors bool
	}
	var all []want
	// Two streams, six batches each that overlap in time, B's third at
	// the very times of A's; the first two of each indexed, the rest hot.
	for k := range 6 {
		for _, pod := range []string{"a", "b"} {
			var ls []Line
			for i := range 4 {
				ts := base + int64(8*k+2*i)*ms
				if pod == "b" && k != 2 {
					ts += ms
				}
				text := fmt.Sprintf("%s k%d i%d", pod, k, i)
				if i == 3 {
					text = "ERROR " + text
				}
				lvl, _ := Extract(text)
				off := int64(k*40 + i*10)
				ls = append(ls, Line{Off: off, TS: ts, Source: "stdout", Level: lvl, Text: text})
				all = append(all, want{ts, pod, off, text, i == 3})
			}
			if err := s.Ingest(ctx, batch(StreamKey{pod, "main", "L"}, envLabels("acme", "e", 0), ls)); err != nil {
				t.Fatal(err)
			}
		}
		if k == 1 {
			indexAll(t, s)
		}
	}
	var hot int
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM log_batches`).Scan(&hot); err != nil || hot != 8 {
		t.Fatalf("%d hot batches: %v", hot, err)
	}
	// The scan's order: time, stream (a's row first), offset.
	slices.SortFunc(all, func(x, y want) int {
		if c := cmpInt(x.ts, y.ts); c != 0 {
			return c
		}
		if c := strings.Compare(x.pod, y.pod); c != 0 {
			return c
		}
		return cmpInt(x.off, y.off)
	})
	var every, errs []string
	for _, w := range all {
		every = append(every, w.text)
		if w.errors {
			errs = append(errs, w.text)
		}
	}
	since := fmt.Sprint(base - 1)
	small := *s
	small.cfg.ScanLimit = 40
	for _, dir := range []string{"backward", "forward"} {
		for _, c := range []struct {
			name  string
			s     *Store
			limit string
			level string
			want  []string
		}{
			{"all", s, "1000", "", every}, {"pages", s, "5", "", every}, {"errors", s, "2", "error", errs},
			{"scan limit", &small, "1000", "", every},
		} {
			v := url.Values{"environment": {"e"}, "direction": {dir}, "since": {since}, "limit": {c.limit}}
			if c.level != "" {
				v.Set("level", c.level)
			}
			if got := pageAll(t, c.s, v); !slices.Equal(got, c.want) {
				t.Errorf("%s %s:\n got %v\nwant %v", dir, c.name, got, c.want)
			}
		}
	}
}

func TestSweepBatches(t *testing.T) {
	cfg := testConfig()
	cfg.Retention = 24 * time.Hour
	s, _ := newTestStore(t, cfg)
	ctx := context.Background()
	key := StreamKey{"p", "c", "1"}
	old := base - int64(30*time.Hour)
	text := func(i int) string { return fmt.Sprintf("line %d", i) }
	// An expired batch, one half expired (kept whole), and a new one.
	for _, ls := range [][]Line{lines(0, old, 3, text),
		{{Off: 100, TS: old, Source: "stdout", Level: LevelUnknown, Text: "half old"}, {Off: 110, TS: base, Source: "stdout", Level: LevelUnknown, Text: "half new"}},
		lines(200, base, 2, text)} {
		if err := s.Ingest(ctx, batch(key, envLabels("acme", "e", 0), ls)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var batches, hotLines, hotBytes int64
	if err := s.db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM log_batches), hot_lines, hot_bytes FROM log_streams`).
		Scan(&batches, &hotLines, &hotBytes); err != nil {
		t.Fatal(err)
	}
	if batches != 2 || hotLines != 4 || hotBytes != 8+8+6+6 {
		t.Errorf("after the sweep: %d batches, hot %d lines, %d bytes", batches, hotLines, hotBytes)
	}
	r := mustRead(t, s, "acme", url.Values{"environment": {"e"}, "since": {fmt.Sprint(old - 1)}})
	if got := strings.Join(texts(r, false), "|"); got != "half old|half new|line 0|line 1" {
		t.Errorf("read %s", got)
	}
	// The indexer finds the stream's hot lines as the counters say.
	if n := indexAll(t, s); n != 1 {
		t.Errorf("%d streams indexed", n)
	}
	if err := s.db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM log_batches), hot_lines, hot_bytes FROM log_streams`).
		Scan(&batches, &hotLines, &hotBytes); err != nil || batches != 0 || hotLines != 0 || hotBytes != 0 {
		t.Errorf("after indexing: %d batches, hot %d lines, %d bytes: %v", batches, hotLines, hotBytes, err)
	}
}

func TestLargeBatchSplit(t *testing.T) {
	s, _ := newTestStore(t, testConfig())
	ctx := context.Background()
	mib := strings.Repeat("x", 1<<20)
	ls := lines(0, base, 3, func(int) string { return mib })
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "1"}, envLabels("acme", "e", 0), ls)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT first_off, last_off, lines FROM log_batches ORDER BY first_off`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var f, l, n int64
		_ = rows.Scan(&f, &l, &n)
		got = append(got, fmt.Sprintf("%d-%d:%d", f, l, n))
	}
	rows.Close()
	if strings.Join(got, " ") != "0-10:2 20-20:1" {
		t.Errorf("batches %v", got)
	}
	if r := mustRead(t, s, "acme", url.Values{"environment": {"e"}}); len(r.lines) != 3 {
		t.Errorf("%d lines", len(r.lines))
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

func TestLegacyLinesDrained(t *testing.T) {
	s, idx := newTestStore(t, testConfig())
	ctx := context.Background()
	ms := int64(time.Millisecond)
	key, only := StreamKey{"p", "c", "1"}, StreamKey{"q", "c", "1"}
	l := envLabels("acme", "e", 0)
	ln := func(off int64, i int, text string, partial, cont bool) Line {
		lvl, _ := Extract(text)
		return Line{Off: off, TS: base + int64(i)*ms, Source: "stdout", Level: lvl, Partial: partial, Cont: cont, Text: text}
	}
	// A new instance stores a batch, one of before two lines, the second
	// partial, and a new one its rest and a line.
	if err := s.Ingest(ctx, batch(key, l, []Line{ln(0, 0, "l0", false, false), ln(10, 1, "l1", false, false)})); err != nil {
		t.Fatal(err)
	}
	writeLegacy(t, s, key, []Line{ln(20, 2, "old2", false, false), ln(30, 3, "p1 ", true, false)}, 1)
	if err := s.Ingest(ctx, batch(key, l, []Line{ln(40, 4, "p2", false, false), ln(50, 5, "l5", false, false)})); err != nil {
		t.Fatal(err)
	}
	// A stream only an instance of before wrote.
	if err := s.Ingest(ctx, batch(only, envLabels("acme", "e", 1), nil)); err != nil {
		t.Fatal(err)
	}
	writeLegacy(t, s, only, []Line{ln(0, 6, "only", false, false)}, 0)

	want := "l0|l1|old2|p1 p2|l5|only"
	read := func(name string) {
		t.Helper()
		for _, dir := range []string{"backward", "forward"} {
			v := url.Values{"environment": {"e"}, "direction": {dir}, "since": {fmt.Sprint(base - 1)}, "limit": {"2"}}
			if got := strings.Join(pageAll(t, s, v), "|"); got != want {
				t.Errorf("%s, %s: %s", name, dir, got)
			}
		}
	}
	read("hot")
	// A follow's poll takes both, in each stream's offset order.
	after := map[int64]int64{}
	got, _, _, err := s.followPoll(ctx, "acme", streamFilter{sel: Selector{Environment: "e"}, live: true, limit: maxStreamsRead}, after)
	if err != nil {
		t.Fatal(err)
	}
	var follow []string
	for _, g := range got {
		follow = append(follow, g.Text)
	}
	if strings.Join(follow, "|") != "l0|l1|old2|p1 |p2|l5|only" {
		t.Errorf("follow %v", follow)
	}

	// The indexer takes what fits, the stream's first: the first batch,
	// then the line of before after it.
	c := claimed{st: &streamRow{}}
	if err := s.db.Pool.QueryRow(ctx, `SELECT id FROM log_streams WHERE pod = 'p'`).Scan(&c.st.ID); err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		limit           int64
		batches, legacy string
	}{{1, "[0]", "[]"}, {4 + 4, "[0]", "[20]"}, {100, "[0 40]", "[20 30]"}} {
		ls, _, err := s.hotLines(ctx, &c, x.limit)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(c.batches) != x.batches || fmt.Sprint(c.legacy) != x.legacy || ls[len(ls)-1].Off < ls[0].Off {
			t.Errorf("limit %d: batches %v, lines of before %v", x.limit, c.batches, c.legacy)
		}
	}

	if n := indexAll(t, s); n != 2 {
		t.Fatalf("%d streams indexed", n)
	}
	var lineRows, batches, hotLines, hotBytes, mark int64
	if err := s.db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM log_lines), (SELECT count(*) FROM log_batches),
		sum(hot_lines), sum(hot_bytes), max(chunked_last) FROM log_streams`).Scan(&lineRows, &batches, &hotLines, &hotBytes, &mark); err != nil {
		t.Fatal(err)
	}
	if lineRows != 0 || batches != 0 || hotLines != 0 || hotBytes != 0 || mark != 50 || idx.count("acme") != 7 {
		t.Errorf("after the drain: %d lines, %d batches, hot %d/%d, mark %d, %d documents", lineRows, batches, hotLines,
			hotBytes, mark, idx.count("acme"))
	}
	read("indexed")
}
