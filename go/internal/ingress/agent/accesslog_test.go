// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	otlplogsv1 "github.com/gardenlinux/feos/go/gen/otlp/collector/logs/v1"
	"github.com/gardenlinux/feos/go/internal/ingress/sinklog"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/pki"
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

// fakeSink is the sink's ingest: a LogsService over TLS with a sink's
// certificate of the fake control plane's CA, which records what it
// takes and answers err while it is set.
type fakeSink struct {
	otlplogsv1.UnimplementedLogsServiceServer
	addr string
	mu   sync.Mutex
	reqs []*otlplogsv1.ExportLogsServiceRequest
	peer string
	err  error
}

func newFakeSink(t *testing.T, iss *pki.Issuer) *fakeSink {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, err := pki.CreateCSR(key, "sink-0")
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := iss.IssueSink(csr, "sink-0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: c.Chain, PrivateKey: key}},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    iss.Bundle(),
	})))
	f := &fakeSink{}
	otlplogsv1.RegisterLogsServiceServer(srv, f)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = ln.Addr().String()
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return f
}

func (f *fakeSink) Export(ctx context.Context, req *otlplogsv1.ExportLogsServiceRequest) (*otlplogsv1.ExportLogsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if p, ok := grpcpeer.FromContext(ctx); ok {
		if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 {
			f.peer = ti.State.PeerCertificates[0].URIs[0].String()
		}
	}
	f.reqs = append(f.reqs, req)
	return &otlplogsv1.ExportLogsServiceResponse{}, nil
}

func (f *fakeSink) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// taken returns the records' paths and offsets the sink took, in order,
// and the URI of the last client.
func (f *fakeSink) taken() (paths []string, offsets []int64, peer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.reqs {
		for _, rl := range req.GetResourceLogs() {
			for _, r := range rl.GetScopeLogs()[0].GetLogRecords() {
				var line sinklog.Line
				_ = json.Unmarshal([]byte(r.GetBody().GetStringValue()), &line)
				paths = append(paths, line.Path)
				offsets = append(offsets, r.GetAttributes()[0].GetValue().GetIntValue())
			}
		}
	}
	return paths, offsets, f.peer
}

// With a sink the access log goes there, with the instance's
// certificate, not to the control plane: a batch the sink failed to take
// is sent again under the same offsets; one it refuses as invalid is
// dropped and counted.
func TestAccessLogToSink(t *testing.T) {
	h := newHarness(t)
	sink := newFakeSink(t, h.fake.Issuer())
	h.cfg.SinkEndpoint = sink.addr
	ta := h.joined(nil)
	ctx := context.Background()
	rec := func(path string) wire.AccessLogEntry {
		e := entry(path)
		e.Tenant, e.Service = "acme", "web"
		return e
	}
	ta.Record(rec("/a"))
	ta.Record(rec("/b"))
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	paths, offsets, peer := sink.taken()
	if !slices.Equal(paths, []string{"/a", "/b"}) || !slices.Equal(offsets, []int64{0, 1}) ||
		peer != pki.IngressID(h.fake.TrustDomain(), testName) {
		t.Fatalf("taken %v %v by %s", paths, offsets, peer)
	}
	if len(h.fake.AccessLogs()) != 0 {
		t.Error("the control plane got the access log")
	}

	sink.fail(status.Error(codes.Unavailable, "down"))
	ta.Record(rec("/c"))
	if err := ta.FlushAccessLog(ctx); err == nil {
		t.Fatal("flush succeeded")
	}
	ta.Record(rec("/d"))
	if sent, dropped, pending := ta.AccessLogCounts(); sent != 2 || dropped != 0 || pending != 2 {
		t.Fatalf("while the sink is down: %d %d %d", sent, dropped, pending)
	}
	sink.fail(nil)
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	if paths, offsets, _ := sink.taken(); !slices.Equal(paths, []string{"/a", "/b", "/c", "/d"}) ||
		!slices.Equal(offsets, []int64{0, 1, 2, 3}) {
		t.Fatalf("after the outage %v %v", paths, offsets)
	}

	sink.fail(status.Error(codes.InvalidArgument, "no"))
	ta.Record(rec("/e"))
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	if sent, dropped, pending := ta.AccessLogCounts(); sent != 4 || dropped != 1 || pending != 0 {
		t.Errorf("after a refusal: %d %d %d", sent, dropped, pending)
	}
}
