// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	portforwardv1 "github.com/ironcore-dev/feos/go/gen/feos/portforward/v1"
	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/tools/toolstest"
)

// wantAnswer checks a plain-text answer of the door.
func wantAnswer(t *testing.T, what string, code int, retry, body string, wantCode int, wantRetry, wantBody string) {
	t.Helper()
	if code != wantCode || retry != wantRetry || body != wantBody+"\n" {
		t.Errorf("%s: %d Retry-After %q %q, want %d Retry-After %q %q", what, code, retry, body, wantCode, wantRetry,
			wantBody)
	}
}

// The forward class: the bad paths, the token only from Authorization,
// the answers of an absent sandbox for unknown and foreign tokens, a
// failed lookup, and the Kubernetes ports.
func TestForwardClass(t *testing.T) {
	w := newWorld(t)
	id := w.sb.ID.String()
	for _, p := range []string{"/fwd/" + id + "/0/", "/fwd/" + id + "/70000/", "/fwd/" + id + "/abc/x",
		"/fwd/" + id + "/-1", "/api/v1/sandboxes/" + id + "/ports/0/"} {
		code, retry, body := w.do("GET", w.front.URL+p, nil, "")
		wantAnswer(t, p, code, retry, body, 404, "", textBadPath)
	}
	noToken := http.Header{"Authorization": {""}}
	code, retry, body := w.do("GET", w.url(guestPort, "/"), noToken, "")
	wantAnswer(t, "no token", code, retry, body, 401, "", textNoToken)
	code, retry, body = w.do("GET", w.url(guestPort, "/?access_token="+w.token), noToken, "")
	wantAnswer(t, "token in the query", code, retry, body, 401, "", textNoToken)
	code, retry, body = w.do("GET", w.url(guestPort, "/"), http.Header{"Authorization": {""},
		"Cookie": {"token=" + w.token}}, "")
	wantAnswer(t, "token in a cookie", code, retry, body, 401, "", textNoToken)
	code, retry, body = w.do("GET", w.url(guestPort, "/"), http.Header{"Authorization": {"Basic " + w.token}}, "")
	wantAnswer(t, "basic", code, retry, body, 401, "", textNoToken)
	code, retry, body = w.do("GET", w.url(guestPort, "/"), http.Header{"Authorization": {"Bearer nope"}}, "")
	wantAnswer(t, "unknown token", code, retry, body, 404, "", textNoSandbox)
	code, retry, body = w.do("GET", w.url(guestPort, "/"), http.Header{"Authorization": {"Bearer token-of-other"}}, "")
	wantAnswer(t, "foreign token", code, retry, body, 404, "", textNoSandbox)
	code, retry, body = w.do("GET", w.front.URL+"/fwd/not-a-uuid/5173/", nil, "")
	wantAnswer(t, "not a uuid", code, retry, body, 404, "", textNoSandbox)

	w.failLookup.Store(true)
	code, retry, body = w.do("GET", w.url(guestPort, "/"), nil, "")
	wantAnswer(t, "lookup", code, retry, body, 500, "", textLookupFailed)
	w.failLookup.Store(false)

	w.kube.mu.Lock()
	w.kube.ports = []int{6443}
	w.kube.mu.Unlock()
	code, retry, body = w.do("GET", w.url(6443, "/api"), nil, "")
	wantAnswer(t, "kube port", code, retry, body, 403, "", "port 6443 is a Kubernetes listener and is never forwarded")
	w.kube.mu.Lock()
	w.kube.err = errors.New("no database")
	w.kube.mu.Unlock()
	code, retry, body = w.do("GET", w.url(guestPort, "/"), nil, "")
	wantAnswer(t, "binding lookup", code, retry, body, 500, "", "binding lookup failed")
	if n := w.forwards.Load(); n != 0 {
		t.Errorf("%d forwards for refused requests", n)
	}
	if n := w.eng.Calls("Acquire", w.sb.ID); n != 1 {
		t.Errorf("%d acquires for refused requests (1 is the world's)", n)
	}
}

