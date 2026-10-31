// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// quickwitQuery selects a tenant's environment, as many as the tests hold.
func quickwitQuery(tenant, env string) quickwit.Query {
	return quickwit.Query{Tenant: tenant, Environment: env, Max: 100000}
}

func testConfig() Config {
	c := Default()
	c.Name = "sink-0"
	return c
}

// newTestDB is a migrated, empty database of the sink's own.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	d := dbtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := Migrate(ctx, d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

func newTestStore(t *testing.T, cfg Config) (*Store, *memIndex) {
	t.Helper()
	d := newTestDB(t)
	idx := newMemIndex()
	return NewStore(d, idx, cfg, dbtest.Logger(t)), idx
}

// base is the tests' time of the first line.
var base = time.Now().Add(-5 * time.Minute).UnixNano()

func envLabels(tenant, env string, replica int) Labels {
	return Labels{Tenant: tenant, Node: "node-1", Instance: "inst" + fmt.Sprint(replica), Environment: env, Kind: "service",
		Object: "api", Replica: replica, Release: 3, Attrs: map[string]string{attrTenant: tenant}}
}

// lines makes n lines from offset off on, 10 bytes apart, a millisecond
// apart from ts on.
func lines(off int64, ts int64, n int, text func(i int) string) []Line {
	out := make([]Line, n)
	for i := range out {
		t := text(i)
		lvl, tr := Extract(t)
		out[i] = Line{Off: off + int64(i)*10, TS: ts + int64(i)*int64(time.Millisecond), Source: "stdout", Level: lvl, Trace: tr, Text: t}
	}
	return out
}

func batch(key StreamKey, l Labels, ls []Line) []StreamBatch {
	return []StreamBatch{{Key: key, Labels: l, Lines: ls, Next: -1}}
}

func mustRead(t *testing.T, s *Store, tenant string, v url.Values) scanResult {
	t.Helper()
	q, err := parseQuery(v, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Read(context.Background(), tenant, q)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func texts(r scanResult, forward bool) []string {
	p := r.page(forward)
	out := make([]string, len(p.Lines))
	for i, l := range p.Lines {
		out[i] = l.Text
	}
	return out
}

func TestIngestIdempotent(t *testing.T) {
	s, _ := newTestStore(t, testConfig())
	ctx := context.Background()
	key := StreamKey{"pod1", "main", "L1"}
	l := envLabels("acme", "env1", 0)
	ls := lines(0, base, 5, func(i int) string { return fmt.Sprintf("line %d", i) })
	for range 3 {
		if err := s.Ingest(ctx, batch(key, l, ls)); err != nil {
			t.Fatal(err)
		}
	}
	// Overlapping and new, and labels that lose to the first write's.
	l2 := envLabels("acme", "other", 1)
	more := lines(30, base+int64(3*time.Millisecond), 4, func(i int) string { return fmt.Sprintf("line %d", i+3) })
	if err := s.Ingest(ctx, batch(key, l2, more)); err != nil {
		t.Fatal(err)
	}
	var n, rowCount, lineCount, bytes, hotLines, hotBytes, next, lastOff int64
	var env string
	if err := s.db.Pool.QueryRow(ctx, `SELECT (SELECT coalesce(sum(lines), 0) FROM log_batches), (SELECT count(*) FROM log_batches),
		lines, bytes, hot_lines, hot_bytes, next_offset, last_off, environment
		FROM log_streams`).Scan(&n, &rowCount, &lineCount, &bytes, &hotLines, &hotBytes, &next, &lastOff, &env); err != nil {
		t.Fatal(err)
	}
	if n != 7 || lineCount != 7 || bytes != 7*6 || hotLines != 7 || hotBytes != 7*6 || next != 60 || lastOff != 60 || env != "env1" {
		t.Errorf("lines %d, counted %d, bytes %d, hot %d/%d, next %d, last %d, env %q", n, lineCount, bytes, hotLines, hotBytes,
			next, lastOff, env)
	}
	// A row per batch: the first, sent three times, once, and the new
	// lines of the one that overlaps it.
	type row struct{ first, last, minTS, maxTS, lines, bytes, levels int64 }
	rs, err := s.db.Pool.Query(ctx, `SELECT first_off, last_off, min_ts, max_ts, lines, bytes, levels FROM log_batches ORDER BY first_off`)
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.first, &r.last, &r.minTS, &r.maxTS, &r.lines, &r.bytes, &r.levels); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rs.Close()
	ms := int64(time.Millisecond)
	if want := []row{{0, 40, base, base + 4*ms, 5, 30, LevelUnknown}, {50, 60, base + 5*ms, base + 6*ms, 2, 12, LevelUnknown}}; rowCount != 2 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("batches %v, want %v", got, want)
	}
	pos, err := s.Positions(ctx, []StreamKey{{"nope", "x", "y"}, key})
	if err != nil || pos[0] != 0 || pos[1] != 60 {
		t.Errorf("positions %v, %v", pos, err)
	}
	// A node that sends log.next_offset gets it back.
	b := batch(key, l, lines(70, base+int64(10*time.Millisecond), 1, func(int) string { return "x" }))
	b[0].Next = 82
	if err := s.Ingest(ctx, b); err != nil {
		t.Fatal(err)
	}
	if pos, _ := s.Positions(ctx, []StreamKey{key}); pos[0] != 82 {
		t.Errorf("positions after next_offset %v", pos)
	}
	if err := s.EndStreams(ctx, []StreamKey{key, {"nope", "x", "y"}}, 0); err != nil {
		t.Fatal(err)
	}
	r := mustRead(t, s, "acme", url.Values{"environment": {"env1"}})
	if got := strings.Join(texts(r, false), "|"); got != "line 0|line 1|line 2|line 3|line 4|line 5|line 6|x" {
		t.Errorf("read %s", got)
	}
	for _, st := range r.streams {
		if st.EndedAt == nil {
			t.Error("stream not ended")
		}
	}
	if r := mustRead(t, s, "other-tenant", url.Values{"environment": {"env1"}}); len(r.lines) != 0 {
		t.Error("another tenant reads the stream")
	}
}

