// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	watchv1 "github.com/ironcore-dev/feos/go/gen/feos/watch/v1"
	"github.com/ironcore-dev/feos/go/internal/auth"
	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
	"github.com/ironcore-dev/feos/go/internal/podid"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/tools/toolstest"
)

// sandboxes is the engine's state reads in the tests: what the tests say.
type sandboxes struct {
	mu     sync.Mutex
	states map[podid.ID][2]string
}

func (f *sandboxes) State(_ context.Context, id podid.ID) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.states[id]
	if !ok {
		return "", "", model.ErrNotFound
	}
	return st[0], st[1], nil
}

func (f *sandboxes) set(id podid.ID, state, node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[id] = [2]string{state, node}
}

// instance is one Service with the four routes behind the sandbox class,
// on an HTTP server of its own, and the client that reaches it.
type instance struct {
	id     string
	s      *Service
	front  *httptest.Server
	client *http.Client
	stop   func()
}

// instanceOptions vary an instance: the node connections it uses (nil: the
// test node's), and HTTP/2 over TLS instead of HTTP/1.
type instanceOptions struct {
	conns nodelink.Conns
	h2    bool
}

// world is a test's sandbox with its pod running on the test node, and
// the instances that share the database and the node connection.
type world struct {
	t     *testing.T
	env   *toolstest.Env
	sbs   *sandboxes
	sb    auth.Sandbox
	token string
	// tokens maps the tests' bearers to sandboxes (the sandbox class).
	tokens map[string]auth.Sandbox
}

func newWorld(t *testing.T) *world {
	t.Helper()
	env := toolstest.New(t, toolstest.Options{PendingDelay: 20 * time.Millisecond})
	w := &world{t: t, env: env, sbs: &sandboxes{states: map[podid.ID][2]string{}}, tokens: map[string]auth.Sandbox{}}
	w.sb = w.newSandbox()
	w.token = w.tokenOf(w.sb)
	return w
}

// newSandbox records a sandbox and runs its pod on the node.
func (w *world) newSandbox() auth.Sandbox {
	w.t.Helper()
	sb := w.env.NewSandbox()
	w.tokens[w.tokenOf(sb)] = sb
	_, release, err := w.env.Engine.Acquire(context.Background(), sb.ID, model.HoldExec)
	if err != nil {
		w.t.Fatal(err)
	}
	release()
	w.sbs.set(sb.ID, model.StateRunning, toolstest.NodeName)
	if err := os.MkdirAll(filepath.Join(w.env.Node.PodDir(sb.ID), "src"), 0o755); err != nil {
		w.t.Fatal(err)
	}
	return sb
}

func (w *world) tokenOf(sb auth.Sandbox) string { return "token-of-" + sb.ID.String() }

// addInstance runs a Service as instance id; tune changes its timings.
func (w *world) addInstance(id string, tune ...func(*timings)) *instance {
	w.t.Helper()
	return w.addInstanceWith(id, instanceOptions{}, tune...)
}

// addInstanceWith runs a Service as instance id, as opts say.
func (w *world) addInstanceWith(id string, opts instanceOptions, tune ...func(*timings)) *instance {
	w.t.Helper()
	conns := opts.conns
	if conns == nil {
		conns = w.env.Port
	}
	s := New(w.sbs, conns, w.env.DB, id, w.env.Log.With("instance", id))
	for _, f := range tune {
		f(&s.t)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(ctx)
	}()
	var once sync.Once
	in := &instance{id: id, s: s}
	in.stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	class := func(h http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			sb, ok := w.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
			if !ok || r.PathValue("id") != sb.ID.String() {
				http.Error(rw, `{"error":"no such sandbox"}`, http.StatusNotFound)
				return
			}
			h(rw, r.WithContext(auth.WithSandbox(r.Context(), sb)))
		})
	}
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/sandboxes/{id}/watches", class(s.Create))
	mux.Handle("PUT /api/v1/sandboxes/{id}/watches/{wid}", class(s.Update))
	mux.Handle("GET /api/v1/sandboxes/{id}/watches/{wid}/events", class(s.Events))
	mux.Handle("DELETE /api/v1/sandboxes/{id}/watches/{wid}", class(s.Delete))
	in.front = httptest.NewUnstartedServer(mux)
	if opts.h2 {
		in.front.EnableHTTP2 = true
		in.front.StartTLS()
	} else {
		in.front.Start()
	}
	in.client = in.front.Client()
	w.t.Cleanup(func() {
		in.front.CloseClientConnections()
		in.front.Close()
		in.stop()
	})
	return in
}