// The token stops at the door, the Host the client named goes on, and the
// rest of the path keeps its escaping, on both mounts.
func TestForwardHeadersAndPath(t *testing.T) {
	w := newWorld(t)
	hdr := http.Header{"Proxy-Authorization": {"Basic Zm9vOmJhcg=="}, "Host": {"5173--wt1.preview.example.test"}}
	code, _, body := w.do("POST", w.url(guestPort, "/a%2Fb/c?x=1&y=%20"), hdr, "hello")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var s seen
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	want := seen{Method: "POST", Host: "5173--wt1.preview.example.test", Path: "/a%2Fb/c", Query: "x=1&y=%20",
		Body: "hello"}
	if s != want {
		t.Errorf("the guest saw %+v, want %+v", s, want)
	}
	for path, wantPath := range map[string]string{
		"/fwd/" + w.sb.ID.String() + "/5173":                      "/",
		"/fwd/" + w.sb.ID.String() + "/5173/":                     "/",
		"/api/v1/sandboxes/" + w.sb.ID.String() + "/ports/5173":   "/",
		"/api/v1/sandboxes/" + w.sb.ID.String() + "/ports/5173/x": "/x",
	} {
		code, _, body := w.do("GET", w.front.URL+path, nil, "")
		if code != 200 {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		if got := w.guest.lastSeen(); got.Path != wantPath || got.Authorization != "" {
			t.Errorf("%s: the guest saw %+v", path, got)
		}
	}
	w.noHoldsLeft()
	if touches, _ := w.eng.counts(); touches != 5 {
		t.Errorf("%d touches for 5 requests", touches)
	}
}

// A run of requests rides one pooled connection, one Forward stream.
func TestPooledConnectionReused(t *testing.T) {
	w := newWorld(t)
	for i := range 10 {
		if code, _, body := w.do("GET", w.url(guestPort, fmt.Sprintf("/m%d.js", i)), nil, ""); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
	}
	if n := w.forwards.Load(); n != 1 {
		t.Errorf("%d Forward streams for 10 sequential requests", n)
	}
	if n := w.guest.opened.Load(); n != 1 {
		t.Errorf("%d guest connections for 10 sequential requests", n)
	}
}

// A hundred concurrent GETs to one port all succeed, through at most 32
// connections (the requests beyond wait for a pooled one).
func TestHundredConcurrentGets(t *testing.T) {
	w := newWorld(t)
	w.guest.delay.Store(int64(30 * time.Millisecond))
	var wg sync.WaitGroup
	errs := make(chan string, 100)
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", w.url(guestPort, fmt.Sprintf("/src/m%d.ts", i)), nil)
			req.Header.Set("Authorization", "Bearer "+w.token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err.Error()
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				errs <- fmt.Sprintf("%d %s", resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if n := w.guest.maxIn.Load(); n > 32 || n < 2 {
		t.Errorf("%d requests at once in the guest", n)
	}
	if n := w.forwards.Load(); n > 32 {
		t.Errorf("%d Forward streams", n)
	}
	w.noHoldsLeft()
}

// Every request in flight holds the sandbox; their ends release them and
// count as activity.
func TestHoldPerRequest(t *testing.T) {
	w := newWorld(t)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _, body := w.do("GET", w.url(guestPort, "/hold"), nil, ""); code != 200 {
				t.Errorf("%d %s", code, body)
			}
		}()
	}
	eventually(t, 10*time.Second, "three requests in the guest", func() bool { return w.guest.inFlight.Load() == 3 })
	if n := w.eng.Holds(w.sb.ID); n != 3 {
		t.Errorf("%d holds for 3 requests in flight", n)
	}
	close(w.guest.release)
	wg.Wait()
	w.noHoldsLeft()
	if touches, _ := w.eng.counts(); touches != 3 {
		t.Errorf("%d touches", touches)
	}
}

// The sandbox's cap of Forward streams on the instance is a waiting one:
// a dial past it waits for a slot (idle pooled connections give theirs
// up), and only after the wait gets 503 with Retry-After: 1.
func TestWaitingCap(t *testing.T) {
	w := newWorld(t, func(tm *timings) {
		tm.perSandbox = 2
		tm.capWait = 1500 * time.Millisecond
	})
	for _, p := range []uint32{5174, 5175} {
		w.env.Node.Script().SetPortAddr(p, w.guest.addr())
	}
	// Two requests hold both slots.
	var wg sync.WaitGroup
	for _, p := range []int{guestPort, 5174} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _, body := w.do("GET", w.url(p, "/hold"), nil, ""); code != 200 {
				t.Errorf("%d %s", code, body)
			}
		}()
	}
	eventually(t, 10*time.Second, "two requests in the guest", func() bool { return w.guest.inFlight.Load() == 2 })
	start := time.Now()
	code, retry, body := w.do("GET", w.url(5175, "/x"), nil, "")
	wantAnswer(t, "past the cap", code, retry, body, 503, "1",
		"too many connections into this sandbox on this instance, try again")
	if d := time.Since(start); d < 1500*time.Millisecond {
		t.Errorf("answered after %v, before the cap's wait", d)
	}

	// A slot that frees up within the wait (a request ends, its connection
	// goes back to its pool) serves the waiting one.
	done := make(chan string, 1)
	go func() {
		code, _, body := w.do("GET", w.url(5175, "/y"), nil, "")
		done <- fmt.Sprintf("%d %s", code, body)
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case got := <-done:
		t.Fatalf("answered while both slots were taken: %s", got)
	default:
	}
	close(w.guest.release)
	select {
	case got := <-done:
		if !strings.HasPrefix(got, "200 ") {
			t.Errorf("the waiting request: %s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting request was not answered")
	}
	wg.Wait()
}

