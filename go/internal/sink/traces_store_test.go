// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	otlptracev1 "github.com/gardenlinux/feos/go/gen/otlp/collector/trace/v1"
	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	tracev1 "github.com/gardenlinux/feos/go/gen/otlp/trace/v1"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

func newTestTraces(t *testing.T, cfg Config) (*Traces, *memTraceIndex) {
	t.Helper()
	idx := newMemTraceIndex()
	return NewTraces(newTestDB(t), idx, cfg, dbtest.Logger(t)), idx
}

// ingestSpans parses a request of a node's and stores it.
func ingestSpans(t *testing.T, tr *Traces, req *otlptracev1.ExportTraceServiceRequest) error {
	t.Helper()
	spans, _, err := parseTraceExport(req, ingestPeer{node: "node-1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tr.Ingest(context.Background(), spans)
}

func hotCount(t *testing.T, tr *Traces) int {
	t.Helper()
	var n int
	if err := tr.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM trace_spans`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func spanIDs(spans []quickwit.SpanDoc) string {
	s := ""
	for _, d := range spans {
		s += d.SpanID[12:] + " "
	}
	return s
}

func TestTraceStore(t *testing.T) {
	tr, idx := newTestTraces(t, testConfig())
	ctx := context.Background()
	start := time.Now().Add(-10 * time.Minute).UnixNano()
	root := testSpan(traceA, "000000000000000a", "", "GET /", start, 20*time.Millisecond)
	root.Attributes = []*commonv1.KeyValue{kv("http.response.status_code", &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: 500}})}
	child := testSpan(traceA, "000000000000000b", "000000000000000a", "query", start+int64(time.Millisecond), 5*time.Millisecond)
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "env1", "api"), root, child)); err != nil {
		t.Fatal(err)
	}
	other := testSpan(traceB, "000000000000000c", "", "POST /x", start+int64(time.Minute), time.Second)
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "env2", "web"), other)); err != nil {
		t.Fatal(err)
	}
	notAcme := testSpan(traceA, "000000000000000d", "", "GET /", start, time.Millisecond)
	if err := ingestSpans(t, tr, traceRequest(podAttrs("other", "env1", "api"), notAcme)); err != nil {
		t.Fatal(err)
	}
	if n := hotCount(t, tr); n != 4 {
		t.Fatalf("%d hot spans", n)
	}
	var used int64
	if err := tr.db.Pool.QueryRow(ctx, `SELECT bytes FROM trace_usage WHERE tenant = 'acme'`).Scan(&used); err != nil || used < 300 {
		t.Errorf("usage %d, %v", used, err)
	}

	// reads answers the same before and after the index took the spans.
	reads := func(when string) {
		t.Helper()
		spans, err := tr.Trace(ctx, quickwit.SpanQuery{Tenant: "acme", TraceID: traceA})
		if err != nil || spanIDs(spans) != "000a 000b " {
			t.Errorf("%s: trace A: %s %v", when, spanIDs(spans), err)
		}
		if spans, err := tr.Trace(ctx, quickwit.SpanQuery{Tenant: "acme", TraceID: traceA, Environment: "env2"}); err != nil || spans != nil {
			t.Errorf("%s: trace A of env2: %s %v", when, spanIDs(spans), err)
		}
		since := start - int64(time.Hour)
		for _, c := range []struct {
			q    quickwit.SpanQuery
			want string
		}{
			{quickwit.SpanQuery{}, traceB + " " + traceA},
			{quickwit.SpanQuery{Service: "api"}, traceA},
			{quickwit.SpanQuery{Operation: "query"}, traceA},
			{quickwit.SpanQuery{Tags: map[string]string{"http.response.status_code": "500"}}, traceA},
			{quickwit.SpanQuery{Tags: map[string]string{"ferun.environment": "env2"}}, traceB},
			{quickwit.SpanQuery{MinDurationNs: int64(500 * time.Millisecond)}, traceB},
			{quickwit.SpanQuery{MaxDurationNs: int64(10 * time.Millisecond)}, traceA},
			{quickwit.SpanQuery{Environment: "env1"}, traceA},
			{quickwit.SpanQuery{Source: SourceIngress}, ""},
			{quickwit.SpanQuery{UntilNs: start + int64(time.Second)}, traceA},
		} {
			q := c.q
			q.Tenant, q.SinceNs = "acme", since
			if q.UntilNs == 0 {
				q.UntilNs = time.Now().UnixNano()
			}
			found, err := tr.Find(ctx, q, 20)
			got := ""
			for i, spans := range found {
				if i > 0 {
					got += " "
				}
				got += spans[0].TraceID
			}
			if err != nil || got != c.want {
				t.Errorf("%s: find %+v: %q %v, want %q", when, c.q, got, err, c.want)
			}
		}
		if found, err := tr.Find(ctx, quickwit.SpanQuery{Tenant: "acme", SinceNs: since, UntilNs: time.Now().UnixNano()}, 1); err != nil ||
			len(found) != 1 || found[0][0].TraceID != traceB {
			t.Errorf("%s: find 1: %v", when, err)
		}
		// A trace found by one span answers its spans of the selectors.
		if found, err := tr.Find(ctx, quickwit.SpanQuery{Tenant: "acme", Operation: "query", SinceNs: since}, 20); err != nil ||
			len(found) != 1 || spanIDs(found[0]) != "000a 000b " {
			t.Errorf("%s: whole trace: %v", when, err)
		}
		svcs, err := tr.Services(ctx, quickwit.SpanQuery{Tenant: "acme"}, since)
		if err != nil || fmt.Sprint(svcs) != "[api web]" {
			t.Errorf("%s: services %v %v", when, svcs, err)
		}
		ops, err := tr.Operations(ctx, quickwit.SpanQuery{Tenant: "acme", Service: "api"}, since)
		if err != nil || fmt.Sprint(ops) != "[GET / query]" {
			t.Errorf("%s: operations %v %v", when, ops, err)
		}
	}
	reads("hot")
	n, err := tr.indexRound(ctx)
	if err != nil || n != 4 {
		t.Fatalf("index round: %d, %v", n, err)
	}
	if hotCount(t, tr) != 0 || idx.count() != 4 {
		t.Fatalf("after the round: %d hot, %d indexed", hotCount(t, tr), idx.count())
	}
	if n, err := tr.indexRound(ctx); err != nil || n != 0 {
		t.Errorf("an empty round: %d, %v", n, err)
	}
	reads("indexed")
	// A span sent again is in the index and in trace_spans: read once.
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "env1", "api"), child)); err != nil {
		t.Fatal(err)
	}
	reads("twice")
	if _, err := tr.indexRound(ctx); err != nil {
		t.Fatal(err)
	}
	reads("indexed twice")

	// The usage of the days before yesterday goes.
	if _, err := tr.db.Pool.Exec(ctx, `INSERT INTO trace_usage VALUES ('acme', (now() AT TIME ZONE 'utc')::date - 2, 5)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.sweepUsage(ctx); err != nil {
		t.Fatal(err)
	}
	var days int
	if err := tr.db.Pool.QueryRow(ctx, `SELECT count(*) FROM trace_usage WHERE tenant = 'acme'`).Scan(&days); err != nil || days != 1 {
		t.Errorf("usage days %d, %v", days, err)
	}
}

func TestTraceQuota(t *testing.T) {
	cfg := testConfig()
	cfg.TraceQuota = 100
	tr, _ := newTestTraces(t, cfg)
	start := time.Now().UnixNano()
	sp := func(id string) *tracev1.Span { return testSpan(traceA, id, "", "x", start, time.Millisecond) }
	// The request that crosses the quota is stored; the next is refused.
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "e", "api"), sp("0000000000000001"))); err != nil {
		t.Fatal(err)
	}
	if err := ingestSpans(t, tr, traceRequest(podAttrs("acme", "e", "api"), sp("0000000000000002"))); !errors.Is(err, ErrTraceQuota) {
		t.Errorf("over the quota: %v", err)
	}
	if err := ingestSpans(t, tr, traceRequest(podAttrs("other", "e", "api"), sp("0000000000000003"))); err != nil {
		t.Errorf("another tenant: %v", err)
	}
	if n := hotCount(t, tr); n != 2 {
		t.Errorf("%d spans stored", n)
	}
}

