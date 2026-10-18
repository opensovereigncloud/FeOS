// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ports

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironcore-dev/feos/go/internal/model"
)

// lease is the world's sandbox's lease, as the environments read it from
// the row.
func (w *world) lease() model.Lease {
	w.t.Helper()
	lease, release, err := w.env.Engine.Acquire(context.Background(), w.sb.ID, model.HoldExec)
	if err != nil {
		w.t.Fatal(err)
	}
	release()
	return lease
}

// Probe and DialPod reach the pod's port without a hold, an Acquire or a
// touch; Probe sends Host localhost:<port> and gives the status code.
func TestProbeAndDialPod(t *testing.T) {
	w := newWorld(t)
	lease := w.lease()
	acquires := w.eng.Calls("Acquire", w.sb.ID)
	touches, _ := w.eng.counts()
	ctx := context.Background()

	code, err := w.h.Probe(ctx, lease, guestPort, "/readyz?full=1")
	if err != nil || code != http.StatusOK {
		t.Fatalf("probe: %d %v", code, err)
	}
	if got := w.guest.lastSeen(); got.Host != "localhost:5173" || got.Path != "/readyz" || got.Query != "full=1" ||
		got.Method != "GET" {
		t.Fatalf("the guest saw %+v", got)
	}
	// The status as the guest answers it (426 for an upgrade it wants).
	if code, err := w.h.Probe(ctx, lease, guestPort, "/ws"); err != nil || code != http.StatusUpgradeRequired {
		t.Fatalf("probe /ws: %d %v", code, err)
	}
	if code, err := w.h.Probe(ctx, lease, guestPort, ""); err != nil || code != 200 || w.guest.lastSeen().Path != "/" {
		t.Fatalf("probe of no path: %d %v", code, err)
	}
	if _, err := w.h.Probe(ctx, lease, guestPort, "readyz"); err == nil {
		t.Fatal("a path without / probed")
	}
	// Nothing listens.
	w.env.Node.Script().SetPortAddr(5199, "127.0.0.1:1")
	if _, err := w.h.Probe(ctx, lease, 5199, "/"); err == nil {
		t.Fatal("a probe of a port nothing listens on passed")
	}
	if _, err := w.h.DialPod(ctx, lease, 5199); err == nil {
		t.Fatal("a dial of a port nothing listens on passed")
	}
	if _, err := w.h.DialPod(ctx, lease, 0); err == nil {
		t.Fatal("port 0 dialled")
	}

	// DialPod: a connection to the port, answered ready.
	conn, err := w.h.DialPod(ctx, lease, guestPort)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("GET /dialled HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("over the dialled connection: %v %v", resp, err)
	}
	var s seen
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil || s.Path != "/dialled" {
		t.Fatalf("the guest saw %+v %v", s, err)
	}
	conn.Close()

	if n := w.eng.Calls("Acquire", w.sb.ID); n != acquires {
		t.Errorf("%d acquires by the checks", n-acquires)
	}
	if now, _ := w.eng.counts(); now != touches {
		t.Errorf("%d touches by the checks", now-touches)
	}
	if n := w.eng.Holds(w.sb.ID); n != 0 {
		t.Errorf("%d holds", n)
	}
}

// slotsInUse is how many slots of the world's sandbox's stream cap and of
// its probe slots are taken.
func (w *world) slotsInUse() (streams, probes int) {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	if c := w.h.caps[w.sb.ID]; c != nil {
		streams = len(c.slots)
	}
	if c := w.h.probes[w.sb.ID]; c != nil {
		probes = len(c.slots)
	}
	return streams, probes
}