// The failures of the node and the guest, as the door answers them.
func TestErrorMapping(t *testing.T) {
	w := newWorld(t)
	script := w.env.Node.Script()
	forward := portforwardv1.PortForwardService_Forward_FullMethodName

	// Nothing listens on the port.
	script.SetPortAddr(5199, "127.0.0.1:1")
	code, retry, body := w.do("GET", w.url(5199, "/"), nil, "")
	wantAnswer(t, "nothing listens", code, retry, body, 502, "", "nothing answered on port 5199 in this sandbox")

	// The guest refuses a Kubernetes listener.
	script.SetKubePorts(w.sb.ID, 5173)
	code, retry, body = w.do("GET", w.url(guestPort, "/"), nil, "")
	wantAnswer(t, "kube listener", code, retry, body, 403, "", "port 5173 is a Kubernetes listener and is never forwarded")
	script.SetKubePorts(w.sb.ID)

	// The guest does not answer.
	script.SetGuestUnreachable(w.sb.ID, true)
	code, retry, body = w.do("GET", w.url(guestPort, "/"), nil, "")
	wantAnswer(t, "unreachable guest", code, retry, body, 503, "1", "the sandbox's guest does not answer, try again")
	script.SetGuestUnreachable(w.sb.ID, false)

	// A node that does not forward ports (today's feosd), and one out of
	// streams.
	hook := func(err error) {
		script.SetHook(forward, func(context.Context, string, any) error { return err })
	}
	hook(status.Errorf(codes.Unimplemented, "%s is not served for pods", forward))
	code, retry, body = w.do("GET", w.url(guestPort, "/"), nil, "")
	wantAnswer(t, "unimplemented", code, retry, body, 502, "", "node n1 does not forward ports yet")
	hook(status.Error(codes.ResourceExhausted, "node n1: stream budget exhausted"))
	code, retry, body = w.do("GET", w.url(guestPort, "/"), nil, "")
	wantAnswer(t, "budget", code, retry, body, 503, "1", "node n1: stream budget exhausted, try again")
	hook(status.Error(codes.Internal, "something else"))
	code, retry, body = w.do("GET", w.url(guestPort, "/"), nil, "")
	wantAnswer(t, "other", code, retry, body, 502, "", "node n1 did not carry the forward")
	script.SetHook(forward, nil)

	// The engine's failures.
	w.eng.FailAcquire(w.sb.ID, model.ErrNotFound)
	code, retry, body = w.do("GET", w.url(guestPort, "/"), nil, "")
	wantAnswer(t, "gone", code, retry, body, 404, "", textNoSandbox)
	w.eng.FailAcquire(w.sb.ID, fmt.Errorf("%w: node n1 is not connected", model.ErrNodeUnavailable))
	code, retry, body = w.do("GET", w.url(guestPort, "/"), nil, "")
	wantAnswer(t, "node unavailable", code, retry, body, 503, "5", "the sandbox's node is unavailable")
	w.eng.FailAcquire(w.sb.ID, errors.New("no capacity on node n1"))
	code, retry, body = w.do("GET", w.url(guestPort, "/"), nil, "")
	wantAnswer(t, "not brought up", code, retry, body, 503, "5",
		"the sandbox could not be brought up: no capacity on node n1")
	w.noHoldsLeft()

	// No connection to the node: the lease says it runs there, the node
	// is cut off.
	w.eng.Stale(w.sb.ID, 1)
	w.env.Node.Partition(true)
	eventually(t, 10*time.Second, "the node gone", func() bool { return !w.env.Port.Connected(toolstest.NodeName) })
	code, retry, body = w.do("GET", w.url(guestPort, "/"), nil, "")
	wantAnswer(t, "no node", code, retry, body, 503, "5", "the sandbox's node is unavailable")
	w.env.Node.Partition(false)
	eventually(t, 30*time.Second, "the node back", func() bool { return w.env.Port.Connected(toolstest.NodeName) })
}