// call sends a request as the sandbox sb and returns the status and body.
func (w *world) call(in *instance, sb auth.Sandbox, method, path, body string) (int, string) {
	w.t.Helper()
	req, err := http.NewRequest(method, in.front.URL+"/api/v1/sandboxes/"+sb.ID.String()+path, strings.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.tokenOf(sb))
	resp, err := in.client.Do(req)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func pathsBody(paths ...string) string {
	b, _ := json.Marshal(map[string]any{"paths": paths})
	return string(b)
}

// create POSTs a watch of the world's sandbox on in.
func (w *world) create(in *instance, paths ...string) response {
	w.t.Helper()
	code, body := w.call(in, w.sb, "POST", "/watches", pathsBody(paths...))
	if code != http.StatusCreated {
		w.t.Fatalf("POST: %d %s", code, body)
	}
	var res response
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		w.t.Fatal(err)
	}
	return res
}

// put PUTs a set on in and returns the answer.
func (w *world) put(in *instance, wid string, paths ...string) response {
	w.t.Helper()
	code, body := w.call(in, w.sb, "PUT", "/watches/"+wid, pathsBody(paths...))
	if code != http.StatusOK {
		w.t.Fatalf("PUT on %s: %d %s", in.id, code, body)
	}
	var res response
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		w.t.Fatal(err)
	}
	return res
}

// row reads a watch's row.
func (w *world) row(wid string) store.Watch {
	w.t.Helper()
	row, err := store.New(w.env.DB).GetWatch(context.Background(), uuid.MustParse(wid))
	if err != nil {
		w.t.Fatal(err)
	}
	return row
}

// guestStreams is the number of watch streams the node has open for the
// sandbox.
func (w *world) guestStreams() int { return w.env.Node.Script().Watches(w.sb.ID) }

// guestEvent sends one event to every watch stream of the sandbox's pod.
func (w *world) guestEvent(dir, name string) int {
	return w.env.Node.Script().WatchEvents(w.sb.ID, &watchv1.Event{Kind: watchv1.EventKind_EVENT_KIND_CREATED, Dir: dir,
		Name: name})
}

// notifySandbox sends ferun_sandbox for the world's sandbox.
func (w *world) notifySandbox() {
	w.t.Helper()
	if err := db.Notify(context.Background(), w.env.DB.Pool, db.ChanSandbox, w.sb.ID.String()); err != nil {
		w.t.Fatal(err)
	}
}

// sseEvent is one event of a stream: its id line and its data.
type sseEvent struct {
	id   uint64
	data string
}

// stream is an open event stream of a watch.
type stream struct {
	events   chan sseEvent
	comments atomic.Int64
	// end receives how the stream ended: nil for a clean end, an error
	// for a cut-off one.
	end    chan error
	cancel context.CancelFunc
}

// open opens the event stream of watch wid on in, with Last-Event-ID last
// unless it is 0, and expects 200.
func (w *world) open(in *instance, wid string, last uint64) *stream {
	w.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET",
		in.front.URL+"/api/v1/sandboxes/"+w.sb.ID.String()+"/watches/"+wid+"/events", nil)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Accept", "text/event-stream")
	if last > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(last, 10))
	}
	resp, err := in.client.Do(req)
	if err != nil {
		cancel()
		w.t.Fatal(err)
	}
	wantProto := 1
	if in.front.EnableHTTP2 {
		wantProto = 2
	}
	if resp.ProtoMajor != wantProto {
		w.t.Errorf("events over HTTP/%d, want HTTP/%d", resp.ProtoMajor, wantProto)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		w.t.Fatalf("events on %s: %d %s", in.id, resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-store" ||
		resp.Header.Get("X-Accel-Buffering") != "no" {
		w.t.Errorf("event stream headers %v", resp.Header)
	}
	st := &stream{events: make(chan sseEvent, 256), end: make(chan error, 1), cancel: cancel}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.data != "" {
					st.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, ":"):
				st.comments.Add(1)
			case strings.HasPrefix(line, "id: "):
				ev.id, _ = strconv.ParseUint(line[len("id: "):], 10, 64)
			case strings.HasPrefix(line, "data: "):
				ev.data = line[len("data: "):]
			}
		}
		st.end <- sc.Err()
	}()
	w.t.Cleanup(cancel)
	return st
}