func TestQuota(t *testing.T) {
	cfg := testConfig()
	cfg.Quota = 100
	s, _ := newTestStore(t, cfg)
	ctx := context.Background()
	text := func(int) string { return strings.Repeat("a", 60) }
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "1"}, envLabels("small", "e", 0), lines(0, base, 2, text))); err != nil {
		t.Fatal(err)
	}
	// Over now: small's next batch is refused, big's in the same batch stored.
	b := append(batch(StreamKey{"p", "c", "1"}, envLabels("small", "e", 0), lines(100, base, 1, text)),
		batch(StreamKey{"q", "c", "1"}, envLabels("big", "e", 0), lines(0, base, 1, text))...)
	err := s.Ingest(ctx, b)
	if !errors.Is(err, ErrQuota) || !strings.Contains(err.Error(), "small") {
		t.Fatalf("over quota: %v", err)
	}
	var small, big int64
	if err := s.db.Pool.QueryRow(ctx, `SELECT (SELECT sum(b.lines) FROM log_batches b JOIN log_streams s ON s.id = b.stream_id WHERE s.tenant = 'small'),
		(SELECT sum(b.lines) FROM log_batches b JOIN log_streams s ON s.id = b.stream_id WHERE s.tenant = 'big')`).Scan(&small, &big); err != nil {
		t.Fatal(err)
	}
	if small != 2 || big != 1 {
		t.Errorf("small %d, big %d", small, big)
	}
}

// indexAll hands every hot line to the index.
func indexAll(t *testing.T, s *Store) int {
	t.Helper()
	total := 0
	for {
		n, err := s.indexRound(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return total
		}
		total += n
	}
}

