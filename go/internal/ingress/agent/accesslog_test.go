// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

func entry(path string) wire.AccessLogEntry {
	return wire.AccessLogEntry{Time: time.Now(), Env: "e1", Path: path, Status: 200, Outcome: "2xx"}
}

// Records go in order, numbered across batches of one run.
func TestAccessLogSent(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)
	ctx := context.Background()
	ta.Record(entry("/a"))
	ta.Record(entry("/b"))
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	ta.Record(entry("/c"))
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	bs := h.fake.AccessLogs()
	if len(bs) != 2 || bs[0].Seq != 0 || len(bs[0].Records) != 2 || bs[1].Seq != 2 || len(bs[1].Records) != 1 ||
		bs[0].Start != bs[1].Start || bs[0].Records[1].Path != "/b" || bs[1].Records[0].Path != "/c" {
		t.Fatalf("batches %+v", bs)
	}
	if _, err := uuid.Parse(bs[0].Start); err != nil {
		t.Errorf("start %q: %v", bs[0].Start, err)
	}
	if sent, dropped, pending := ta.AccessLogCounts(); sent != 3 || dropped != 0 || pending != 0 {
		t.Errorf("counts %d %d %d", sent, dropped, pending)
	}
}

// A batch the control plane failed to store waits; one it refuses as
// invalid, or every record when it takes no access log, is dropped and
// counted in the next batch's Missed.
func TestAccessLogFailures(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)
	ctx := context.Background()
	ta.Record(entry("/a"))
	h.fake.FailAccessLogs(status.Error(codes.Unavailable, "down"))
	if err := ta.FlushAccessLog(ctx); err == nil {
		t.Fatal("flush succeeded")
	}
	if _, dropped, pending := ta.AccessLogCounts(); dropped != 0 || pending != 1 {
		t.Fatalf("after UNAVAILABLE: dropped %d, pending %d", dropped, pending)
	}
	h.fake.FailAccessLogs(status.Error(codes.Unimplemented, "no"))
	ta.Record(entry("/b"))
	if err := ta.FlushAccessLog(ctx); err == nil {
		t.Fatal("flush succeeded")
	}
	if _, dropped, pending := ta.AccessLogCounts(); dropped != 2 || pending != 0 {
		t.Fatalf("after UNIMPLEMENTED: dropped %d, pending %d", dropped, pending)
	}
	h.fake.FailAccessLogs(nil)
	ta.Record(entry("/c"))
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	bs := h.fake.AccessLogs()
	if len(bs) != 1 || bs[0].Seq != 2 || bs[0].Missed != 2 || bs[0].Records[0].Path != "/c" {
		t.Fatalf("batches %+v", bs)
	}
}

// Beyond the buffer a record is dropped; Record never blocks.
func TestAccessLogBufferFull(t *testing.T) {
	h := newHarness(t)
	a := h.start(h.cfg, nil)
	for range accessLogBuffer + 5 {
		a.Record(entry("/"))
	}
	if sent, dropped, pending := a.AccessLogCounts(); sent != 0 || dropped != 5 || pending != accessLogBuffer {
		t.Errorf("counts %d %d %d", sent, dropped, pending)
	}
	if err := a.FlushAccessLog(context.Background()); err != ErrNotConnected {
		t.Errorf("flush without a connection: %v", err)
	}
}