// A check takes a probe slot, never a slot of the sandbox's stream cap: a
// cap that forwarded traffic fills does not fail it. It never waits for a
// probe slot either: past them it fails at once (ErrProbeBusy). And it
// has its own timeout.
func TestProbeCapAndTimeout(t *testing.T) {
	w := newWorld(t, func(tm *timings) {
		tm.perSandbox = 1
		tm.capWait = time.Minute
		tm.probeTimeout = 300 * time.Millisecond
		tm.probeSlots = 1
	})
	lease := w.lease()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.do("GET", w.url(guestPort, "/hold"), nil, "")
	}()
	eventually(t, 10*time.Second, "the request in the guest", func() bool { return w.guest.inFlight.Load() == 1 })
	// The forwarded request holds the only slot of the cap; the probe
	// passes beside it.
	if code, err := w.h.Probe(context.Background(), lease, guestPort, "/"); err != nil || code != http.StatusOK {
		t.Fatalf("a probe beside a full stream cap: %d %v", code, err)
	}
	if streams, probes := w.slotsInUse(); streams != 1 || probes != 0 {
		t.Fatalf("after the probe: %d stream slots, %d probe slots taken, want 1 and 0", streams, probes)
	}

	// The probe slot taken by an open DialPod: the next check fails at
	// once, and passes once it is closed.
	conn, err := w.h.DialPod(context.Background(), lease, guestPort)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = w.h.Probe(context.Background(), lease, guestPort, "/")
	if !errors.Is(err, ErrProbeBusy) || time.Since(start) > time.Second {
		t.Fatalf("a probe past the probe slots: %v after %v", err, time.Since(start))
	}
	if _, err := w.h.DialPod(context.Background(), lease, guestPort); !errors.Is(err, ErrProbeBusy) {
		t.Fatalf("a dial past the probe slots: %v", err)
	}
	conn.Close()
	if code, err := w.h.Probe(context.Background(), lease, guestPort, "/"); err != nil || code != http.StatusOK {
		t.Fatalf("a probe once the slot is free: %d %v", code, err)
	}
	close(w.guest.release)
	wg.Wait()
	w.noHoldsLeft()

	// A guest slower than the probe's timeout fails it, and gives its slot
	// back.
	w.guest.delay.Store(int64(2 * time.Second))
	start = time.Now()
	_, err = w.h.Probe(context.Background(), lease, guestPort, "/slow")
	if err == nil || !strings.Contains(err.Error(), "did not answer within 300ms") || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("a slow probe: %v after %v", err, time.Since(start))
	}
	if _, probes := w.slotsInUse(); probes != 0 {
		t.Fatalf("%d probe slots taken after a timed out probe", probes)
	}
}

// With every slot of the sandbox's stream cap held by open forwarded
// streams, a probe and a dial still pass: they take probe slots, give
// them back, and leave the cap and the forwarding pools as they were.
func TestProbePastAFullStreamCap(t *testing.T) {
	w := newWorld(t)
	lease := w.lease()
	slots := defaultTimings().perSandbox
	var once sync.Once
	release := func() { once.Do(func() { close(w.guest.release) }) }
	t.Cleanup(release)
	var wg sync.WaitGroup
	for range slots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _, body := w.do("GET", w.url(guestPort, "/hold"), nil, ""); code != 200 {
				t.Errorf("%d %s", code, body)
			}
		}()
	}
	eventually(t, 10*time.Second, "every slot held by a forwarded stream", func() bool {
		return w.guest.inFlight.Load() == int64(slots)
	})
	if streams, _ := w.slotsInUse(); streams != slots {
		t.Fatalf("%d stream slots taken, want %d", streams, slots)
	}
	forwards := w.forwards.Load()
	start := time.Now()
	for range 3 {
		if code, err := w.h.Probe(context.Background(), lease, guestPort, "/readyz"); err != nil || code != http.StatusOK {
			t.Fatalf("a probe past a full stream cap: %d %v", code, err)
		}
	}
	conn, err := w.h.DialPod(context.Background(), lease, guestPort)
	if err != nil {
		t.Fatalf("a dial past a full stream cap: %v", err)
	}
	if streams, probes := w.slotsInUse(); streams != slots || probes != 1 {
		t.Fatalf("with a dial open: %d stream slots, %d probe slots taken, want %d and 1", streams, probes, slots)
	}
	conn.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the checks took %v", d)
	}
	if n := w.forwards.Load() - forwards; n != 4 {
		t.Errorf("%d Forward streams for 4 checks", n)
	}
	if streams, probes := w.slotsInUse(); streams != slots || probes != 0 {
		t.Fatalf("after the checks: %d stream slots, %d probe slots taken, want %d and 0", streams, probes, slots)
	}
	if n := w.guest.inFlight.Load(); n != int64(slots) {
		t.Errorf("%d forwarded requests in the guest, want %d", n, slots)
	}
	release()
	wg.Wait()
	w.noHoldsLeft()
	// The forwarded requests' connections went back to their pool, which
	// a request reuses; the checks' never entered it.
	opened := w.guest.opened.Load()
	if code, _, body := w.do("GET", w.url(guestPort, "/after"), nil, ""); code != 200 {
		t.Fatalf("a forward after the checks: %d %s", code, body)
	}
	if n := w.guest.opened.Load() - opened; n != 0 {
		t.Errorf("a forward after the checks opened %d connections, want a pooled one", n)
	}
	w.h.mu.Lock()
	left := len(w.h.probes)
	w.h.mu.Unlock()
	if left != 0 {
		t.Errorf("%d sandboxes with probe slots left", left)
	}
}