// next returns the stream's next event, or fails the test after within.
func (st *stream) next(t *testing.T, within time.Duration) sseEvent {
	t.Helper()
	select {
	case ev := <-st.events:
		return ev
	case err := <-st.end:
		t.Fatalf("the stream ended (%v) while an event was expected", err)
	case <-time.After(within):
		t.Fatal("no event")
	}
	return sseEvent{}
}

// quiet checks that no event arrives for d.
func (st *stream) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case ev := <-st.events:
		t.Fatalf("unexpected event %d %s", ev.id, ev.data)
	case <-time.After(d):
	}
}

// ended waits for the stream's end and returns how it ended.
func (st *stream) ended(t *testing.T, within time.Duration) error {
	t.Helper()
	for {
		select {
		case <-st.events:
		case err := <-st.end:
			return err
		case <-time.After(within):
			t.Fatal("the stream did not end")
			return nil
		}
	}
}

// staleData is the data of a stale event for paths.
func staleData(id uint64, paths ...string) string {
	if paths == nil {
		paths = []string{}
	}
	b, _ := json.Marshal(paths)
	return `{"id":` + strconv.FormatUint(id, 10) + `,"kind":"stale","dir":"","cookie":0,"stalePaths":` + string(b) + `}`
}

// createdData is the data of a created event.
func createdData(id uint64, dir, name string) string {
	return `{"id":` + strconv.FormatUint(id, 10) + `,"kind":"created","dir":"` + dir + `","name":"` + name +
		`","isDir":false,"cookie":0}`
}

// eventually waits up to within for cond.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// epochID is the id of the n-th event of an epoch.
func epochID(epoch, n uint64) uint64 { return epoch<<32 | n }

// faults are the node connections of an instance with faults in the
// guest's watch streams, which the test node does not script: a stream
// that ends right after an ack (as a guest that drops it), an ack that
// arrives late. It records when each watch stream was opened.
type faults struct {
	nodelink.Conns

	mu sync.Mutex
	// endAfterAck ends every stream that has had an ack, at its next read;
	// endAlways every stream at its next read.
	endAfterAck, endAlways bool
	// lateAck holds the next ack back for that long.
	lateAck time.Duration
	opens   []time.Time
}

func newFaults(conns nodelink.Conns) *faults { return &faults{Conns: conns} }

func (f *faults) setEndAfterAck(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endAfterAck = on
}

func (f *faults) setEndAlways(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endAlways = on
}

func (f *faults) setLateAck(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lateAck = d
}

// opened returns when the watch streams were opened.
func (f *faults) opened() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time{}, f.opens...)
}

func (f *faults) Conn(ctx context.Context, node string) (grpc.ClientConnInterface, error) {
	cc, err := f.Conns.Conn(ctx, node)
	if err != nil {
		return nil, err
	}
	return &faultConn{ClientConnInterface: cc, f: f}, nil
}

type faultConn struct {
	grpc.ClientConnInterface
	f *faults
}

func (c *faultConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string,
	opts ...grpc.CallOption) (grpc.ClientStream, error) {
	cs, err := c.ClientConnInterface.NewStream(ctx, desc, method, opts...)
	if err != nil || method != watchv1.WatchService_Watch_FullMethodName {
		return cs, err
	}
	c.f.mu.Lock()
	c.f.opens = append(c.f.opens, time.Now())
	c.f.mu.Unlock()
	return &faultStream{ClientStream: cs, f: c.f}, nil
}

// faultStream is a watch stream with the faults; its one reader is the
// owner's.
type faultStream struct {
	grpc.ClientStream
	f     *faults
	acked bool
}

func (s *faultStream) RecvMsg(m any) error {
	s.f.mu.Lock()
	end := s.f.endAlways || s.acked && s.f.endAfterAck
	s.f.mu.Unlock()
	if end {
		return status.Error(codes.Unavailable, "the guest dropped the stream")
	}
	if err := s.ClientStream.RecvMsg(m); err != nil {
		return err
	}
	if res, ok := m.(*watchv1.WatchResponse); ok && res.GetAck() != nil {
		s.acked = true
		s.f.mu.Lock()
		late := s.f.lateAck
		s.f.lateAck = 0
		s.f.mu.Unlock()
		time.Sleep(late)
	}
	return nil
}