// A node that does not run the pod as the lease says (paused under the
// door's feet): a GET syncs, wakes and goes once more; a POST is told to
// try again while the sandbox wakes in the background.
func TestStalePod(t *testing.T) {
	w := newWorld(t)
	suspend := func() {
		t.Helper()
		pods, err := w.env.Pods(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pods.SuspendPod(context.Background(), &podv1.SuspendPodRequest{PodId: w.sb.ID.String()}); err != nil {
			t.Fatal(err)
		}
	}
	podState := func() podv1.PodState {
		st, err := w.env.PodState(context.Background(), w.sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	suspend()
	w.eng.Stale(w.sb.ID, 1)
	code, _, body := w.do("GET", w.url(guestPort, "/page"), nil, "")
	if code != 200 {
		t.Fatalf("GET after the pod paused: %d %s", code, body)
	}
	if n := w.eng.Calls("Sync", w.sb.ID); n != 1 {
		t.Errorf("%d syncs", n)
	}
	if st := podState(); st != podv1.PodState_POD_STATE_RUNNING {
		t.Errorf("pod %v", st)
	}

	// Another port: a pool of its own, without the pooled connection the
	// GET left (the test node keeps a stream that stood across a pause).
	w.env.Node.Script().SetPortAddr(5176, w.guest.addr())
	suspend()
	w.eng.Stale(w.sb.ID, 1)
	code, retry, body := w.do("POST", w.url(5176, "/api"), nil, "x")
	wantAnswer(t, "POST after the pod paused", code, retry, body, 503, "1", textWakingUp)
	eventually(t, 10*time.Second, "the background wake", func() bool {
		return podState() == podv1.PodState_POD_STATE_RUNNING && w.eng.Calls("Sync", w.sb.ID) == 2
	})
	eventually(t, 10*time.Second, "the wake's ensure", func() bool {
		_, ensures := w.eng.counts()
		return ensures == 1
	})
	w.noHoldsLeft()
}

// A client that gives up while its sandbox is brought up does not cancel
// the bringing up; the hold that arrives after it left is released.
func TestWakeDetachedFromRequest(t *testing.T) {
	w := newWorld(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	ctxErr := make(chan error, 1)
	w.eng.mu.Lock()
	w.eng.gate = func(ctx context.Context) {
		entered <- struct{}{}
		<-release
		ctxErr <- ctx.Err()
	}
	w.eng.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", w.url(guestPort, "/"), nil)
	req.Header.Set("Authorization", "Bearer "+w.token)
	failed := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		failed <- err
	}()
	<-entered
	cancel()
	if err := <-failed; err == nil {
		t.Fatal("the request did not fail with its context")
	}
	close(release)
	if err := <-ctxErr; err != nil {
		t.Errorf("the engine's context ended with the request: %v", err)
	}
	w.eng.mu.Lock()
	w.eng.gate = nil
	w.eng.mu.Unlock()
	eventually(t, 10*time.Second, "the late hold released", func() bool { return w.eng.Holds(w.sb.ID) == 0 })
	if n := w.forwards.Load(); n != 0 {
		t.Errorf("%d forwards for a request that left", n)
	}
}

// An upgraded connection rides through both ways and keeps its hold
// until it ends; CloseUpgraded closes it and returns once its hold is
// released. An upgrade after CloseUpgraded is closed at once.
func TestUpgradeAndCloseUpgraded(t *testing.T) {
	w := newWorld(t)
	conn, br := w.rawUpgrade("/ws")
	if got := w.guest.lastSeen(); got.Host != "5173--preview.example.test" || got.Authorization != "" || got.Path != "/ws" {
		t.Errorf("the guest saw %+v", got)
	}
	for _, msg := range []string{"ping", "pong and more"} {
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(br, buf); err != nil || string(buf) != msg {
			t.Fatalf("echo %q: %q %v", msg, buf, err)
		}
	}
	if n := w.eng.Holds(w.sb.ID); n != 1 {
		t.Errorf("%d holds while upgraded", n)
	}
	w.h.CloseUpgraded()
	if n := w.eng.Holds(w.sb.ID); n != 0 {
		t.Errorf("%d holds after CloseUpgraded", n)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Error("the upgraded connection is still open")
	}

	// After CloseUpgraded an upgrade is cut at once.
	conn2, br2 := w.rawUpgrade("/ws")
	_ = conn2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br2.ReadByte(); err == nil {
		t.Error("an upgrade after CloseUpgraded stays open")
	}
	eventually(t, 5*time.Second, "holds released", func() bool { return w.eng.Holds(w.sb.ID) == 0 })
}

// A 101 the proxy refuses (a protocol the client did not ask for, also to
// a request that asked for none) closes its connection: the Forward
// stream ends and its slot of the sandbox's cap is free again.
func TestRefusedUpgradeFreesItsSlot(t *testing.T) {
	w := newWorld(t, func(tm *timings) {
		tm.perSandbox = 1
		tm.capWait = time.Second
	})
	for _, hdr := range []http.Header{nil, {"Connection": {"Upgrade"}, "Upgrade": {"websocket"}}} {
		start := time.Now()
		code, retry, body := w.do("GET", w.url(guestPort, "/switch"), hdr, "")
		wantAnswer(t, "a refused 101", code, retry, body, 502, "", nothingAnswered(guestPort))
		if d := time.Since(start); d >= time.Second {
			t.Errorf("a refused 101 answered after %v, past the cap's wait", d)
		}
	}
	start := time.Now()
	if code, _, body := w.do("GET", w.url(guestPort, "/"), nil, ""); code != 200 {
		t.Fatalf("after the refused 101s: %d %s", code, body)
	}
	if d := time.Since(start); d >= time.Second {
		t.Errorf("the next request waited %v for the sandbox's one slot", d)
	}
	w.h.mu.Lock()
	upgraded, active := len(w.h.upgraded), w.h.upActive
	w.h.mu.Unlock()
	if upgraded != 0 || active != 0 {
		t.Errorf("%d upgraded connections tracked, %d counted", upgraded, active)
	}
	w.noHoldsLeft()
}

// Run retires a pool when its sandbox leaves running (ferun_sandbox): its
// idle connection closes; a notification that changes nothing keeps it.
func TestRunRetiresPools(t *testing.T) {
	w := newWorld(t)
	if code, _, body := w.do("GET", w.url(guestPort, "/"), nil, ""); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	eventually(t, 5*time.Second, "one pooled connection", func() bool { return w.guest.open.Load() == 1 })
	w.h.mu.Lock()
	e := w.h.transports[tkey{w.sb.ID, guestPort}]
	w.h.mu.Unlock()
	if e == nil {
		t.Fatal("no pool")
	}
	ctx := context.Background()
	setRow := func(state string) {
		t.Helper()
		if _, err := w.env.DB.Pool.Exec(ctx, "UPDATE sandboxes SET client_state = $2, node = $3, incarnation = $4 WHERE id = $1",
			w.sb.ID, state, e.node, e.incarnation); err != nil {
			t.Fatal(err)
		}
		if err := db.Notify(ctx, w.env.DB.Pool, db.ChanSandbox, w.sb.ID.String()); err != nil {
			t.Fatal(err)
		}
	}
	setRow(model.StateRunning)
	time.Sleep(300 * time.Millisecond)
	if n := w.guest.open.Load(); n != 1 {
		t.Fatalf("%d connections after a notification that changed nothing", n)
	}
	setRow(model.StatePaused)
	eventually(t, 5*time.Second, "the pooled connection closed", func() bool { return w.guest.open.Load() == 0 })
	w.h.mu.Lock()
	_, kept := w.h.transports[tkey{w.sb.ID, guestPort}]
	w.h.mu.Unlock()
	if kept {
		t.Error("the pool is still in service")
	}
	// The next request makes a new pool.
	if code, _, body := w.do("GET", w.url(guestPort, "/"), nil, ""); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
}

// The pools evict themselves when unused, and stay within their bound.
func TestPoolEviction(t *testing.T) {
	w := newWorld(t, func(tm *timings) {
		tm.maxTransports = 2
		tm.unusedAfter = 200 * time.Millisecond
	})
	for _, p := range []uint32{5174, 5175} {
		w.env.Node.Script().SetPortAddr(p, w.guest.addr())
	}
	for _, p := range []int{guestPort, 5174, 5175} {
		if code, _, body := w.do("GET", w.url(p, "/"), nil, ""); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
	}
	w.h.mu.Lock()
	n := len(w.h.transports)
	w.h.mu.Unlock()
	if n != 2 {
		t.Errorf("%d pools, want the bound 2", n)
	}
	time.Sleep(300 * time.Millisecond)
	w.h.sweep()
	w.h.mu.Lock()
	n = len(w.h.transports)
	w.h.mu.Unlock()
	if n != 0 {
		t.Errorf("%d pools after they went unused", n)
	}
	time.Sleep(retiredLinger)
	w.h.sweep()
	eventually(t, 5*time.Second, "every connection closed", func() bool { return w.guest.open.Load() == 0 })
	w.h.mu.Lock()
	n = len(w.h.retired)
	w.h.mu.Unlock()
	if n != 0 {
		t.Errorf("%d retired pools kept", n)
	}
}