func TestIndexAndQueries(t *testing.T) {
	cfg := testConfig()
	s, idx := newTestStore(t, cfg)
	ctx := context.Background()
	key1, key2 := StreamKey{"pod1", "main", "L1"}, StreamKey{"pod2", "main", "L1"}
	// Stream 1: 30 lines, indexed in pieces; stream 2: interleaved in
	// time, its first batch indexed, two kept hot.
	ln := func(prefix string) func(int) string {
		return func(i int) string {
			lvl := "INFO"
			if i%10 == 7 {
				lvl = "ERROR"
			}
			return fmt.Sprintf("%s %s line %02d", lvl, prefix, i)
		}
	}
	for i := 0; i < 30; i += 10 {
		if err := s.Ingest(ctx, batch(key1, envLabels("acme", "env1", 0),
			lines(int64(i)*10, base+int64(2*i)*int64(time.Millisecond), 10, func(j int) string { return ln("a")(i + j) }))); err != nil {
			t.Fatal(err)
		}
		if n := indexAll(t, s); n != 1 {
			t.Fatalf("%d streams indexed", n)
		}
	}
	// The index holds one of the batches twice, as after an answer lost.
	idx.dupNext = true
	if err := s.Ingest(ctx, batch(key2, envLabels("acme", "env1", 1),
		lines(0, base+int64(time.Millisecond), 10, func(j int) string { return ln("b")(j) }))); err != nil {
		t.Fatal(err)
	}
	indexAll(t, s)
	// The rest of stream 2 stays hot.
	for k := range 2 {
		if err := s.Ingest(ctx, batch(key2, envLabels("acme", "env1", 1),
			lines(100+int64(k)*100, base+int64(11+10*k)*int64(time.Millisecond), 10, func(j int) string { return ln("b")(j + 10 + 10*k) }))); err != nil {
			t.Fatal(err)
		}
	}
	var hot, rows int
	if err := s.db.Pool.QueryRow(ctx, `SELECT sum(lines), count(*) FROM log_batches`).Scan(&hot, &rows); err != nil {
		t.Fatal(err)
	}
	if n := idx.count("acme"); n != 50 || hot != 20 || rows != 2 {
		t.Fatalf("indexed %d, hot %d in %d batches", n, hot, rows)
	}
	if ids, _ := idx.Indexes(ctx, ""); fmt.Sprint(ids) != "[ferun-logs-v2-30d]" {
		t.Errorf("indexes %v", ids)
	}
	// A read finds the lines of an index of before (version 1,
	// <prefix>-<days>d) with the current version's: stream 1's first
	// batch moves there.
	idx.mu.Lock()
	v2 := idx.docs["ferun-logs-v2-30d"]
	idx.docs["ferun-logs-30d"], idx.docs["ferun-logs-v2-30d"] = v2[:10], v2[10:]
	idx.mu.Unlock()
	// A line sent again after it was indexed is not stored again.
	if err := s.Ingest(ctx, batch(key1, envLabels("acme", "env1", 0), lines(0, base, 1, ln("a")))); err != nil {
		t.Fatal(err)
	}

	all := mustRead(t, s, "acme", url.Values{"environment": {"env1"}, "limit": {"100"}})
	if len(all.lines) != 60 || all.cursor != nil || all.partial {
		t.Fatalf("%d lines, cursor %v, partial %v", len(all.lines), all.cursor, all.partial)
	}
	page := all.page(false)
	for i := 1; i < len(page.Lines); i++ {
		if page.Lines[i].TS < page.Lines[i-1].TS {
			t.Fatalf("lines not oldest first at %d", i)
		}
	}
	if len(page.Streams) != 2 {
		t.Errorf("%d streams", len(page.Streams))
	}

	// Pages backward and forward meet every line once.
	for _, dir := range []string{"backward", "forward"} {
		seen := map[string]bool{}
		v := url.Values{"environment": {"env1"}, "limit": {"7"}, "direction": {dir}}
		if dir == "forward" {
			v.Set("since", fmt.Sprint(base-int64(time.Hour)))
		}
		for pages := 0; ; pages++ {
			if pages > 20 {
				t.Fatal("too many pages")
			}
			r := mustRead(t, s, "acme", v)
			for _, x := range texts(r, dir == "forward") {
				if seen[x] {
					t.Fatalf("%s: %q twice", dir, x)
				}
				seen[x] = true
			}
			if r.cursor == nil {
				break
			}
			v.Set("cursor", encodeCursor(dir == "forward", *r.cursor))
		}
		if len(seen) != 60 {
			t.Errorf("%s: %d lines", dir, len(seen))
		}
	}

	// Filters, across both stages.
	errs := mustRead(t, s, "acme", url.Values{"environment": {"env1"}, "level": {"error"}})
	if got := strings.Join(texts(errs, false), "|"); got != "ERROR a line 07|ERROR b line 07|ERROR b line 17|ERROR a line 17|ERROR b line 27|ERROR a line 27" {
		t.Errorf("errors: %s", got)
	}
	words := mustRead(t, s, "acme", url.Values{"environment": {"env1"}, "q": {"A -error"}, "replica": {"0"}})
	if len(words.lines) != 27 {
		t.Errorf("words: %d lines", len(words.lines))
	}
	re := mustRead(t, s, "acme", url.Values{"instance": {"inst0"}, "re": {`line 2[0-4]$`}})
	if len(re.lines) != 5 {
		t.Errorf("re: %d lines", len(re.lines))
	}

	// The scan limit makes a partial page with a cursor that goes on.
	small := *s
	small.cfg.ScanLimit = 100
	q, _ := parseQuery(url.Values{"environment": {"env1"}, "q": {"zzz"}}, time.Now())
	r, err := small.Read(ctx, "acme", q)
	if err != nil {
		t.Fatal(err)
	}
	if !r.partial || r.cursor == nil || len(r.lines) != 0 {
		t.Errorf("scan limit: partial %v cursor %v lines %d", r.partial, r.cursor, len(r.lines))
	}

	// Streams list.
	sts, err := s.Streams(ctx, "acme", Selector{Environment: "env1"}, 0)
	if err != nil || len(sts) != 2 || sts[0].Lines != 30 {
		t.Fatalf("streams %v, %v", sts, err)
	}
}

