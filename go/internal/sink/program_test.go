// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	sinkv1 "github.com/gardenlinux/feos/go/gen/feos/sink/v1"
	otlplogsv1 "github.com/gardenlinux/feos/go/gen/otlp/collector/logs/v1"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
)

const (
	testTD     = "run.example.com"
	testDomain = "example.com"
)

// fakeFerund is ferund's part of the contract: the service certificate,
// issued with pki, and a token's tenant.
type fakeFerund struct {
	iss   *pki.Issuer
	certs atomic.Int32
}

func newCA(t *testing.T) *pki.Issuer {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "ca")
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: dir, TrustDomain: testTD,
		DNSDomains: []string{testDomain}}); err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(dir, testTD)
	if err != nil {
		t.Fatal(err)
	}
	return iss
}

func pemCerts(ders ...[]byte) string {
	var b strings.Builder
	for _, d := range ders {
		b.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d}))
	}
	return b.String()
}

func (f *fakeFerund) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == pathServiceCertificates:
		if r.Header.Get("Authorization") != "Bearer svc-token" {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		var req certRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b, _ := pem.Decode([]byte(req.CSR))
		if b == nil || b.Type != "CERTIFICATE REQUEST" {
			http.Error(w, "no CSR", http.StatusBadRequest)
			return
		}
		// ferund's own issuance is built apart; a pod's certificate has
		// the shape the contract asks: the one URI, ServerAuth.
		c, leaf, err := f.iss.IssuePod(b.Bytes, ServerID(testTD, "sink-0"), nil, 24*time.Hour)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.certs.Add(1)
		writeJSON(w, http.StatusOK, certAnswer{Certificate: pemCerts(c.Chain...), Bundle: pemCerts(c.TrustBundle...), NotAfter: leaf.NotAfter})
	case r.Method == http.MethodGet && r.URL.Path == pathTenant:
		if r.Header.Get("Authorization") != "Bearer tenant-token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid token"}`))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"tenant": "acme"})
	default:
		http.NotFound(w, r)
	}
}

// nodeTLS is a node's client side: its node certificate, and the check of
// the sink's certificate by the bundle and its one URI.
func nodeTLS(t *testing.T, iss *pki.Issuer, name string, pod bool) *tls.Config {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, err := pki.CreateCSR(key, name)
	if err != nil {
		t.Fatal(err)
	}
	var chain [][]byte
	if pod {
		c, _, err := iss.IssuePod(csr, pki.SandboxID(testTD, "acme", podid.ID("abcdefghjkm")), nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		chain = c.Chain
	} else {
		c, _, err := iss.IssueNode(csr, name, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		chain = c.Chain
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{{Certificate: chain, PrivateKey: key}},
		InsecureSkipVerify: true, // verified below, by URI
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			certs := make([]*x509.Certificate, len(raw))
			for i, r := range raw {
				c, err := x509.ParseCertificate(r)
				if err != nil {
					return err
				}
				certs[i] = c
			}
			inter := x509.NewCertPool()
			for _, c := range certs[1:] {
				inter.AddCert(c)
			}
			if _, err := certs[0].Verify(x509.VerifyOptions{Roots: iss.Bundle(), Intermediates: inter}); err != nil {
				return err
			}
			if len(certs[0].URIs) != 1 || certs[0].URIs[0].String() != ServerID(testTD, "sink-0") {
				return errors.New("not the sink")
			}
			return nil
		},
	}
}

func TestEndToEnd(t *testing.T) {
	dbURL := dbtest.NewDatabase(t)
	iss := newCA(t)
	ferund := &fakeFerund{iss: iss}
	fsrv := httptest.NewServer(ferund)
	defer fsrv.Close()
	cfg := Default()
	cfg.Name, cfg.Listen, cfg.IngestListen = "sink-0", "127.0.0.1:0", "127.0.0.1:0"
	cfg.DatabaseURL, cfg.TrustDomain, cfg.FerunURL, cfg.ServiceToken = dbURL, testTD, fsrv.URL, "svc-token"
	cfg.ChunkBytes, cfg.ChunkInterval, cfg.FollowPoll = 1, 50*time.Millisecond, 20*time.Millisecond
	bucket := newMemBucket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prog, err := New(ctx, cfg, Deps{Bucket: bucket}, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- prog.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	actx, acancel := context.WithTimeout(ctx, 30*time.Second)
	defer acancel()
	addr, err := prog.IngestAddr(actx)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := grpc.NewClient(addr.String(), grpc.WithTransportCredentials(credentials.NewTLS(nodeTLS(t, iss, "node-1", false))))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	logs := otlplogsv1.NewLogsServiceClient(conn)
	sinks := sinkv1.NewSinkServiceClient(conn)
	now := uint64(time.Now().UnixNano())
	req := exportRequest(resourceAttrs("abcdefghjkm", "main", "log1", "acme",
		map[string]string{attrEnvironment: "env1", attrKind: "service", attrObject: "api", attrReplica: "0", attrRelease: "4", "ferun.node": "liar"}),
		record(0, now-3000, "stdout", "INFO starting"), record(20, now-2000, "stderr", `{"level":"error","trace_id":"`+traceA+`"}`),
		record(70, now-1000, "stdout", "plain"))
	for range 2 {
		if _, err := logs.Export(actx, req); err != nil {
			t.Fatalf("export: %v", err)
		}
	}
	pos, err := sinks.Positions(actx, &sinkv1.PositionsRequest{Streams: []*sinkv1.StreamKey{{Pod: "abcdefghjkm", Container: "main", LogId: "log1"}}})
	if err != nil || len(pos.NextOffsets) != 1 || pos.NextOffsets[0] != 70 {
		t.Fatalf("positions %v, %v", pos, err)
	}

	// A pod's certificate is no node's.
	podConn, err := grpc.NewClient(addr.String(), grpc.WithTransportCredentials(credentials.NewTLS(nodeTLS(t, iss, "x", true))))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = podConn.Close() }()
	if _, err := otlplogsv1.NewLogsServiceClient(podConn).Export(actx, req); err == nil {
		t.Error("a pod exported")
	} else if c := status.Code(err); c != codes.Unavailable && c != codes.PermissionDenied {
		t.Errorf("a pod's export: %v", err)
	}

	api := "http://" + prog.APIAddr().String()
	get := func(path, token string) (*http.Response, []byte) {
		t.Helper()
		r, _ := http.NewRequestWithContext(actx, http.MethodGet, api+path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	if resp, b := get("/api/v1/logs?environment=env1", "wrong"); resp.StatusCode != 401 || string(b) != `{"error":"invalid token"}` {
		t.Errorf("wrong token: %d %s", resp.StatusCode, b)
	}
	if resp, _ := get("/api/v1/logs?environment=env1", ""); resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	if resp, _ := get("/healthz", ""); resp.StatusCode != 200 {
		t.Errorf("healthz %d", resp.StatusCode)
	}
	// The lines move to the cold stage; the read finds them there.
	waitFor(t, func() bool { return len(bucket.keys()) > 0 })
	resp, b := get("/api/v1/logs?environment=env1", "tenant-token")
	if resp.StatusCode != 200 {
		t.Fatalf("logs: %d %s", resp.StatusCode, b)
	}
	var page PageJSON
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 3 || page.Lines[1].Level != "error" || page.Lines[1].Source != "stderr" || page.Lines[2].Text != "plain" {
		t.Fatalf("page %s", b)
	}
	st := page.Streams[page.Lines[0].Stream]
	if st.Node != "node-1" || st.Environment != "env1" || st.Release != 4 || st.Lines != 3 || st.Tenant != "acme" {
		t.Errorf("stream %+v", st)
	}
	if !strings.Contains(string(b), `"ts":"`) || !strings.Contains(string(b), `"offset":"20"`) {
		t.Errorf("ts and offset are not strings: %s", b)
	}
	resp, b = get("/api/v1/logs?environment=env1&trace="+traceA, "tenant-token")
	if resp.StatusCode != 200 || strings.Count(string(b), `"offset"`) != 1 {
		t.Errorf("trace: %d %s", resp.StatusCode, b)
	}
	resp, b = get("/api/v1/log-streams?environment=env1", "tenant-token")
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"container":"main"`) {
		t.Errorf("streams: %d %s", resp.StatusCode, b)
	}
	if resp, _ := get("/api/v1/logs?kind=service", "tenant-token"); resp.StatusCode != 400 {
		t.Errorf("no selector: %d", resp.StatusCode)
	}

	// Follow: the page, then a new line, as NDJSON.
	fctx, fcancel := context.WithCancel(actx)
	defer fcancel()
	fr, _ := http.NewRequestWithContext(fctx, http.MethodGet, api+"/api/v1/logs?environment=env1&follow=1", nil)
	fr.Header.Set("Authorization", "Bearer tenant-token")
	fresp, err := http.DefaultClient.Do(fr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresp.Body.Close() }()
	if ct := fresp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content type %q", ct)
	}
	sc := bufio.NewScanner(fresp.Body)
	var got []followLine
	for len(got) < 4 && sc.Scan() {
		var fl followLine
		if err := json.Unmarshal(sc.Bytes(), &fl); err != nil {
			t.Fatal(err)
		}
		got = append(got, fl)
		if len(got) == 3 {
			if _, err := logs.Export(actx, exportRequest(resourceAttrs("abcdefghjkm", "main", "log1", "acme",
				map[string]string{attrEnvironment: "env1"}), record(90, now, "stdout", "followed"))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(got) != 4 || got[0].Stream == nil || got[1].Stream != nil || got[3].Line.Text != "followed" {
		t.Errorf("follow: %+v", got)
	}
	fcancel()

	if _, err := sinks.EndStreams(actx, &sinkv1.EndStreamsRequest{Streams: []*sinkv1.StreamKey{{Pod: "abcdefghjkm", Container: "main", LogId: "log1"}}}); err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequestWithContext(actx, http.MethodDelete, api+"/api/v1/log-streams?environment=env1", nil)
	r.Header.Set("Authorization", "Bearer tenant-token")
	dresp, err := http.DefaultClient.Do(r)
	if err != nil || dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %v %v", dresp, err)
	}
	_ = dresp.Body.Close()
	resp, b = get("/api/v1/log-streams?environment=env1", "tenant-token")
	if resp.StatusCode != 200 || string(b) != "{\"streams\":[]}\n" {
		t.Errorf("after delete: %s", b)
	}
	if ferund.certs.Load() != 1 {
		t.Errorf("%d certificates", ferund.certs.Load())
	}
}
