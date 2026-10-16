// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/ironcore-dev/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
)

func TestUplink(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	up := e.dialUplink(n)
	resp, err := up.fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{})
	if err != nil {
		t.Fatal(err)
	}
	// The handler got the node and serial from the certificate.
	if got := resp.Instances[0]; got.Id != "n1" || got.NodeAddress != n.serial {
		t.Errorf("peer %v", got)
	}
	stream, err := up.fleet.PublishEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&fleetv1.EventBatch{StartId: "s"}); err != nil {
		t.Fatal(err)
	}
	if ack, err := stream.Recv(); err != nil || ack.StartId != "n1/s" {
		t.Fatalf("ack %v %v", ack, err)
	}
	stream.CloseSend()
	// Join is not served on an uplink.
	_, err = up.fleet.Join(context.Background(), &fleetv1.JoinRequest{Name: "x"})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("Join on the uplink: %v", err)
	}
	if p := e.presented(); len(p) != 1 || p[0].serial != n.serial {
		t.Errorf("OnPresented %v", p)
	}
	// An uplink is no node connection.
	if e.srv.Connected("n1") || e.srv.Connections("n1") != 0 {
		t.Error("an uplink counts as a node connection")
	}
	if _, u := e.linkCount("n1"); u != 1 {
		t.Errorf("uplinks %d", u)
	}
}

func TestNodeFromContext(t *testing.T) {
	if _, ok := NodeFromContext(context.Background()); ok {
		t.Error("a peer in an empty context")
	}
	p := Peer{Node: "n1", Serial: "ab", Addr: "1.2.3.4:5", NotAfter: time.Unix(1, 0)}
	if got, ok := NodeFromContext(ContextWithPeer(context.Background(), p)); !ok || got != p {
		t.Errorf("got %v %v", got, ok)
	}
}

func TestRefusalServer(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	e.chk.set(n.serial, time.Time{}, Refusal("node n1 was removed; join it again"))
	up := e.dialUplink(n)
	for name, call := range map[string]func() error{
		"ReportTelemetry": func() error {
			_, err := up.fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{})
			return err
		},
		"RenewCertificate": func() error {
			_, err := up.fleet.RenewCertificate(context.Background(), &fleetv1.RenewCertificateRequest{})
			return err
		},
		"PublishEvents": func() error {
			s, err := up.fleet.PublishEvents(context.Background())
			if err != nil {
				return err
			}
			_, err = s.Recv()
			return err
		},
	} {
		err := call()
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "node n1 was removed; join it again" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Another service on the refusal server is unknown.
	_, err := podv1.NewPodServiceClient(up.cc).GetPod(context.Background(), &podv1.GetPodRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("GetPod on the refusal server: %v", err)
	}
	if len(e.presented()) != 0 {
		t.Error("a refused uplink was presented")
	}
	if _, u := e.linkCount("n1"); u != 0 {
		t.Error("a refused uplink was registered")
	}

	// A refusal that is an error wrapping model.ErrNodeRefused the usual
	// way answers with its own text, without the sentinel's.
	for name, refusal := range map[string]error{
		"n2": fmt.Errorf("identity of node n2 forked; join it again with a token pinned to its name: %w", model.ErrNodeRefused),
		"n3": fmt.Errorf("%w: certificate of node n3 was revoked (removed); join it again", model.ErrNodeRefused),
	} {
		m := e.node(name, time.Hour)
		e.chk.set(m.serial, time.Time{}, refusal)
		want := strings.TrimSuffix(strings.TrimPrefix(refusal.Error(), "node refused: "), ": node refused")
		_, err = e.dialUplink(m).fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{})
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != want {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}

	// A refused node connection is closed, not answered.
	ns := e.dialNode(n, "a")
	ns.waitClosed(t, 3*time.Second)
}

func TestRefusalText(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{Refusal("node %s was removed; join it again", "n1"), "node n1 was removed; join it again"},
		{fmt.Errorf("check: %w", Refusal("node n1 was removed; join it again")), "node n1 was removed; join it again"},
		{fmt.Errorf("%w: node n1 was removed; join it again", model.ErrNodeRefused), "node n1 was removed; join it again"},
		{fmt.Errorf("identity of node n1 forked; join it again with a token pinned to its name: %w", model.ErrNodeRefused),
			"identity of node n1 forked; join it again with a token pinned to its name"},
		{model.ErrNodeRefused, "node refused"},
		{fmt.Errorf("%w: past its grace", model.ErrCertSuperseded), "past its grace"},
	} {
		if got := refusalText(c.err); got != c.want {
			t.Errorf("refusalText(%q) = %q, want %q", c.err, got, c.want)
		}
	}
	if !errors.Is(Refusal("x"), model.ErrNodeRefused) {
		t.Error("Refusal does not wrap ErrNodeRefused")
	}
}