func TestTraces(t *testing.T) {
	cfg := testConfig()
	s, _ := newTestStore(t, cfg)
	ctx := context.Background()
	other := "0af7651916cd43dd8448eb211c80319c"
	text := func(i int) string {
		tr := other
		if i == 3 {
			tr = traceA
		}
		return fmt.Sprintf(`{"level":"info","i":%d,"trace_id":"%s"}`, i, tr)
	}
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "1"}, envLabels("acme", "e", 0), lines(0, base-int64(48*time.Hour), 5, text))); err != nil {
		t.Fatal(err)
	}
	indexAll(t, s)
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "1"}, envLabels("acme", "e", 0), lines(100, base, 5, text))); err != nil {
		t.Fatal(err)
	}
	// Two days back, without since: the index finds the old one, the hot
	// stage the new.
	r := mustRead(t, s, "acme", url.Values{"environment": {"e"}, "trace": {traceA}})
	if len(r.lines) != 2 {
		t.Errorf("trace: %d lines: %v", len(r.lines), texts(r, false))
	}
}

func TestRetentionAndDelete(t *testing.T) {
	cfg := testConfig()
	cfg.Retention = 24 * time.Hour
	cfg.RetentionMax = 72 * time.Hour
	s, idx := newTestStore(t, cfg)
	ctx := context.Background()
	old := base - int64(30*time.Hour)
	text := func(i int) string { return fmt.Sprintf("line %d", i) }
	// Default retention: expires. retention_days 2: kept. 400: clamped to
	// 3 days, expires.
	short, long := envLabels("acme", "e", 0), envLabels("acme", "e", 1)
	long.RetentionDays = 2
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "short"}, short, lines(0, old, 3, text))); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "long"}, long, lines(0, old, 3, text))); err != nil {
		t.Fatal(err)
	}
	ancient := envLabels("acme", "e", 2)
	ancient.RetentionDays = 400
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "ancient"}, ancient, lines(0, base-int64(100*time.Hour), 1, text))); err != nil {
		t.Fatal(err)
	}
	indexAll(t, s)
	// An index per retention: the default's, and the longest's for the
	// streams that ask for more.
	if ids, _ := idx.Indexes(ctx, "ferun-logs-"); fmt.Sprint(ids) != "[ferun-logs-v2-1d ferun-logs-v2-3d]" || idx.count("acme") != 7 {
		t.Fatalf("indexes %v, %d documents", ids, idx.count("acme"))
	}
	// A hot line past retention goes too.
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "short"}, short, lines(100, old, 1, text))); err != nil {
		t.Fatal(err)
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var streams []string
	rows, _ := s.db.Pool.Query(ctx, `SELECT log_id FROM log_streams ORDER BY log_id`)
	for rows.Next() {
		var x string
		_ = rows.Scan(&x)
		streams = append(streams, x)
	}
	rows.Close()
	if fmt.Sprint(streams) != "[long]" {
		t.Errorf("after the sweep: streams %v", streams)
	}
	// The index keeps what its own retention keeps; a read finds only the
	// streams left.
	r := mustRead(t, s, "acme", url.Values{"environment": {"e"}, "since": {fmt.Sprint(base - int64(200*time.Hour))}})
	if len(r.lines) != 3 {
		t.Errorf("after the sweep: %d lines read: %v", len(r.lines), texts(r, false))
	}

	// DELETE of an environment's streams: rows, lines, a delete task.
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "new"}, short, lines(0, base, 2, text))); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "job"}, Labels{Tenant: "acme", Job: "j1", Attrs: map[string]string{}},
		lines(0, base, 2, text))); err != nil {
		t.Fatal(err)
	}
	// An index of before (version 1) gets its delete task too.
	if err := idx.EnsureIndex(ctx, "ferun-logs-1d", quickwit.IndexSettings{}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Ingest(ctx, "ferun-logs-1d", []quickwit.Doc{{Tenant: "acme", Environment: "e", Stream: "s_v1", Text: "v1"}}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DeleteStreams(ctx, "other", "e", ""); err != nil || n != 0 {
		t.Fatalf("another tenant deleted %d: %v", n, err)
	}
	n, err := s.DeleteStreams(ctx, "acme", "e", "")
	if err != nil || n != 2 {
		t.Fatalf("deleted %d: %v", n, err)
	}
	var left, hot int
	_ = s.db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM log_streams), (SELECT sum(lines) FROM log_batches)`).Scan(&left, &hot)
	envDocs, _ := idx.Search(ctx, "ferun-logs-*", quickwitQuery("acme", "e"))
	if left != 1 || hot != 2 || len(envDocs) != 0 {
		t.Errorf("after delete: %d streams, %d hot lines, %d documents", left, hot, len(envDocs))
	}
}

func TestPartitions(t *testing.T) {
	s, _ := newTestStore(t, testConfig())
	ctx := context.Background()
	// Of each hot table, an empty partition of last week and one with a
	// row.
	for _, table := range hotTables {
		for _, d := range []string{"2026-01-01", "2026-01-02"} {
			dd, _ := time.Parse(time.DateOnly, d)
			if _, err := s.db.Pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				partitionName(table, dd), table, d, dd.AddDate(0, 0, 1).Format(time.DateOnly))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "1"}, envLabels("acme", "e", 0), lines(0, base, 1, func(int) string { return "x" }))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Pool.Exec(ctx, `INSERT INTO log_lines (day, stream_id, off, ts, source, level, partial, text)
		SELECT '2026-01-02', id, 99, 1, '', 16, false, 'old' FROM log_streams`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Pool.Exec(ctx, `INSERT INTO log_batches (day, stream_id, first_off, last_off, min_ts, max_ts, lines, bytes, levels, data)
		SELECT '2026-01-02', id, 98, 98, 1, 1, 1, 3, 16, $1 FROM log_streams`,
		encodeBatch([]Line{{Off: 98, TS: 1, Level: LevelUnknown, Text: "old"}})); err != nil {
		t.Fatal(err)
	}
	dropped, err := maintainPartitions(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(dropped) != "[log_batches_20260101 log_lines_20260101]" {
		t.Errorf("dropped %v", dropped)
	}
	// Every partition of log_batches compresses its data with lz4.
	var parts, lz4 int
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE a.attcompression = 'l')
		FROM pg_inherits i JOIN pg_attribute a ON a.attrelid = i.inhrelid AND a.attname = 'data'
		WHERE i.inhparent = 'log_batches'::regclass`).Scan(&parts, &lz4); err != nil {
		t.Fatal(err)
	}
	if parts != daysAhead+2 || lz4 != parts {
		t.Errorf("%d partitions, %d with lz4", parts, lz4)
	}
}