func TestTraceIndexClaims(t *testing.T) {
	d := newTestDB(t)
	idx := newMemTraceIndex()
	cfg := testConfig()
	a := NewTraces(d, idx, cfg, dbtest.Logger(t))
	b := NewTraces(d, idx, cfg, dbtest.Logger(t))
	ctx := context.Background()
	start := time.Now().UnixNano()
	spans := make([]*tracev1.Span, traceClaimSpans+10)
	for i := range spans {
		spans[i] = testSpan(traceA, fmt.Sprintf("%016x", i+1), "", "x", start+int64(i), time.Millisecond)
	}
	if err := ingestSpans(t, a, traceRequest(podAttrs("acme", "e", "api"), spans...)); err != nil {
		t.Fatal(err)
	}
	// A failed round keeps its claims for the lease: the other instance
	// finds nothing to claim.
	idx.failIngest = errors.New("down")
	if n, err := a.indexRound(ctx); err == nil || n != 0 {
		t.Fatalf("failed round: %d, %v", n, err)
	}
	idx.failIngest = nil
	n, err := b.indexRound(ctx)
	if err != nil || n != 10 {
		t.Fatalf("beside the claims: %d, %v", n, err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE trace_spans SET claim_lease = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if n, err := b.indexRound(ctx); err != nil || n != traceClaimSpans {
		t.Fatalf("after the lease: %d, %v", n, err)
	}
	if hotCount(t, a) != 0 || idx.count() != len(spans) {
		t.Errorf("%d hot, %d indexed", hotCount(t, a), idx.count())
	}
	// A round bounds the bytes of its documents.
	big := testSpan(traceA, "00000000000fffff", "", "big", start, time.Millisecond)
	big.Attributes = []*commonv1.KeyValue{kv("blob", str(strings.Repeat("x", 4<<20)))}
	big2 := testSpan(traceA, "00000000000ffffe", "", "big", start, time.Millisecond)
	big2.Attributes = big.Attributes
	if err := ingestSpans(t, a, traceRequest(podAttrs("acme", "e", "api"), big, big2)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if n, err := a.indexRound(ctx); err != nil || n != 1 {
			t.Errorf("a round of big spans: %d, %v", n, err)
		}
	}
}