func TestRefusalLifetime(t *testing.T) {
	e := newEnv(t, envOpt{tune: func(s *Server) { s.joinLifetime = 300 * time.Millisecond }})
	n := e.node("n1", time.Hour)
	e.chk.set(n.serial, time.Time{}, Refusal("certificate of node n1 was revoked (removed); join it again"))
	up := e.dialUplink(n)
	if _, err := up.fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{}); status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
	up.mu.Lock()
	c := up.conns[0]
	up.mu.Unlock()
	select {
	case <-c.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("a refusal connection outlived its lifetime")
	}
	eventually(t, time.Second, "slot free", func() bool { return len(e.srv.joinSlots) == 0 })
}

func TestSupersededUplink(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	up := e.dialUplink(n)
	stream, err := up.fleet.PublishEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&fleetv1.EventBatch{StartId: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	// The node renewed: its old certificate is superseded, revoked in an
	// hour.
	e.chk.set(n.serial, time.Now().Add(time.Hour), nil)
	e.srv.Recheck(context.Background(), "n1")
	_, err = up.fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{})
	if status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "certificate superseded; reconnect with the node's newest certificate" {
		t.Fatalf("new call on a superseded uplink: %v", err)
	}
	// The stream in flight goes on.
	if err := stream.Send(&fleetv1.EventBatch{StartId: "b"}); err != nil {
		t.Fatal(err)
	}
	if ack, err := stream.Recv(); err != nil || ack.StartId != "n1/b" {
		t.Fatalf("stream on a superseded uplink: %v %v", ack, err)
	}
	if _, u := e.linkCount("n1"); u != 1 {
		t.Fatal("closed with a stream in flight")
	}
	// Its end closes the uplink.
	stream.CloseSend()
	if _, err := stream.Recv(); err == nil {
		t.Fatal("stream did not end")
	}
	eventually(t, 3*time.Second, "closed when idle", func() bool { _, u := e.linkCount("n1"); return u == 0 })
	up.mu.Lock()
	c := up.conns[0]
	up.mu.Unlock()
	select {
	case <-c.closed:
	case <-time.After(3 * time.Second):
		t.Error("the node's side of the superseded uplink is open")
	}
}

func TestSupersededUplinkClosesAtRevocation(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	up := e.dialUplink(n)
	stream, err := up.fleet.PublishEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stream.Send(&fleetv1.EventBatch{})
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	revokeAt := time.Now().Add(500 * time.Millisecond)
	e.chk.set(n.serial, revokeAt, nil)
	e.srv.Recheck(context.Background(), "n1")
	// A stream that never ends is cut at the revocation time.
	if _, err := stream.Recv(); err == nil {
		t.Fatal("stream outlived the revocation")
	}
	if time.Now().Before(revokeAt) {
		t.Error("cut before the revocation time")
	}
	eventually(t, 2*time.Second, "unregistered", func() bool { _, u := e.linkCount("n1"); return u == 0 })
}

func TestSupersededAtAcceptUplink(t *testing.T) {
	e := newEnv(t, envOpt{tune: func(s *Server) { s.supersededGrace = 300 * time.Millisecond }})
	n := e.node("n1", time.Hour)
	e.chk.set(n.serial, time.Now().Add(time.Hour), nil)
	up := e.dialUplink(n)
	_, err := up.fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{})
	if status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "certificate superseded; reconnect with the node's newest certificate" {
		t.Fatalf("call with a superseded certificate: %v", err)
	}
	eventually(t, 3*time.Second, "closed after the idle grace", func() bool { _, u := e.linkCount("n1"); return u == 0 })
}