func TestIndexClaimsAcrossInstances(t *testing.T) {
	cfg := testConfig()
	d := newTestDB(t)
	idx := newMemIndex()
	ctx := context.Background()
	stores := make([]*Store, 4)
	for i := range stores {
		c := cfg
		c.Name = fmt.Sprintf("sink-%d", i)
		stores[i] = NewStore(d, idx, c, dbtest.Logger(t))
	}
	const streams, rounds = 20, 5
	for r := 0; r < rounds; r++ {
		for i := 0; i < streams; i++ {
			if err := stores[0].Ingest(ctx, batch(StreamKey{fmt.Sprintf("pod%d", i), "c", "1"}, envLabels("acme", "e", i),
				lines(int64(r)*100, base+int64(r)*int64(time.Second), 5, func(j int) string { return fmt.Sprintf("r%d l%d", r, j) }))); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		errc := make(chan error, len(stores))
		for _, s := range stores {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					n, err := s.indexRound(ctx)
					if err != nil {
						errc <- err
						return
					}
					if n == 0 {
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errc)
		for err := range errc {
			t.Fatal(err)
		}
	}
	var hot int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM log_batches`).Scan(&hot); err != nil {
		t.Fatal(err)
	}
	if n := idx.count("acme"); hot != 0 || n != streams*rounds*5 {
		t.Errorf("hot %d, indexed %d", hot, n)
	}
	r := mustRead(t, stores[1], "acme", url.Values{"environment": {"e"}, "limit": {"5000"}, "since": {fmt.Sprint(base - 1)}})
	if len(r.lines) != streams*rounds*5 {
		t.Errorf("read %d lines", len(r.lines))
	}
}

func TestIndexFailure(t *testing.T) {
	cfg := testConfig()
	s, idx := newTestStore(t, cfg)
	ctx := context.Background()
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "1"}, envLabels("acme", "e", 0), lines(0, base, 3, func(int) string { return "x" }))); err != nil {
		t.Fatal(err)
	}
	idx.failIngest = errors.New("index down")
	if _, err := s.indexRound(ctx); err == nil {
		t.Fatal("no error")
	}
	// The claim holds for the backoff: no round takes the stream at once.
	idx.failIngest = nil
	if n, err := s.indexRound(ctx); err != nil || n != 0 {
		t.Fatalf("claimed again at once: %d, %v", n, err)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE log_streams SET chunk_lease = now() - interval '1s'`); err != nil {
		t.Fatal(err)
	}
	if n := indexAll(t, s); n != 1 {
		t.Fatalf("%d streams indexed after the lease ran out", n)
	}
	// Lines stay readable throughout.
	if r := mustRead(t, s, "acme", url.Values{"environment": {"e"}}); len(r.lines) != 3 {
		t.Errorf("%d lines", len(r.lines))
	}
}

// followRecorder collects a follow's output.
type followRecorder struct {
	mu      sync.Mutex
	lines   []string
	streams int
	beats   int
}

func (f *followRecorder) Line(l Line, st *streamRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, l.Text)
	return nil
}

