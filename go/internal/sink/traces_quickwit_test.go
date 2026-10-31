// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	tracev1 "github.com/gardenlinux/feos/go/gen/otlp/trace/v1"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// TestTraceIndexQuickwit runs against the Quickwit at
// FERUN_TEST_QUICKWIT_URL: the trace index's configuration is taken, the
// sink's documents are indexed and come back as they went, and the
// searches the reads make answer as SpanQuery.Match does.
func TestTraceIndexQuickwit(t *testing.T) {
	u := os.Getenv("FERUN_TEST_QUICKWIT_URL")
	if u == "" {
		t.Skip("FERUN_TEST_QUICKWIT_URL is not set; skipping a test that needs Quickwit")
	}
	c, err := quickwit.New(u, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	prefix := fmt.Sprintf("test-traces-%d", time.Now().UnixNano())
	id := prefix + "-1d"
	for range 2 {
		if err := c.EnsureTraceIndex(ctx, id, quickwit.IndexSettings{RetentionDays: 1, CommitSecs: 1, Partitions: 4}); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		req, _ := http.NewRequest(http.MethodDelete, u+"/api/v1/indexes/"+id, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()

	start := time.Now().Add(-time.Minute).UnixNano()
	i64 := func(n int64) *commonv1.AnyValue {
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: n}}
	}
	dbl := func(f float64) *commonv1.AnyValue {
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: f}}
	}
	root := testSpan(traceA, "000000000000000a", "", "GET /", start, 30*time.Millisecond)
	root.TraceState, root.Flags = "k=v", 1
	root.Status = &tracev1.Status{Code: tracev1.Status_STATUS_CODE_ERROR, Message: "upstream reset"}
	root.Attributes = []*commonv1.KeyValue{kv("http.response.status_code", i64(503)), kv("ratio", dbl(2)), kv("big", i64(1<<60)),
		kv("a b:c", str("odd key")), kv("x-dash", str("v")),
		kv("list", &commonv1.AnyValue{Value: &commonv1.AnyValue_ArrayValue{ArrayValue: &commonv1.ArrayValue{
			Values: []*commonv1.AnyValue{str("a"), i64(1)}}}}),
		kv("map", &commonv1.AnyValue{Value: &commonv1.AnyValue_KvlistValue{KvlistValue: &commonv1.KeyValueList{
			Values: []*commonv1.KeyValue{kv("k", str("v"))}}}}),
		kv("raw", &commonv1.AnyValue{Value: &commonv1.AnyValue_BytesValue{BytesValue: []byte{0, 1}}})}
	root.Events = []*tracev1.Span_Event{{TimeUnixNano: uint64(start + 1000), Name: "retry", Attributes: []*commonv1.KeyValue{kv("n", i64(1))}}}
	root.Links = []*tracev1.Span_Link{{TraceId: mustHex(traceB), SpanId: mustHex("000000000000000c")}}
	child := testSpan(traceA, "000000000000000b", "000000000000000a", "query", start+int64(time.Millisecond), 5*time.Millisecond)
	other := testSpan(traceB, "000000000000000c", "", "POST /x", start+int64(10*time.Second), time.Second)
	var docs []quickwit.SpanDoc
	var raws []json.RawMessage
	for _, r := range []struct {
		attrs map[string]string
		spans []*tracev1.Span
	}{
		{podAttrs("acme", "env1", "api"), []*tracev1.Span{root, child}},
		{podAttrs("acme", "env2", "web"), []*tracev1.Span{other}},
		{podAttrs("other", "env1", "api"), []*tracev1.Span{testSpan(traceA, "000000000000000d", "", "GET /", start, 0)}},
	} {
		spans, _, err := parseTraceExport(traceRequest(r.attrs, r.spans...), ingestPeer{node: "node-1"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range spans {
			docs, raws = append(docs, s.doc), append(raws, s.raw)
		}
	}
	if err := c.IngestSpans(ctx, id, raws); err != nil {
		t.Fatal(err)
	}
	pattern := prefix + "-*"

	got, err := c.SearchSpans(ctx, pattern, quickwit.SpanQuery{Tenant: "acme", TraceID: traceA, Max: 100})
	if err != nil || len(got) != 2 {
		t.Fatalf("trace A: %+v, %v", got, err)
	}
	// A document comes back as it went, and reads as Jaeger's the same.
	want, _ := json.Marshal(jaegerTraceOf(docs[:2]))
	back, _ := json.Marshal(jaegerTraceOf(got))
	if string(want) != string(back) {
		t.Errorf("the trace from the index:\n got %s\nwant %s", back, want)
	}
	if !reflect.DeepEqual(got[0].Events, docs[0].Events) || got[0].TraceState != "k=v" || got[0].Flags != 1 {
		t.Errorf("events or fields: %+v", got[0])
	}

	all := quickwit.SpanQuery{Tenant: "acme", SinceNs: start - int64(time.Hour), UntilNs: time.Now().UnixNano()}
	for _, q := range []quickwit.SpanQuery{
		all,
		{Environment: "env1"}, {Source: SourceIngress}, {Service: "web"}, {Operation: "query"},
		{MinDurationNs: int64(20 * time.Millisecond)}, {MaxDurationNs: int64(10 * time.Millisecond)},
		{SinceNs: start + int64(time.Millisecond), UntilNs: start + int64(time.Second)},
		{Tags: map[string]string{"http.response.status_code": "503"}},
		{Tags: map[string]string{"ratio": "2"}},
		{Tags: map[string]string{"a b:c": "odd key", "x-dash": "v"}},
		{Tags: map[string]string{"ferun.environment": "env2"}},
		{Tags: map[string]string{"big": "1152921504606846976"}},
		{Tags: map[string]string{"http.response.status_code": "500"}},
	} {
		q.Tenant = "acme"
		if q.SinceNs == 0 {
			q.SinceNs, q.UntilNs = all.SinceNs, all.UntilNs
		}
		hits, err := c.NewestTraces(ctx, pattern, q, 10)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var gotIDs, wantIDs []string
		for _, h := range hits {
			gotIDs = append(gotIDs, h.TraceID)
		}
		newest := map[string]int64{}
		for i := range docs {
			if q.Match(&docs[i]) {
				newest[docs[i].TraceID] = max(newest[docs[i].TraceID], docs[i].StartNs)
			}
		}
		for _, tid := range []string{traceB, traceA} { // traceB's span is the newest
			if _, ok := newest[tid]; ok {
				wantIDs = append(wantIDs, tid)
			}
		}
		if strings.Join(gotIDs, " ") != strings.Join(wantIDs, " ") {
			t.Errorf("%s: %v, want %v", q, gotIDs, wantIDs)
		}
	}
	svcs, err := c.SpanTerms(ctx, pattern, all, "service_name", 100)
	if err != nil || strings.Join(slices.Sorted(slices.Values(svcs)), " ") != "api web" {
		t.Errorf("services %v, %v", svcs, err)
	}
	q := all
	q.Service = "api"
	ops, err := c.SpanTerms(ctx, pattern, q, "span_name", 100)
	if err != nil || strings.Join(slices.Sorted(slices.Values(ops)), ",") != "GET /,query" {
		t.Errorf("operations %v, %v", ops, err)
	}
	if err := c.IngestSpans(ctx, "no-such-index", raws[:1]); !quickwit.IsNotFound(err) {
		t.Errorf("ingest into no index: %v", err)
	}

	// The indexer's way, through trace_spans' jsonb, keeps them as well.
	if os.Getenv("FERUN_TEST_DATABASE_URL") == "" {
		return
	}
	cfg := testConfig()
	cfg.TraceIndexPrefix, cfg.TraceRetention, cfg.CommitTimeout = prefix+"-db", 24*time.Hour, time.Second
	tr := NewTraces(newTestDB(t), c, cfg, dbtest.Logger(t))
	spans, _, err := parseTraceExport(traceRequest(podAttrs("acme", "env1", "api"), root, child), ingestPeer{node: "node-1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Ingest(ctx, spans); err != nil {
		t.Fatal(err)
	}
	dbID, _ := tr.indexID()
	defer func() {
		req, _ := http.NewRequest(http.MethodDelete, u+"/api/v1/indexes/"+dbID, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	if n, err := tr.indexRound(ctx); err != nil || n != 2 {
		t.Fatalf("index round: %d, %v", n, err)
	}
	got, err = tr.Trace(ctx, quickwit.SpanQuery{Tenant: "acme", TraceID: traceA})
	if back, _ := json.Marshal(jaegerTraceOf(got)); err != nil || string(back) != string(want) {
		t.Errorf("the trace through trace_spans:\n got %s\nwant %s (%v)", back, want, err)
	}
}