// routeFront serves ForwardTo as the route door would: /route/<rest> to
// the world's sandbox's guest port, the rest as the upstream path.
func (w *world) routeFront() *httptest.Server {
	// No ServeMux: it would clean the paths before the door sees them.
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		up := strings.TrimPrefix(r.URL.Path, "/route")
		raw := strings.TrimPrefix(r.URL.EscapedPath(), "/route")
		if raw == up {
			raw = ""
		}
		w.h.ForwardTo(rw, r, w.sb.ID, guestPort, up, raw)
	}))
	w.t.Cleanup(srv.Close)
	return srv
}

// ForwardTo forwards as the door does: the Host goes on, Authorization
// stops, the path keeps its escaping, the query stays, the sandbox is held
// for the request and touched; a ".." segment, also encoded, is refused,
// and so is a Kubernetes port; upgrades work.
func TestForwardTo(t *testing.T) {
	w := newWorld(t)
	front := w.routeFront()
	touches, _ := w.eng.counts()
	hdr := http.Header{"Authorization": {"Bearer route-token"}, "Host": {"stage.preview.example.test"}}
	code, _, body := w.do("POST", front.URL+"/route/api/a%2Fb?x=1", hdr, "hi")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var s seen
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	want := seen{Method: "POST", Host: "stage.preview.example.test", Path: "/api/a%2Fb", Query: "x=1", Body: "hi"}
	if s != want {
		t.Fatalf("the guest saw %+v, want %+v", s, want)
	}
	w.noHoldsLeft()
	if now, _ := w.eng.counts(); now != touches+1 {
		t.Errorf("%d touches for a request", now-touches)
	}

	for _, p := range []string{"/route/a/../b", "/route/a/%2e%2e/b", "/route/a%2F..%2Fb", "/route/a/%252e%252e/b", "/route/.."} {
		req, _ := http.NewRequest("GET", front.URL, nil)
		req.URL.Opaque = p // sent as written: the client cleans nothing
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || string(b) != textDotDot+"\n" {
			t.Errorf("%s: %d %q", p, resp.StatusCode, b)
		}
	}
	// "..." and "a..b" are names, not parent segments.
	if code, _, body := w.do("GET", front.URL+"/route/.../a..b", nil, ""); code != 200 {
		t.Errorf("dots in names: %d %s", code, body)
	}

	// The Kubernetes refusal.
	w.kube.mu.Lock()
	w.kube.ports = []int{guestPort}
	w.kube.mu.Unlock()
	code, retry, body := w.do("GET", front.URL+"/route/x", nil, "")
	wantAnswer(t, "kube", code, retry, body, 403, "", kubeText(guestPort))
	w.kube.mu.Lock()
	w.kube.ports = nil
	w.kube.mu.Unlock()

	// An upgrade through the route.
	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /route/ws HTTP/1.1\r\nHost: stage.example.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
}

func TestDotDot(t *testing.T) {
	for _, c := range []struct {
		path, raw string
		bad       bool
	}{
		{"/", "", false},
		{"/a/b", "", false},
		{"/a/..", "", true},
		{"..", "", true},
		{"/a/../b", "", true},
		{"/a/.../b", "", false},
		{"/a/b..c", "", false},
		{"/a/b", "/a%2F..%2Fb", true},
		{"/a/%2e%2e/b", "/a/%252e%252e/b", true},
		{"/a b", "/a%20b", false},
		{"/a", "/a%zz", true},
	} {
		if got := dotDot(c.path, c.raw); got != c.bad {
			t.Errorf("dotDot(%q, %q) = %v", c.path, c.raw, got)
		}
	}
}
