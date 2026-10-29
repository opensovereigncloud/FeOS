// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A batch is stored once, its records attributed to their
// environments' owners, numbered from its Seq; records of environments
// that do not exist, or whose environment is not an ID, are skipped; text
// is cut to the wire's bounds.
func TestPublishAccessLogs(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.world()
	env := h.mkEnv("web", webSpec, true)
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	rec := func(env, path string, at time.Duration) wire.AccessLogEntry {
		return wire.AccessLogEntry{Time: t0.Add(at), DurationNs: 1500, Env: env, Service: "web", Instance: "i1",
			Host: "app.apps.example", Method: "GET", Path: path, Proto: "HTTP/2.0", Status: 200, Outcome: "2xx",
			BytesOut: 12, ClientIP: "203.0.113.9", UserAgent: "ua\x00x", Attempts: 1,
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7"}
	}
	b := &wire.AccessLogBatch{Start: uuid.NewString(), Seq: 10, Records: []wire.AccessLogEntry{
		rec(env.ID.String(), "/a", 0),
		rec(uuid.NewString(), "/gone", time.Second),
		rec("bogus", "/bogus", 2*time.Second),
		rec(env.ID.String(), "/"+strings.Repeat("p", 2*wire.MaxAccessLogPath), 3*time.Second),
	}}
	for range 2 {
		if _, err := c.api.PublishAccessLogs(bg, b); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := h.st.ListEnvAccessLogs(bg, env.ID, store.AccessLogQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Seq != 10 || rows[1].Seq != 13 || r.Ingress != "in-0" || r.Owner != "acme" || r.Path != "/a" ||
		r.TsNs != t0.UnixNano() || r.UserAgent != "uax" || r.Status != 200 || r.Attempts != 1 ||
		r.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || r.Proto != "HTTP/2.0" {
		t.Errorf("row %+v", r)
	}
	if n := len(rows[1].Path); n != wire.MaxAccessLogPath {
		t.Errorf("path of %d bytes, want %d", n, wire.MaxAccessLogPath)
	}

	// A page: the newest before a time, ascending; of a trace.
	page, err := h.st.ListEnvAccessLogs(bg, env.ID, store.AccessLogQuery{Limit: 1, BeforeTsNs: rows[1].TsNs})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Path != "/a" {
		t.Errorf("page before the last %+v", page)
	}
	page, err = h.st.ListEnvAccessLogs(bg, env.ID, store.AccessLogQuery{Limit: 10, TraceID: strings.Repeat("0", 32)})
	if err != nil || len(page) != 0 {
		t.Errorf("another trace: %+v, %v", page, err)
	}
}

func TestPublishAccessLogsInvalid(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	_, err := c.api.PublishAccessLogs(bg, &wire.AccessLogBatch{Start: "nope"})
	wantCode(t, "start", err, codes.InvalidArgument, "not a UUID")
	_, err = c.api.PublishAccessLogs(bg, &wire.AccessLogBatch{Start: uuid.NewString(), Seq: -1})
	wantCode(t, "seq", err, codes.InvalidArgument, "negative")
	_, err = c.api.PublishAccessLogs(bg, &wire.AccessLogBatch{Start: uuid.NewString(),
		Records: slices.Repeat([]wire.AccessLogEntry{{}}, wire.MaxAccessLogRecords+1)})
	wantCode(t, "records", err, codes.InvalidArgument, "more than 500")
}