func TestUplinkLimit(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	var ups []*uplink
	for i := range 4 {
		up := e.dialUplink(n)
		if _, err := up.fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{}); err != nil {
			t.Fatalf("uplink %d: %v", i, err)
		}
		ups = append(ups, up)
	}
	fifth := e.dialUplink(n)
	if _, err := fifth.fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{}); err == nil {
		t.Fatal("a fifth uplink of a node was served")
	}
	if e.srv.droppedUplinks.Load() == 0 {
		t.Error("not counted")
	}
	// Superseded uplinks are not counted: a node that renewed reaches the
	// limit with its new certificate while the old ones drain.
	for _, up := range ups[:2] {
		s, err := up.fleet.PublishEvents(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		s.Send(&fleetv1.EventBatch{})
		if _, err := s.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	e.chk.set(n.serial, time.Now().Add(time.Hour), nil)
	e.srv.Recheck(context.Background(), "n1")
	eventually(t, 3*time.Second, "idle superseded closed", func() bool { _, u := e.linkCount("n1"); return u == 2 })
	renewed := e.node("n1", time.Hour)
	for i := range 4 {
		if _, err := e.dialUplink(renewed).fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{}); err != nil {
			t.Fatalf("renewed uplink %d: %v", i, err)
		}
	}
	if _, u := e.linkCount("n1"); u != 6 {
		t.Errorf("uplinks %d, want 4 new and 2 superseded draining", u)
	}
}

func TestJoinServer(t *testing.T) {
	e := newEnv(t, envOpt{tune: func(s *Server) {
		s.joinSlots = make(chan struct{}, 2)
		s.joinLifetime = 700 * time.Millisecond
	}})
	a := e.dialUplink(nil)
	resp, err := a.fleet.Join(context.Background(), &fleetv1.JoinRequest{Name: "n1"})
	if err != nil || resp.Name != "n1" {
		t.Fatalf("Join: %v %v", resp, err)
	}
	for name, call := range map[string]func() error{
		"ReportTelemetry": func() error {
			_, err := a.fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{})
			return err
		},
		"PublishEvents": func() error {
			s, err := a.fleet.PublishEvents(context.Background())
			if err != nil {
				return err
			}
			_, err = s.Recv()
			return err
		},
		"GetPod": func() error {
			_, err := podv1.NewPodServiceClient(a.cc).GetPod(context.Background(), &podv1.GetPodRequest{})
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.Unimplemented {
			t.Errorf("%s on the join server: %v", name, err)
		}
	}
	if e.chk.total() != 0 {
		t.Error("a join connection was checked")
	}

	// Two join connections at once; the third is closed.
	b := e.dialUplink(nil)
	if _, err := b.fleet.Join(context.Background(), &fleetv1.JoinRequest{Name: "n2"}); err != nil {
		t.Fatal(err)
	}
	if len(e.srv.joinSlots) != 2 {
		t.Fatalf("slots %d", len(e.srv.joinSlots))
	}
	c, err := tls.Dial("tcp", e.addr(), e.tlsConfig(nil, ALPNUplink))
	if err != nil {
		t.Fatal(err)
	}
	if !readClosed(c, time.Second) {
		t.Error("a third join connection was served")
	}
	c.Close()
	if e.srv.droppedJoins.Load() != 1 {
		t.Errorf("dropped %d", e.srv.droppedJoins.Load())
	}
	// Each is closed after its lifetime, which frees the slots.
	eventually(t, 3*time.Second, "join connections closed", func() bool { return len(e.srv.joinSlots) == 0 })
	if a.dials() != 1 || b.dials() != 1 {
		t.Fatal("dials")
	}
	select {
	case <-a.conns[0].closed:
	default:
		t.Error("the node's side of a join connection is open")
	}
	if _, err := e.dialUplink(nil).fleet.Join(context.Background(), &fleetv1.JoinRequest{Name: "n3"}); err != nil {
		t.Errorf("Join after the slots freed: %v", err)
	}
}

func TestJoinServerOptions(t *testing.T) {
	joins := &joinStub{gate: make(chan struct{}, 8), entered: make(chan string, 8)}
	e := newEnv(t, envOpt{joins: joins})
	a := e.dialUplink(nil)
	join := func(name string, csr int) error {
		_, err := a.fleet.Join(context.Background(), &fleetv1.JoinRequest{Name: name, Csr: make([]byte, csr)})
		return err
	}
	// The first Join brings the server's settings to the node.
	joins.gate <- struct{}{}
	if err := join("n0", 0); err != nil {
		t.Fatal(err)
	}
	<-joins.entered

	// MaxRecvMsgSize: 64 KiB.
	joins.gate <- struct{}{}
	if err := join("n1", 60<<10); err != nil {
		t.Fatalf("a Join of 60 KiB: %v", err)
	}
	<-joins.entered
	if err := join("n2", 65<<10); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("a Join of 65 KiB: %v", err)
	}

	// MaxConcurrentStreams: 2. A third call on the connection waits for one
	// of the two before it.
	results := make(chan error, 3)
	for _, name := range []string{"a", "b"} {
		go func() { results <- join(name, 0) }()
		<-joins.entered
	}
	go func() { results <- join("c", 0) }()
	select {
	case name := <-joins.entered:
		t.Fatalf("a third concurrent Join (%s) ran on the connection", name)
	case <-time.After(300 * time.Millisecond):
	}
	joins.gate <- struct{}{}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	select {
	case <-joins.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the third Join did not run once a stream was free")
	}
	joins.gate <- struct{}{}
	joins.gate <- struct{}{}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if a.dials() != 1 {
		t.Errorf("dials %d, want one connection", a.dials())
	}
}

