// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// fakeHolder is the control plane's Hold: it refuses the instances in
// refuse, fails with err, and waits for block before it answers.
type fakeHolder struct {
	mu     sync.Mutex
	calls  []wire.HoldRequest
	refuse map[string]string
	err    error
	block  chan struct{}
}

func (f *fakeHolder) Hold(ctx context.Context, req wire.HoldRequest) (wire.HoldResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	block, err := f.block, f.err
	refuse := f.refuse
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return wire.HoldResponse{}, ctx.Err()
		}
	}
	if err != nil {
		return wire.HoldResponse{}, err
	}
	res := wire.HoldResponse{LeaseSeconds: 30}
	for _, id := range req.Hold {
		if reason, ok := refuse[id]; ok {
			res.Refused = append(res.Refused, wire.HoldRefusal{Instance: id, Reason: reason})
		} else {
			res.Held = append(res.Held, id)
		}
	}
	return res, nil
}

func (f *fakeHolder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeHolder) call(i int) wire.HoldRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

func idleSvc(name string, eps ...wire.Endpoint) wire.Service {
	s := svc(name, eps...)
	s.Idle = true
	return s
}

// Concurrent first requests to an idle service's instance share one
// Hold, and none reaches the instance before it is held.
func TestHoldsAdmitOnce(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	holder := &fakeHolder{block: make(chan struct{})}
	p, srv := newTestProxy(t, Config{}, Deps{Holder: holder}, fn, "n1")
	p.SetState(testState(idleSvc("web", ep("w1", "fd00::1", "n1"))))
	done := make(chan result, 5)
	for range 5 {
		go func() { done <- send(context.Background(), srv, "GET", testHost, "/", nil) }()
	}
	eventually(t, "the hold call", func() bool { return holder.count() == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := holder.count(); n != 1 {
		t.Fatalf("%d hold calls for concurrent requests", n)
	}
	if fn.dialed("[fd00::1]:8080") != 0 {
		t.Fatal("a request reached the instance before its hold")
	}
	close(holder.block)
	for range 5 {
		decode(t, <-done)
	}
	if got := holder.call(0); !slices.Equal(got.Hold, []string{"w1"}) || len(got.Release) != 0 || got.Leaving {
		t.Errorf("hold call %+v", got)
	}
	// A valid hold: no call.
	decode(t, get(t, srv, testHost, "/"))
	if n := holder.count(); n != 1 {
		t.Errorf("%d hold calls with a valid hold", n)
	}
	if got := p.Held(); !slices.Equal(got, []string{"w1"}) {
		t.Errorf("held %v", got)
	}
}

// A refused instance is skipped until the routing changes; the request
// goes to another one.
func TestHoldsRefused(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "one", "fd00::1")
	upstream(t, fn, "two", "fd00::2")
	holder := &fakeHolder{refuse: map[string]string{"w1": "busy"}}
	p, srv := newTestProxy(t, Config{}, Deps{Holder: holder}, fn, "n1")
	state := func(pending int) *wire.State {
		s := idleSvc("web", ep("w1", "fd00::1", "n1"), ep("w2", "fd00::2", "n1"))
		s.Pending = pending
		return testState(s)
	}
	p.SetState(state(0))
	for range 3 {
		if e := decode(t, get(t, srv, testHost, "/")); e.Upstream != "two" {
			t.Fatalf("went to %s", e.Upstream)
		}
	}
	if n := holder.count(); n != 2 || holder.call(0).Hold[0] != "w1" || holder.call(1).Hold[0] != "w2" {
		t.Fatalf("hold calls %+v", holder.calls)
	}
	if fn.dialed("[fd00::1]:8080") != 0 {
		t.Error("a refused instance got a request")
	}
	if p.m.holdRefusals.Load() != 1 {
		t.Errorf("refusals %d", p.m.holdRefusals.Load())
	}
	// A routing change: the instance is tried again.
	p.SetState(state(1))
	for range 2 {
		decode(t, get(t, srv, testHost, "/"))
	}
	if n := holder.count(); n != 3 {
		t.Errorf("hold calls %d after the routing changed", n)
	}
	// Every instance refused: the request waits for the routing.
	holder.mu.Lock()
	holder.refuse["w2"] = "not running"
	holder.mu.Unlock()
	p2, srv2 := newTestProxy(t, Config{PendingHold: 200 * time.Millisecond}, Deps{Holder: holder}, fn, "n1")
	p2.SetState(state(0))
	start := time.Now()
	wantPlain(t, get(t, srv2, testHost, "/"), 503, "no ready instance of web")
	if time.Since(start) < 200*time.Millisecond {
		t.Error("answered without waiting for the routing")
	}
}

// A Holder that fails lets the request go on unheld.
func TestHoldsHolderError(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	holder := &fakeHolder{err: errors.New("not connected")}
	p, srv := newTestProxy(t, Config{}, Deps{Holder: holder}, fn, "n1")
	p.SetState(testState(idleSvc("web", ep("w1", "fd00::1", "n1"))))
	decode(t, get(t, srv, testHost, "/"))
	decode(t, get(t, srv, testHost, "/"))
	if n := holder.count(); n != 1 {
		t.Errorf("%d hold calls within the retry time", n)
	}
	if p.m.holdAdmitFailures.Load() != 1 || len(p.Held()) != 0 {
		t.Errorf("failures %d, held %v", p.m.holdAdmitFailures.Load(), p.Held())
	}
}

// Services without an idle policy never hold.
func TestHoldsNotIdle(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	holder := &fakeHolder{}
	p, srv := newTestProxy(t, Config{}, Deps{Holder: holder}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	for range 3 {
		decode(t, get(t, srv, testHost, "/"))
	}
	p.holds.pass(context.Background())
	if n := holder.count(); n != 0 {
		t.Errorf("%d hold calls for a service without idle", n)
	}
}

// The renew loop holds again the instances with a request in flight or
// in the last 10 s, and releases the quiet ones.
func TestHoldsRenew(t *testing.T) {
	holder := &fakeHolder{refuse: map[string]string{}}
	h := newHolds(holder, slog.New(slog.DiscardHandler), &metrics{})
	now := time.Unix(1_000_000, 0)
	h.now = func() time.Time { return now }
	ctx := context.Background()

	relA, refused := h.acquire(ctx, "a")
	if refused {
		t.Fatal("refused")
	}
	relB, _ := h.acquire(ctx, "b")
	relB()
	if n := holder.count(); n != 2 {
		t.Fatalf("hold calls %d", n)
	}
	if got := h.held(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("held %v", got)
	}
	// A valid hold needs no call.
	now = now.Add(5 * time.Second)
	relB2, _ := h.acquire(ctx, "b")
	relB2()
	if n := holder.count(); n != 2 {
		t.Fatalf("hold calls %d with valid holds", n)
	}
	// 15 s after b's last request: a (in flight) is renewed, b
	// released.
	now = now.Add(15 * time.Second)
	h.pass(ctx)
	if n := holder.count(); n != 3 {
		t.Fatalf("hold calls %d", n)
	}
	if got := holder.call(2); !slices.Equal(got.Hold, []string{"a"}) || !slices.Equal(got.Release, []string{"b"}) {
		t.Errorf("renew call %+v", got)
	}
	if got := h.held(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("held %v", got)
	}
	// a's request ends; 5 s later it is still active and renewed.
	relA()
	now = now.Add(5 * time.Second)
	h.pass(ctx)
	if got := holder.call(3); !slices.Equal(got.Hold, []string{"a"}) || len(got.Release) != 0 {
		t.Errorf("renew call %+v", got)
	}
	// Quiet: released and forgotten.
	now = now.Add(10 * time.Second)
	h.pass(ctx)
	if got := holder.call(4); len(got.Hold) != 0 || !slices.Equal(got.Release, []string{"a"}) {
		t.Errorf("release call %+v", got)
	}
	if got := h.held(); len(got) != 0 {
		t.Errorf("held %v", got)
	}
	now = now.Add(10 * time.Second)
	h.pass(ctx)
	if n := holder.count(); n != 5 {
		t.Errorf("hold calls %d with nothing to hold", n)
	}
	// After a release, a new request admits again.
	relA, _ = h.acquire(ctx, "a")
	relA()
	if n := holder.count(); n != 6 {
		t.Errorf("hold calls %d after a release", n)
	}
	// A renewal the control plane refuses ends the hold; one that fails
	// leaves it to lapse.
	holder.mu.Lock()
	holder.refuse["a"] = "not running"
	holder.mu.Unlock()
	h.pass(ctx)
	if got := h.held(); len(got) != 0 {
		t.Errorf("held %v after a refused renewal", got)
	}
	holder.mu.Lock()
	delete(holder.refuse, "a")
	holder.err = errors.New("unavailable")
	holder.mu.Unlock()
	relA, _ = h.acquire(ctx, "a")
	h.pass(ctx)
	if h.m.holdRenewFailures.Load() != 1 {
		t.Errorf("renew failures %d", h.m.holdRenewFailures.Load())
	}
	relA()
	// Draining admits nothing.
	holder.mu.Lock()
	holder.err = nil
	holder.mu.Unlock()
	h.drain()
	n := holder.count()
	now = now.Add(time.Minute)
	relC, refused := h.acquire(ctx, "c")
	relC()
	if refused || holder.count() != n {
		t.Errorf("admitted while draining")
	}
}

// A release in flight finishes before a new request admits the
// instance again, so it never lands after the admit.
func TestHoldsReleaseBeforeAdmit(t *testing.T) {
	holder := &fakeHolder{}
	h := newHolds(holder, slog.New(slog.DiscardHandler), &metrics{})
	var mu sync.Mutex
	now := time.Unix(1_000_000, 0)
	h.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	ctx := context.Background()
	rel, _ := h.acquire(ctx, "a")
	rel()
	mu.Lock()
	now = now.Add(15 * time.Second)
	mu.Unlock()
	block := make(chan struct{})
	holder.mu.Lock()
	holder.block = block
	holder.mu.Unlock()
	passed := make(chan struct{})
	go func() {
		h.pass(ctx)
		close(passed)
	}()
	eventually(t, "the release call", func() bool { return holder.count() == 2 })
	acquired := make(chan struct{})
	go func() {
		rel, _ := h.acquire(ctx, "a")
		rel()
		close(acquired)
	}()
	time.Sleep(50 * time.Millisecond)
	if n := holder.count(); n != 2 {
		t.Fatalf("admitted during the release: %d calls", n)
	}
	holder.mu.Lock()
	holder.block = nil
	holder.mu.Unlock()
	close(block)
	<-passed
	<-acquired
	calls := []string{}
	for i := range holder.count() {
		c := holder.call(i)
		calls = append(calls, "hold "+strings.Join(c.Hold, ",")+" release "+strings.Join(c.Release, ","))
	}
	want := []string{"hold a release ", "hold  release a", "hold a release "}
	if !slices.Equal(calls, want) {
		t.Errorf("calls %q, want %q", calls, want)
	}
}

// A response cut off mid-stream (ReverseProxy aborts it with
// http.ErrAbortHandler) still ends the request's count, so the renew
// loop releases the quiet instance, and the request counts as aborted.
func TestHoldsReleaseOnAbort(t *testing.T) {
	for _, c := range []struct {
		name    string
		handler http.HandlerFunc
		// cancel: the client goes away after the first line.
		cancel bool
	}{
		{"client gone", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: one\n\n")
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
		}, true},
		{"upstream reset", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			_, _ = io.WriteString(w, "partial\n")
			_ = http.NewResponseController(w).Flush()
			panic(http.ErrAbortHandler)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fn := newFakeNet()
			handlerAt(t, fn, "fd00::1", c.handler)
			holder := &fakeHolder{}
			p, srv := newTestProxy(t, Config{}, Deps{Holder: holder}, fn, "n1")
			p.SetState(testState(idleSvc("web", ep("w1", "fd00::1", "n1"))))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
			req.Host = testHost
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(res.Body)
			if s, err := br.ReadString('\n'); err != nil || (s != "data: one\n" && s != "partial\n") {
				t.Fatalf("first line %q %v", s, err)
			}
			if c.cancel {
				cancel()
			} else if _, err := io.ReadAll(br); err == nil {
				t.Error("a cut-off body read to its end")
			}
			_ = res.Body.Close()
			inflight := func() int {
				p.holds.mu.Lock()
				defer p.holds.mu.Unlock()
				if st := p.holds.inst["w1"]; st != nil {
					return st.inflight
				}
				return -1
			}
			eventually(t, "the request's count to end", func() bool { return inflight() == 0 })
			eventually(t, "the aborted count", func() bool {
				p.m.requestsMu.Lock()
				defer p.m.requestsMu.Unlock()
				return p.m.requests["aborted"] == 1 && p.m.requests["2xx"] == 0
			})
			// Quiet now: the next pass releases the instance.
			p.holds.mu.Lock()
			p.holds.now = func() time.Time { return time.Now().Add(time.Minute) }
			p.holds.mu.Unlock()
			p.holds.pass(context.Background())
			if got := holder.call(holder.count() - 1); len(got.Hold) != 0 || !slices.Equal(got.Release, []string{"w1"}) {
				t.Errorf("pass after the abort %+v", got)
			}
			if got := p.Held(); len(got) != 0 {
				t.Errorf("held %v", got)
			}
		})
	}
}
