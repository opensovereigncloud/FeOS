// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

func pods(t *testing.T, e *env, node string) podv1.PodServiceClient {
	t.Helper()
	cc, err := e.srv.Conn(context.Background(), node)
	if err != nil {
		t.Fatalf("Conn(%s): %v", node, err)
	}
	return podv1.NewPodServiceClient(cc)
}

// whichSide answers with the id of the node side that served the call.
func whichSide(t *testing.T, e *env, node string) string {
	t.Helper()
	info, err := pods(t, e, node).GetPod(context.Background(), &podv1.GetPodRequest{})
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	return info.Reason
}

func nextEvent(t *testing.T, ch <-chan ConnEvent) ConnEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no ConnEvent")
		return ConnEvent{}
	}
}

func noEvent(t *testing.T, ch <-chan ConnEvent, d time.Duration) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected ConnEvent %+v", ev)
	case <-time.After(d):
	}
}

func TestNodeConnection(t *testing.T) {
	e := newEnv(t)
	events, unsubscribe := e.srv.Subscribe()
	defer unsubscribe()
	n := e.node("n1", time.Hour)
	ns := e.dialNode(n, "a")
	if ev := nextEvent(t, events); ev != (ConnEvent{Node: "n1", Connected: true}) {
		t.Errorf("event %+v", ev)
	}
	if !e.srv.Connected("n1") || e.srv.Connections("n1") != 1 || fmt.Sprint(e.srv.Nodes()) != "[n1]" {
		t.Errorf("Connected %v Connections %d Nodes %v", e.srv.Connected("n1"), e.srv.Connections("n1"), e.srv.Nodes())
	}
	p := e.presented()
	if len(p) != 1 || p[0].node != "n1" || p[0].serial != n.serial || p[0].addr == "" {
		t.Errorf("OnPresented %+v", p)
	}
	eventually(t, 2*time.Second, "OnNodeConn", func() bool { return fmt.Sprint(e.nodeConns()) == "[n1]" })
	if e.chk.count(n.serial, true) != 1 {
		t.Errorf("accept checks %d", e.chk.count(n.serial, true))
	}

	// PodContext reaches the node as feos-pod-id.
	id := podid.Mint()
	cc, err := e.srv.Conn(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	info, err := podv1.NewPodServiceClient(cc).GetPod(PodContext(context.Background(), id), &podv1.GetPodRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.PodId != id.String() || info.Reason != "a" {
		t.Errorf("GetPod %v", info)
	}
	// Setting it twice keeps one value.
	ctx := PodContext(PodContext(context.Background(), podid.Mint()), id)
	if info, err := podv1.NewPodServiceClient(cc).GetPod(ctx, &podv1.GetPodRequest{}); err != nil || info.PodId != id.String() {
		t.Errorf("GetPod with PodContext twice: %v %v", info, err)
	}

	// The node goes away.
	ns.srv.Stop()
	if ev := nextEvent(t, events); ev != (ConnEvent{Node: "n1", Connected: false}) {
		t.Errorf("event %+v", ev)
	}
	eventually(t, 3*time.Second, "unregistered", func() bool { return e.srv.Connections("n1") == 0 })
	if e.srv.Connected("n1") || len(e.srv.Nodes()) != 0 {
		t.Error("still connected")
	}
	_, err = podv1.NewPodServiceClient(cc).GetPod(context.Background(), &podv1.GetPodRequest{})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "node n1 is not connected to instance i1") {
		t.Errorf("call after the node left: %v", err)
	}
}

func TestCheckNodeAtAccept(t *testing.T) {
	e := newEnv(t)
	refused := e.node("n1", time.Hour)
	e.chk.set(refused.serial, time.Time{}, Refusal("node n1 was removed; join it again"))
	ns := e.dialNode(refused, "a")
	ns.waitClosed(t, 3*time.Second)
	transient := e.node("n2", time.Hour)
	e.chk.set(transient.serial, time.Time{}, errors.New("database down"))
	ns = e.dialNode(transient, "b")
	ns.waitClosed(t, 3*time.Second)
	if e.srv.Connected("n1") || e.srv.Connected("n2") || len(e.presented()) != 0 {
		t.Errorf("refused connections registered or presented: %v", e.presented())
	}
}

func TestRecheckRefusedCloses(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	ns := e.dialNode(n, "a")
	up := e.dialUplink(n)
	if _, err := up.fleet.ReportTelemetry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	e.chk.set(n.serial, time.Time{}, fmt.Errorf("certificate revoked: %w", model.ErrNodeRefused))
	e.srv.Recheck(context.Background(), "n1")
	ns.waitClosed(t, 3*time.Second)
	if e.srv.Connected("n1") {
		t.Error("still connected after a refusal")
	}
	eventually(t, 3*time.Second, "uplink closed", func() bool { l, u := e.linkCount("n1"); return l+u == 0 })
	if e.chk.count(n.serial, false) != 2 {
		t.Errorf("re-checks %d, want 2 (node connection and uplink)", e.chk.count(n.serial, false))
	}
}

func TestRecheckTransientKeepsAndRetries(t *testing.T) {
	e := newEnv(t, envOpt{tune: func(s *Server) {
		s.recheckMinBackoff = 50 * time.Millisecond
		s.recheckMaxBackoff = 200 * time.Millisecond
	}})
	n := e.node("n1", time.Hour)
	ns := e.dialNode(n, "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	e.chk.set(n.serial, time.Time{}, errors.New("connection reset by the database"))
	e.srv.Recheck(context.Background(), "n1")
	eventually(t, 3*time.Second, "retried", func() bool { return e.chk.count(n.serial, false) >= 4 })
	if ns.isClosed() || !e.srv.Connected("n1") {
		t.Fatal("a transient error closed the connection")
	}
	// Backoff: doubling from 50 ms, capped at 200 ms.
	e.srv.mu.Lock()
	delay := e.srv.nodes["n1"].retryDelay
	e.srv.mu.Unlock()
	if delay != 200*time.Millisecond {
		t.Errorf("retry delay %v, want the cap", delay)
	}
	e.chk.set(n.serial, time.Time{}, nil)
	eventually(t, 2*time.Second, "answered", func() bool {
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		return e.srv.nodes["n1"].retry == nil
	})
	k := e.chk.count(n.serial, false)
	never(t, 500*time.Millisecond, "more retries after an answer", func() bool { return e.chk.count(n.serial, false) != k })
	if !e.srv.Connected("n1") {
		t.Error("disconnected")
	}
	if e.logs.count("nodes' port: re-check failed; connection kept, retrying") == 0 {
		t.Error("transient error not logged")
	}
}

func TestRevocationAheadArmsRecheck(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	revokeAt := time.Now().Add(700 * time.Millisecond)
	e.chk.set(n.serial, revokeAt, nil)
	e.chk.setRecheck(n.serial, time.Time{}, fmt.Errorf("%w: past its grace", model.ErrCertSuperseded))
	ns := e.dialNode(n, "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	// The only connection is current although superseded.
	if whichSide(t, e, "n1") != "a" {
		t.Fatal("not served by the superseded connection")
	}
	ns.waitClosed(t, 5*time.Second)
	if time.Now().Before(revokeAt) {
		t.Error("closed before the revocation time")
	}
	if e.chk.count(n.serial, false) < 1 {
		t.Error("no re-check at the revocation time")
	}
}

func TestRecheckReArmsNewRevocationTime(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	e.chk.set(n.serial, time.Now().Add(time.Hour), nil)
	ns := e.dialNode(n, "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	// A re-check with an earlier revocation re-arms the timer.
	e.chk.set(n.serial, time.Now().Add(500*time.Millisecond), nil)
	e.chk.setRecheck(n.serial, time.Now().Add(500*time.Millisecond), nil)
	e.srv.Recheck(context.Background(), "n1")
	e.chk.setRecheck(n.serial, time.Time{}, Refusal("revoked"))
	ns.waitClosed(t, 5*time.Second)
}

func TestRevocationPassedByInstanceClock(t *testing.T) {
	e := newEnv(t, envOpt{tune: func(s *Server) {
		s.recheckMinBackoff = 100 * time.Millisecond
		s.recheckMaxBackoff = 200 * time.Millisecond
	}})
	// The database's clock runs behind this instance's: the revocation time
	// it answers has passed here, but the certificate is in its grace there.
	n := e.node("n1", time.Hour)
	e.chk.set(n.serial, time.Now().Add(-50*time.Millisecond), nil)
	ns := e.dialNode(n, "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	superseded := func() bool {
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		l := e.srv.nodes["n1"].current
		return l != nil && l.superseded
	}
	if !superseded() {
		t.Fatal("a connection with a revocation time is not superseded at accept")
	}
	// It stays superseded, and its re-check is repeated while the database
	// still answers the same revocation time.
	eventually(t, 3*time.Second, "re-checked again and again", func() bool { return e.chk.count(n.serial, false) >= 3 })
	if !superseded() || ns.isClosed() {
		t.Fatal("un-superseded or closed while the database keeps its grace")
	}
	if whichSide(t, e, "n1") != "a" {
		t.Error("the only connection is not current")
	}
	// Once the database's clock passes the revocation, the re-check retires it.
	e.chk.setRecheck(n.serial, time.Time{}, fmt.Errorf("%w: past its grace", model.ErrCertSuperseded))
	ns.waitClosed(t, 3*time.Second)

	// An uplink accepted in the same window is superseded too: it takes no
	// call, does not count toward the node's uplinks and is closed.
	m := e.node("n2", time.Hour)
	e.chk.set(m.serial, time.Now().Add(-50*time.Millisecond), nil)
	up := e.dialUplink(m)
	_, err := up.fleet.ReportTelemetry(context.Background(), nil)
	if status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "certificate superseded; reconnect with the node's newest certificate" {
		t.Fatalf("call on an uplink whose revocation time passed here: %v", err)
	}
	eventually(t, 3*time.Second, "uplink closed", func() bool { _, u := e.linkCount("n2"); return u == 0 })
}

func TestRecheckAnswersInOrder(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	e.dialNode(n, "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	entered := make(chan struct{})
	release := make(chan struct{})
	var rechecks atomic.Int32
	e.chk.setHook(func(c checkCall) {
		if !c.atAccept && rechecks.Add(1) == 1 {
			close(entered)
			<-release
		}
	})
	done := make(chan struct{}, 2)
	// A sweep's check reads the certificate as current and is slow.
	go func() {
		e.srv.Recheck(context.Background(), "n1")
		done <- struct{}{}
	}()
	<-entered
	// Meanwhile the node renews: its certificate is superseded, and the
	// renewal's re-check runs.
	revokeAt := time.Now().Add(time.Hour)
	e.chk.set(n.serial, revokeAt, nil)
	go func() {
		e.srv.Recheck(context.Background(), "n1")
		done <- struct{}{}
	}()
	time.Sleep(200 * time.Millisecond)
	close(release)
	for range 2 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Recheck did not return")
		}
	}
	// The older answer never lands after the newer one.
	e.srv.mu.Lock()
	l := e.srv.nodes["n1"].links[0]
	sup, at, armed := l.superseded, l.revokeAt, l.revokeTimer != nil
	e.srv.mu.Unlock()
	if !sup || !at.Equal(revokeAt) || !armed {
		t.Errorf("after both re-checks: superseded %v, revocation %v, timer armed %v", sup, at, armed)
	}
	if got := e.chk.count(n.serial, false); got != 2 {
		t.Errorf("re-checks %d, want 2", got)
	}
}

func TestSupersededNotCurrentBesideNewer(t *testing.T) {
	e := newEnv(t)
	old := e.node("n1", time.Hour)
	time.Sleep(1100 * time.Millisecond) // a NotBefore one second later
	renewed := e.node("n1", time.Hour)
	if !renewed.leaf.NotBefore.After(old.leaf.NotBefore) {
		t.Fatal("certificates not ordered")
	}

	a := e.dialNode(old, "old")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	b := e.dialNode(renewed, "new")
	// The old connection is idle: it drains and closes at once.
	a.waitClosed(t, 3*time.Second)
	if whichSide(t, e, "n1") != "new" {
		t.Error("the newer certificate's connection is not current")
	}

	// The old certificate, now superseded, dials again: never current
	// while the newer one's is.
	e.chk.set(old.serial, time.Now().Add(time.Hour), nil)
	c := e.dialNode(old, "old-again")
	c.waitClosed(t, 3*time.Second)
	if whichSide(t, e, "n1") != "new" || b.isClosed() {
		t.Error("the superseded connection took over")
	}

	// Newest certificate wins over newest registration: the older,
	// unsuperseded certificate registered later is not current either.
	e.chk.set(old.serial, time.Time{}, nil)
	d := e.dialNode(old, "old-later")
	d.waitClosed(t, 3*time.Second)
	if whichSide(t, e, "n1") != "new" {
		t.Error("a later registration of an older certificate took over")
	}
	// Same certificate twice: the newer registration is current.
	f := e.dialNode(renewed, "new-2")
	b.waitClosed(t, 3*time.Second)
	if whichSide(t, e, "n1") != "new-2" || f.isClosed() {
		t.Error("the newer registration is not current")
	}
}

func TestNotAfterCloses(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", 2*time.Second)
	ns := e.dialNode(n, "a")
	up := e.dialUplink(n)
	if _, err := up.fleet.ReportTelemetry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	ns.waitClosed(t, 5*time.Second)
	if time.Now().Before(n.leaf.NotAfter) {
		t.Error("closed before NotAfter")
	}
	eventually(t, 3*time.Second, "uplink closed", func() bool { _, u := e.linkCount("n1"); return u == 0 })
}

func TestDrainingFinishesInFlightCalls(t *testing.T) {
	e := newEnv(t)
	n := e.node("n1", time.Hour)
	a := e.dialNode(n, "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	client := pods(t, e, "n1")
	done := make(chan error, 1)
	go func() {
		_, err := client.CreatePod(context.Background(), &podv1.CreatePodRequest{PodId: "p1"})
		done <- err
	}()
	<-a.pods.entered
	b := e.dialNode(n, "b")
	eventually(t, 3*time.Second, "two connections", func() bool { return e.srv.Connections("n1") == 2 })
	// New calls go to the new connection, through the same client.
	eventually(t, 3*time.Second, "b current", func() bool { return whichSide(t, e, "n1") == "b" })
	info, err := client.GetPod(context.Background(), &podv1.GetPodRequest{})
	if err != nil || info.Reason != "b" {
		t.Fatalf("a kept client's call went to %v (%v)", info, err)
	}
	if a.isClosed() {
		t.Fatal("the draining connection closed with a call in flight")
	}
	close(a.pods.gate)
	if err := <-done; err != nil {
		t.Fatalf("in-flight call: %v", err)
	}
	a.waitClosed(t, 3*time.Second)
	if e.srv.Connections("n1") != 1 || b.isClosed() {
		t.Error("connections after the drain")
	}
}

func TestGoAwayWhileCallInFlight(t *testing.T) {
	e := newEnv(t)
	events, unsubscribe := e.srv.Subscribe()
	defer unsubscribe()
	n := e.node("n1", time.Hour)
	a := e.dialNode(n, "a")
	nextEvent(t, events)
	done := make(chan error, 1)
	go func() {
		_, err := pods(t, e, "n1").CreatePod(context.Background(), &podv1.CreatePodRequest{PodId: "p1"})
		done <- err
	}()
	<-a.pods.entered
	stopped := make(chan struct{})
	go func() { a.srv.GracefulStop(); close(stopped) }() // GOAWAY, then wait for the call
	// The channel leaves READY: the connection takes no new calls.
	if ev := nextEvent(t, events); ev.Connected {
		t.Fatalf("event %+v", ev)
	}
	if e.srv.Connected("n1") {
		t.Error("a connection that got GOAWAY is still current")
	}
	if e.srv.Connections("n1") != 1 {
		t.Error("the draining connection was dropped with a call in flight")
	}
	close(a.pods.gate)
	if err := <-done; err != nil {
		t.Fatalf("the call in flight failed: %v", err)
	}
	<-stopped
	eventually(t, 3*time.Second, "closed", func() bool { return e.srv.Connections("n1") == 0 })
	// The node dials again.
	e.dialNode(n, "b")
	if ev := nextEvent(t, events); !ev.Connected {
		t.Fatalf("event %+v", ev)
	}
	if whichSide(t, e, "n1") != "b" {
		t.Error("not served by the new connection")
	}
}

func TestDrainTimeout(t *testing.T) {
	e := newEnv(t, envOpt{cfg: func(c *Config) { c.ConnDrainTimeout = 400 * time.Millisecond }})
	n := e.node("n1", time.Hour)
	a := e.dialNode(n, "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	cc, err := e.srv.Conn(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	// A lifecycle call and a long stream on the old connection.
	created := make(chan error, 1)
	go func() {
		_, err := podv1.NewPodServiceClient(cc).CreatePod(context.Background(), &podv1.CreatePodRequest{PodId: "p1"})
		created <- err
	}()
	<-a.pods.entered
	exec, err := containerv1.NewContainerServiceClient(cc).Exec(context.Background(), &containerv1.ExecRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Recv(); err != nil {
		t.Fatal(err)
	}
	b := e.dialNode(n, "b")
	eventually(t, 3*time.Second, "b current", func() bool { return whichSide(t, e, "n1") == "b" })
	start := time.Now()
	// At the drain timeout the long stream is cut...
	if _, err := exec.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("exec after the drain timeout: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("stream cut after %v", d)
	}
	// ...but the lifecycle call is never cut by it.
	time.Sleep(300 * time.Millisecond)
	if a.isClosed() {
		t.Fatal("the drain timeout closed a connection with a lifecycle call in flight")
	}
	close(a.pods.gate)
	if err := <-created; err != nil {
		t.Fatalf("lifecycle call: %v", err)
	}
	a.waitClosed(t, 3*time.Second)

	// Without a lifecycle call, the drain timeout closes the connection,
	// cutting what runs on it.
	cc, _ = e.srv.Conn(context.Background(), "n1")
	read := make(chan error, 1)
	go func() {
		_, err := toolClient(cc).Read(context.Background(), readReq("x"))
		read <- err
	}()
	<-b.tools.entered
	e.dialNode(n, "c")
	b.waitClosed(t, 3*time.Second)
	if err := <-read; err == nil {
		t.Error("a tool call on a connection closed at the drain timeout succeeded")
	}
}

func TestReadyBeforeRegistration(t *testing.T) {
	e := newEnv(t, envOpt{tune: func(s *Server) { s.readyTimeout = 500 * time.Millisecond }})
	n := e.node("n1", time.Hour)
	// A node that completes TLS but never speaks HTTP/2.
	c, err := tls.Dial("tcp", e.addr(), e.tlsConfig(n, ALPNNode))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	eventually(t, 2*time.Second, "checked", func() bool { return e.chk.count(n.serial, true) == 1 })
	never(t, 300*time.Millisecond, "registered before READY", func() bool { return e.srv.Connections("n1") > 0 })
	if !readClosed(c, 3*time.Second) {
		t.Fatal("not closed after the ready timeout")
	}
	if e.srv.Connected("n1") || e.srv.Connections("n1") != 0 {
		t.Error("registered")
	}
}

func TestIdleTimeoutDisabled(t *testing.T) {
	// The control: with an idle timeout the channel goes IDLE when unused,
	// and the once-dialer could never bring it back.
	e := newEnv(t, envOpt{tune: func(s *Server) { s.idleTimeout = 300 * time.Millisecond }})
	e.dialNode(e.node("n1", time.Hour), "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	eventually(t, 5*time.Second, "an idle channel dropped", func() bool { return !e.srv.Connected("n1") })

	// The port's default disables idleness: an unused connection stays.
	e = newEnv(t)
	if e.srv.idleTimeout != 0 {
		t.Fatalf("idle timeout %v", e.srv.idleTimeout)
	}
	ns := e.dialNode(e.node("n1", time.Hour), "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	never(t, 2*time.Second, "the idle connection dropped", func() bool { return !e.srv.Connected("n1") || ns.isClosed() })
}

func TestKeepalivePingsAccepted(t *testing.T) {
	if testing.Short() {
		t.Skip("runs 65 s")
	}
	t.Parallel()
	e := newEnv(t)
	ns := e.dialNode(e.node("n1", time.Hour), "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	// 65 s without a call: ferund pings every 10 s without streams, the
	// node's enforcement (MinTime 5 s, PermitWithoutStream) accepts them.
	never(t, 65*time.Second, "the connection dropped", func() bool { return !e.srv.Connected("n1") || ns.isClosed() })
	if whichSide(t, e, "n1") != "a" {
		t.Error("not served by the original connection")
	}
}

func TestStreamAndToolBudgets(t *testing.T) {
	e := newEnv(t, envOpt{cfg: func(c *Config) { c.MaxStreams = 8 }}) // 4 long streams, 2 tool calls
	n := e.node("n1", time.Hour)
	ns := e.dialNode(n, "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	cc, err := e.srv.Conn(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	containers := containerv1.NewContainerServiceClient(cc)
	var cancels []context.CancelFunc
	for range 4 {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		s, err := containers.Exec(ctx, &containerv1.ExecRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	_, err = containers.Exec(context.Background(), &containerv1.ExecRequest{})
	if status.Code(err) != codes.ResourceExhausted || status.Convert(err).Message() != "node n1: stream budget exhausted" ||
		!refused(err) {
		t.Fatalf("fifth long stream: %v", err)
	}
	// What is not budgeted still goes through.
	evCtx, evCancel := context.WithCancel(context.Background())
	cancels = append(cancels, evCancel)
	ev, err := podv1.NewPodServiceClient(cc).StreamPodEvents(evCtx, &podv1.StreamPodEventsRequest{PodId: "p"})
	if err != nil {
		t.Fatalf("StreamPodEvents: %v", err)
	}
	if _, err := ev.Recv(); err != nil {
		t.Fatalf("StreamPodEvents: %v", err)
	}
	if whichSide(t, e, "n1") != "a" {
		t.Fatal("GetPod")
	}
	// A stream whose context ends gives its slot back.
	cancels[0]()
	eventually(t, 2*time.Second, "slot back", func() bool {
		ctx, cancel := context.WithCancel(context.Background())
		s, err := containers.Exec(ctx, &containerv1.ExecRequest{})
		if err != nil {
			cancel()
			return false
		}
		cancels = append(cancels, cancel)
		_, err = s.Recv()
		return err == nil
	})

	// Tool calls: two at once.
	tools := toolClient(cc)
	results := make(chan error, 3)
	for range 2 {
		go func() {
			_, err := tools.Read(context.Background(), readReq("x"))
			results <- err
		}()
		<-ns.tools.entered
	}
	_, err = tools.Read(context.Background(), readReq("y"))
	if status.Code(err) != codes.ResourceExhausted || status.Convert(err).Message() != "node n1: tool budget exhausted" ||
		!refused(err) {
		t.Fatalf("third tool call: %v", err)
	}
	close(ns.tools.gate)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tools.Read(context.Background(), readReq("z")); err != nil {
		t.Fatalf("tool call after the others ended: %v", err)
	}
	for _, c := range cancels {
		c()
	}
	eventually(t, 2*time.Second, "all slots back", func() bool {
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		l := e.srv.nodes["n1"].current
		return l.long == 0 && l.tools == 0 && l.active == 0
	})
}

func TestForwardCapIsTheConsumers(t *testing.T) {
	e := newEnv(t)
	e.dialNode(e.node("n1", time.Hour), "a")
	eventually(t, 3*time.Second, "connected", func() bool { return e.srv.Connected("n1") })
	cc, _ := e.srv.Conn(context.Background(), "n1")
	ctx, cancel := context.WithCancel(PodContext(context.Background(), podid.Mint()))
	defer cancel()
	// 40 forward streams of one pod: beyond the consumer's cap of 32, which
	// nodelink does not enforce; only the stream budget applies.
	for i := range 40 {
		s, err := forwardClient(cc).Forward(ctx)
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		if err := s.Send(forwardReq()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Recv(); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}
}

func TestConnWaits(t *testing.T) {
	e := newEnv(t, envOpt{cfg: func(c *Config) { c.NodeWait = 300 * time.Millisecond }})

	start := time.Now()
	_, err := e.srv.Conn(context.Background(), "n9")
	if status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "node n9 is not connected to instance i1" ||
		!refused(err) {
		t.Fatalf("Conn: %v", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 2*time.Second {
		t.Errorf("waited %v, want the node wait", d)
	}
	if err := e.srv.WaitConnected(context.Background(), "n9"); status.Code(err) != codes.Unavailable {
		t.Errorf("WaitConnected: %v", err)
	}

	// ctx ends the wait.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	start = time.Now()
	_, err = e.srv.Conn(ctx, "n9")
	cancel()
	if status.Code(err) != codes.Unavailable || time.Since(start) > 250*time.Millisecond {
		t.Errorf("Conn with a short ctx: %v after %v", err, time.Since(start))
	}

	// A node that connects within the wait.
	n := e.node("n1", time.Hour)
	got := make(chan error, 2)
	go func() { _, err := e.srv.Conn(context.Background(), "n1"); got <- err }()
	go func() { got <- e.srv.WaitConnected(context.Background(), "n1") }()
	time.Sleep(50 * time.Millisecond)
	a := e.dialNode(n, "a")
	for range 2 {
		if err := <-got; err != nil {
			t.Fatalf("waiting for a connecting node: %v", err)
		}
	}

	// The rollout deadline: a node that announced stopping is waited for up
	// to it, beyond the node wait.
	a.srv.Stop()
	eventually(t, 3*time.Second, "gone", func() bool { return !e.srv.Connected("n1") })
	e.srv.SetStoppingUntil("n1", time.Now().Add(1500*time.Millisecond))
	go func() { _, err := e.srv.Conn(context.Background(), "n1"); got <- err }()
	time.Sleep(800 * time.Millisecond)
	e.dialNode(n, "b")
	if err := <-got; err != nil {
		t.Fatalf("waiting within the rollout grace: %v", err)
	}
	// Past the rollout deadline, the node wait applies.
	e.srv.SetStoppingUntil("n2", time.Now().Add(700*time.Millisecond))
	start = time.Now()
	err = e.srv.WaitConnected(context.Background(), "n2")
	if d := time.Since(start); status.Code(err) != codes.Unavailable || d < 600*time.Millisecond || d > 2*time.Second {
		t.Errorf("WaitConnected within a rollout deadline: %v after %v", err, d)
	}
	// A rollout deadline set while a caller waits extends its wait.
	go func() { got <- e.srv.WaitConnected(context.Background(), "n3") }()
	time.Sleep(100 * time.Millisecond)
	e.srv.SetStoppingUntil("n3", time.Now().Add(time.Second))
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-got:
		t.Fatalf("the wait ended before the new rollout deadline: %v", err)
	default:
	}
	e.dialNode(e.node("n3", time.Hour), "c")
	if err := <-got; err != nil {
		t.Errorf("WaitConnected n3: %v", err)
	}
	// Clearing it ends the extension.
	e.srv.SetStoppingUntil("n2", time.Time{})
	e.srv.mu.Lock()
	_, kept := e.srv.nodes["n2"]
	e.srv.mu.Unlock()
	if kept {
		t.Error("a node with nothing left is kept")
	}
}

func TestSubscribeEvents(t *testing.T) {
	e := newEnv(t)
	events, unsubscribe := e.srv.Subscribe()
	n := e.node("n1", time.Hour)
	a := e.dialNode(n, "a")
	if ev := nextEvent(t, events); ev != (ConnEvent{"n1", true}) {
		t.Fatalf("%+v", ev)
	}
	// A second connection: no event; the first drains and closes: none.
	e.dialNode(n, "b")
	a.waitClosed(t, 3*time.Second)
	noEvent(t, events, 200*time.Millisecond)
	if len(e.nodeConns()) != 1 {
		t.Errorf("OnNodeConn %v", e.nodeConns())
	}
	m := e.node("n2", time.Hour)
	c := e.dialNode(m, "c")
	if ev := nextEvent(t, events); ev != (ConnEvent{"n2", true}) {
		t.Fatalf("%+v", ev)
	}
	c.srv.Stop()
	if ev := nextEvent(t, events); ev != (ConnEvent{"n2", false}) {
		t.Fatalf("%+v", ev)
	}
	unsubscribe()
	unsubscribe()
	for range events {
	}
}

// refused reports whether err is the port's own refusal (a *RefusalError).
func refused(err error) bool {
	var re *RefusalError
	return errors.As(err, &re)
}
