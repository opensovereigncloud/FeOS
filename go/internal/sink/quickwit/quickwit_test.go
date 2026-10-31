// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package quickwit

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestQueryString(t *testing.T) {
	two, off, to := 2, int64(10), int64(30)
	for _, c := range []struct {
		q    Query
		want string
	}{
		{Query{}, "*"},
		{Query{Tenant: "acme", Environment: `e"1`, Replica: &two}, `tenant:"acme" AND environment:"e\"1" AND replica:2`},
		{Query{Tenant: "t", SinceNs: 1, UntilNs: 2e9}, `tenant:"t" AND ts:[1970-01-01T00:00:00.000000001Z TO 1970-01-01T00:00:02.000000000Z]`},
		{Query{Stream: "s_1", OffAfter: &off, OffTo: &to}, `stream:"s_1" AND off:{10 TO 30]`},
		{Query{Levels: 1 | 2}, `level:IN [1 2]`},
		{Query{Levels: 31}, `*`},
		{Query{Words: []string{"user_id=42", `a\b`}, NotWords: []string{"x"}},
			`text:"user_id=42" AND text:"a\\b" AND NOT text:"x"`},
		{Query{Words: []string{"w"}, OrPartial: true}, `((text:"w") OR partial:true)`},
	} {
		if got := c.q.String(); got != c.want {
			t.Errorf("%+v:\n got %s\nwant %s", c.q, got, c.want)
		}
	}
}

// TestQuickwit runs against the Quickwit at FERUN_TEST_QUICKWIT_URL (one
// node of every service, e.g. `quickwit run` in its container).
func TestQuickwit(t *testing.T) {
	u := os.Getenv("FERUN_TEST_QUICKWIT_URL")
	if u == "" {
		t.Skip("FERUN_TEST_QUICKWIT_URL is not set; skipping a test that needs Quickwit")
	}
	c, err := New(u, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := fmt.Sprintf("test-logs-%d-1d", time.Now().UnixNano())
	if err := c.EnsureIndex(ctx, id, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureIndex(ctx, id, 1, 1); err != nil {
		t.Fatalf("again: %v", err)
	}
	defer func() { _ = c.do(context.Background(), "DELETE", c.search+"/api/v1/indexes/"+id, "", nil, nil) }()
	now := time.Now().UnixNano()
	docs := []Doc{
		{TS: now, Tenant: "acme", Stream: "s_1", Environment: "e", Source: "container", IOStream: "stdout", Level: 1, Off: 10,
			Text: "GET /x user_id=42 ERROR connection reset", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736"},
		{TS: now + 1, Tenant: "acme", Stream: "s_1", Environment: "e", Source: "container", IOStream: "stdout", Level: 4, Off: 20,
			Text: "second line", Partial: true},
		{TS: now + 1, Tenant: "acme", Stream: "s_2", Environment: "e", Source: "container", IOStream: "stderr", Level: 4, Off: 20,
			Text: "other stream"},
		{TS: now, Tenant: "other", Stream: "s_3", Environment: "e", Source: "container", Level: 1, Off: 0, Text: "ERROR not acme's"},
	}
	if err := c.Ingest(ctx, id, docs); err != nil {
		t.Fatal(err)
	}
	pattern := strings.TrimSuffix(id, "1d") + "*"
	search := func(q Query) string {
		t.Helper()
		if q.Max == 0 {
			q.Max = 100
		}
		hits, err := c.Search(ctx, pattern, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, fmt.Sprintf("%s/%d", h.Stream, h.Off))
		}
		return strings.Join(out, " ")
	}
	for _, x := range []struct {
		q    Query
		want string
	}{
		{Query{Tenant: "acme"}, "s_2/20 s_1/20 s_1/10"},
		{Query{Tenant: "acme", Order: OldestFirst}, "s_1/10 s_1/20 s_2/20"},
		{Query{Tenant: "acme", Words: []string{"user_id=42"}}, "s_1/10"},
		{Query{Tenant: "acme", Words: []string{"erro"}}, ""},
		{Query{Tenant: "acme", Words: []string{"reset"}, OrPartial: true}, "s_1/20 s_1/10"},
		{Query{Tenant: "acme", NotWords: []string{"second"}}, "s_2/20 s_1/10"},
		{Query{Tenant: "acme", Levels: 1}, "s_1/10"},
		{Query{Tenant: "acme", Trace: "4bf92f3577b34da6a3ce929d0e0e4736"}, "s_1/10"},
		{Query{Tenant: "acme", SinceNs: now + 1, UntilNs: now + 1}, "s_2/20 s_1/20"},
		{Query{Tenant: "acme", Stream: "s_1", OffAfter: new(int64), OffTo: ptr(int64(15)), Order: ByOffset}, "s_1/10"},
		{Query{Tenant: "acme", IOStream: "stderr"}, "s_2/20"},
		{Query{Tenant: "acme", Max: 1}, "s_2/20"},
	} {
		if got := search(x.q); got != x.want {
			t.Errorf("%s: got %q, want %q", x.q, got, x.want)
		}
	}
	hits, err := c.Search(ctx, pattern, Query{Tenant: "acme", Stream: "s_1", Max: 1})
	if err != nil || len(hits) != 1 || hits[0].TS != now+1 || !hits[0].Partial || hits[0].Text != "second line" {
		t.Errorf("a hit's fields: %+v, %v", hits, err)
	}
	ids, err := c.Indexes(ctx, "test-logs-")
	if err != nil || !strings.Contains(strings.Join(ids, " "), id) {
		t.Errorf("indexes %v, %v", ids, err)
	}
	if err := c.Delete(ctx, id, Query{Tenant: "acme", Environment: "e"}); err != nil {
		t.Errorf("delete task: %v", err)
	}
	if err := c.Ingest(ctx, "no-such-index", docs[:1]); !IsNotFound(err) {
		t.Errorf("ingest into no index: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