func TestJoinServerTimeouts(t *testing.T) {
	e := newEnv(t, envOpt{tune: func(s *Server) {
		s.joinConnTimeout = 300 * time.Millisecond
		s.joinMaxAge = 300 * time.Millisecond
		s.join = s.newJoinServer()
		s.refusal = s.newRefusalServer()
	}})
	// ConnectionTimeout: a connection that never speaks HTTP/2 is closed
	// long before the join lifetime.
	c, err := tls.Dial("tcp", e.addr(), e.tlsConfig(nil, ALPNUplink))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if !readClosed(c, 3*time.Second) {
		t.Fatal("a silent join connection was not closed")
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("closed after %v, before the connection timeout", d)
	}
	// MaxConnectionAge: the server ends an old connection with GOAWAY, and
	// the node's next Join dials a new one.
	a := e.dialUplink(nil)
	if _, err := a.fleet.Join(context.Background(), &fleetv1.JoinRequest{Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if _, err := a.fleet.Join(context.Background(), &fleetv1.JoinRequest{Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	if a.dials() != 2 {
		t.Errorf("dials %d, want 2 after the connection's maximum age", a.dials())
	}
}

func TestUplinkServerOptions(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	up := e.dialUplink(n)
	// MaxRecvMsgSize: 2 MiB.
	if _, err := up.fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{Hostname: strings.Repeat("x", 1<<20)}); err != nil {
		t.Fatalf("telemetry of 1 MiB: %v", err)
	}
	if _, err := up.fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{Hostname: strings.Repeat("x", 2<<20)}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("telemetry over 2 MiB: %v", err)
	}
	// MaxConcurrentStreams: 16. A seventeenth stream waits for a free one.
	var streams []fleetv1.FleetService_PublishEventsClient
	for i := range 16 {
		s, err := up.fleet.PublishEvents(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Send(&fleetv1.EventBatch{StartId: "s"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Recv(); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		streams = append(streams, s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err := up.fleet.PublishEvents(ctx)
	cancel()
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("a seventeenth stream: %v", err)
	}
	streams[0].CloseSend()
	if _, err := streams[0].Recv(); err == nil {
		t.Fatal("stream did not end")
	}
	s, err := up.fleet.PublishEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(&fleetv1.EventBatch{StartId: "t"}); err != nil {
		t.Fatal(err)
	}
	if ack, err := s.Recv(); err != nil || ack.StartId != "n1/t" {
		t.Fatalf("a stream once one was free: %v %v", ack, err)
	}
	if up.dials() != 1 {
		t.Errorf("dials %d, want one uplink", up.dials())
	}
}

func TestRecheckAllSpreads(t *testing.T) {
	e := newEnv(t, envOpt{tune: func(s *Server) { s.recheckSpread = 600 * time.Millisecond }})
	var mu sync.Mutex
	times := map[string]time.Time{}
	var nodes []*tnode
	for _, name := range []string{"n1", "n2", "n3", "n4"} {
		n := e.node(name, time.Hour)
		nodes = append(nodes, n)
		e.dialNode(n, name)
	}
	e.dialUplink(nodes[0]).fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{})
	eventually(t, 5*time.Second, "all connected", func() bool { return len(e.srv.Nodes()) == 4 })
	e.chk.setHook(func(c checkCall) {
		if !c.atAccept {
			mu.Lock()
			if _, seen := times[c.node]; !seen {
				times[c.node] = time.Now()
			}
			mu.Unlock()
		}
	})
	start := time.Now()
	e.srv.RecheckAll(context.Background())
	e.srv.RecheckAll(context.Background()) // coalesced into one more sweep
	if time.Since(start) > 100*time.Millisecond {
		t.Error("RecheckAll blocked")
	}
	eventually(t, 5*time.Second, "every node re-checked", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(times) == 4
	})
	mu.Lock()
	var ts []time.Time
	for _, at := range times {
		ts = append(ts, at)
	}
	mu.Unlock()
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	if spread := ts[3].Sub(ts[0]); spread < 500*time.Millisecond {
		t.Errorf("checks spread over %v, want at least 600ms", spread)
	}
	// The second call's sweep follows the first.
	eventually(t, 5*time.Second, "second sweep", func() bool { return e.chk.count(nodes[3].serial, false) == 2 })
	if e.chk.count(nodes[0].serial, false) != 4 {
		t.Errorf("n1 re-checks %d, want 2 sweeps × (node connection + uplink)", e.chk.count(nodes[0].serial, false))
	}
	eventually(t, 2*time.Second, "sweep over", func() bool {
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		return !e.srv.sweeping
	})
}

func TestRecheckDuringAccept(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	entered := make(chan struct{})
	release := make(chan struct{})
	e.chk.setHook(func(c checkCall) {
		if c.atAccept {
			close(entered)
			<-release
		}
	})
	ns := e.dialNode(n, "a")
	<-entered
	// The node's certificate is revoked while its connection is between
	// its accept check and its registration.
	e.chk.setRecheck(n.serial, time.Time{}, Refusal("revoked"))
	e.srv.Recheck(context.Background(), "n1")
	close(release)
	ns.waitClosed(t, 5*time.Second)
	if e.chk.count(n.serial, false) != 1 {
		t.Errorf("re-checks %d", e.chk.count(n.serial, false))
	}
}

func TestServeContextEndKeepsPort(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	ns := e.dialNode(n, "a")
	up := e.dialUplink(n)
	stream, err := up.fleet.PublishEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stream.Send(&fleetv1.EventBatch{StartId: "a"})
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	// SIGTERM: the instance's context ends, Serve returns.
	e.cancel()
	select {
	case err := <-e.served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
		e.served <- nil
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return when its context ended")
	}
	// Until Shutdown nothing closes: the node connection takes calls, the
	// uplink's stream goes on, and the port accepts nodes that dial again.
	never(t, 300*time.Millisecond, "a connection closed", func() bool { return ns.isClosed() || !e.srv.Connected("n1") })
	if whichSide(t, e, "n1") != "a" {
		t.Error("the node connection does not serve")
	}
	if err := stream.Send(&fleetv1.EventBatch{StartId: "b"}); err != nil {
		t.Fatal(err)
	}
	if ack, err := stream.Recv(); err != nil || ack.StartId != "n1/b" {
		t.Fatalf("uplink stream after the context ended: %v %v", ack, err)
	}
	m := e.node("n2", time.Hour)
	e.dialNode(m, "b")
	eventually(t, 3*time.Second, "a new node connection", func() bool { return e.srv.Connected("n2") })
	if _, err := e.dialUplink(m).fleet.ReportTelemetry(context.Background(), &fleetv1.Telemetry{}); err != nil {
		t.Fatalf("a new uplink: %v", err)
	}
	// Shutdown closes them.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e.srv.Shutdown(ctx)
	ns.waitClosed(t, 3*time.Second)
	if _, err := stream.Recv(); err == nil {
		t.Error("the uplink stream survived the shutdown")
	}
}

func TestShutdown(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	ns := e.dialNode(n, "a")
	up := e.dialUplink(n)
	stream, err := up.fleet.PublishEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stream.Send(&fleetv1.EventBatch{})
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	e.srv.Shutdown(ctx)
	if time.Since(start) > 3*time.Second {
		t.Error("Shutdown overran its deadline")
	}
	ns.waitClosed(t, 3*time.Second)
	if _, err := stream.Recv(); err == nil {
		t.Error("an uplink stream survived the shutdown")
	}
	select {
	case err := <-e.served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
		e.served <- nil
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return")
	}
	if e.srv.Connected("n1") {
		t.Error("connected after the shutdown")
	}
	if _, err := e.srv.Conn(context.Background(), "n1"); status.Code(err) != codes.Unavailable {
		t.Errorf("Conn after the shutdown: %v", err)
	}
	e.srv.Shutdown(ctx) // twice is harmless
	if up.cc.GetState() == connectivity.Ready {
		t.Log("the uplink channel still reports READY")
	}
}