func (f *followRecorder) Heartbeat() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats++
	return nil
}

func (f *followRecorder) snapshot() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...), f.beats
}

func TestFollow(t *testing.T) {
	cfg := testConfig()
	cfg.FollowPoll = 20 * time.Millisecond
	cfg.Heartbeat = 50 * time.Millisecond
	s, _ := newTestStore(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := StreamKey{"p", "c", "1"}
	text := func(prefix string) func(int) string {
		return func(i int) string { return fmt.Sprintf("%s%d", prefix, i) }
	}
	if err := s.Ingest(ctx, batch(key, envLabels("acme", "e", 0), lines(0, base, 3, text("old")))); err != nil {
		t.Fatal(err)
	}
	q, _ := parseQuery(url.Values{"environment": {"e"}, "follow": {"1"}, "q": {"-skip"}}, time.Now())
	rec := &followRecorder{}
	done := make(chan error, 1)
	go func() { done <- s.Follow(ctx, "acme", q, rec) }()
	waitFor(t, func() bool { l, _ := rec.snapshot(); return len(l) == 3 })
	// New lines, some indexed before the follow sees them, a new
	// stream, and a line the filter drops.
	if err := s.Ingest(ctx, batch(key, envLabels("acme", "e", 0), lines(100, base+int64(time.Second), 3, text("new")))); err != nil {
		t.Fatal(err)
	}
	indexAll(t, s)
	if err := s.Ingest(ctx, batch(key, envLabels("acme", "e", 0), lines(200, base+int64(2*time.Second), 2, text("hot")))); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, batch(StreamKey{"p2", "c", "1"}, envLabels("acme", "e", 1), append(
		lines(0, base+int64(3*time.Second), 1, text("second")), lines(10, base+int64(4*time.Second), 1, text("skip"))...))); err != nil {
		t.Fatal(err)
	}
	// A line in two parts, in two batches: given once, joined.
	if err := s.Ingest(ctx, batch(key, envLabels("acme", "e", 0), []Line{
		{Off: 300, TS: base + int64(5*time.Second), Source: "stdout", Level: LevelUnknown, Partial: true, Text: "part1 "}}),
	); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * cfg.FollowPoll)
	if err := s.Ingest(ctx, batch(key, envLabels("acme", "e", 0), []Line{
		{Off: 310, TS: base + int64(6*time.Second), Source: "stdout", Level: LevelUnknown, Text: "part2"}}),
	); err != nil {
		t.Fatal(err)
	}
	want := "old0|old1|old2|new0|new1|new2|hot0|hot1|second0|part1 part2"
	waitFor(t, func() bool { l, _ := rec.snapshot(); return strings.Join(l, "|") == want })
	waitFor(t, func() bool { _, b := rec.snapshot(); return b > 0 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if l, _ := rec.snapshot(); strings.Join(l, "|") != want {
		t.Errorf("follow gave %v", l)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
