// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/ironcore-dev/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/db/dbtest"
	"github.com/ironcore-dev/feos/go/internal/fleet"
	"github.com/ironcore-dev/feos/go/internal/leader"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
	"github.com/ironcore-dev/feos/go/internal/pki"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

var bg = context.Background()

const (
	testTD     = "feos.test"
	testTenant = "t1"
)

// issuerDir holds the platform CA of every test of the package.
var issuerDir string

func TestMain(m *testing.M) {
	base, err := os.MkdirTemp("", "sandbox-test-ca-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	issuerDir = filepath.Join(base, "ca")
	err = pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: issuerDir, TrustDomain: testTD,
		DNSDomains: []string{testTD}})
	code := 1
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	} else {
		code = m.Run()
	}
	os.RemoveAll(base)
	os.Exit(code)
}

// instance is one ferund instance of a test: its nodes' port, fleet,
// engine, elector and heartbeat.
type instance struct {
	id   string
	srv  *nodelink.Server
	fl   *fleet.Service
	eng  *Engine
	el   *leader.Elector
	hb   *leader.Heartbeat
	stop func()
}

type harness struct {
	t    *testing.T
	d    *db.DB
	st   *store.Store
	iss  *pki.Issuer
	objs *testnode.ObjectStore
	ins  []*instance

	mu    sync.Mutex
	nodes map[string]*testnode.Node
}

// newHarness gives the test a database with the tenant t1 and one
// instance; mod changes the engine's configuration.
func newHarness(t *testing.T, mod ...func(*Config)) *harness {
	t.Helper()
	t.Parallel()
	d := dbtest.New(t)
	iss, err := pki.LoadIssuer(issuerDir, testTD)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, d: d, st: store.New(d), iss: iss, objs: testnode.NewObjectStore("store-1"),
		nodes: map[string]*testnode.Node{}}
	if _, _, err := h.st.CreateTenant(bg, testTenant); err != nil {
		t.Fatal(err)
	}
	h.addInstance("i1", mod...)
	return h
}

// addInstance starts an instance; its engine has the tests' timings.
func (h *harness) addInstance(id string, mod ...func(*Config)) *instance {
	t := h.t
	t.Helper()
	log := dbtest.Logger(t)
	in := &instance{id: id}
	src, err := pki.NewServerCertSource(h.iss, []string{h.iss.ControlPlaneDomain()}, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := nodelink.New(nodelink.Config{
		Addr: "127.0.0.1:0", TrustDomain: testTD, InstanceID: id, Bundle: h.iss.Bundle(),
		ServerCert: src.GetCertificate, NodeWait: 500 * time.Millisecond, ConnDrainTimeout: time.Minute,
		CheckNode: func(ctx context.Context, node, serial, addr string, atAccept bool) (time.Time, error) {
			return in.fl.CheckNode(ctx, node, serial, addr, atAccept)
		},
		OnPresented: func(node, serial, addr string) { in.fl.OnPresented(node, serial, addr) },
		OnNodeConn:  func(node string) { in.fl.OnNodeConn(node) },
		Log:         log,
	})
	if err != nil {
		t.Fatal(err)
	}
	in.srv = srv
	in.el = leader.NewElector(h.d, id, log)
	cfg := Config{
		InstanceID: id, TrustDomain: testTD, Placement: PlacementBinpack, DiskBudget: 1 << 30,
		NodeWait: 500 * time.Millisecond, Log: log,
	}
	for _, m := range mod {
		m(&cfg)
	}
	in.eng, err = New(cfg, Deps{DB: h.d, Conns: srv, Elector: in.el})
	if err != nil {
		t.Fatal(err)
	}
	in.eng.resolveImage = fakeResolve
	in.eng.t.waitPoll = 100 * time.Millisecond
	in.eng.t.deferredEvery = 500 * time.Millisecond
	in.fl = fleet.New(fleet.Config{InstanceID: id, TrustDomain: testTD, RolloutGrace: 30 * time.Second,
		NodeTTL: time.Hour, ConnDrainTimeout: time.Minute, Log: log},
		fleet.Deps{DB: h.d, Issuer: h.iss, Links: srv, Hooks: in.eng, Elector: in.el})
	fleetv1.RegisterFleetServiceServer(srv.Uplink(), in.fl.UplinkServer())
	fleetv1.RegisterFleetServiceServer(srv.Join(), in.fl.JoinServer())
	in.hb = leader.NewHeartbeat(h.d, leader.InstanceInfo{ID: id, NodeAddress: srv.Addr().String()}, log)

	ctx, cancel := context.WithCancel(bg)
	var wg sync.WaitGroup
	run := func(f func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f(ctx)
		}()
	}
	run(srv.Serve)
	run(in.fl.Run)
	run(in.eng.Run)
	run(in.el.Run)
	run(in.hb.Run)
	var once sync.Once
	in.stop = func() {
		once.Do(func() {
			sctx, scancel := context.WithTimeout(bg, 10*time.Second)
			srv.Shutdown(sctx)
			scancel()
			cancel()
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Errorf("instance %s did not stop", id)
			}
		})
	}
	t.Cleanup(in.stop)
	h.ins = append(h.ins, in)
	if len(h.ins) == 1 {
		eventually(t, 10*time.Second, "instance "+id+" leads", in.el.IsLeader)
	}
	return in
}

// failNextOp makes the next op of the first instance find no connection
// to its target after its begin, running before first (when set).
func (h *harness) failNextOp(before func()) {
	var once sync.Once
	h.eng().connHook = func(hd *OpHandle) error {
		var err error
		once.Do(func() {
			if before != nil {
				before()
			}
			err = status.Errorf(codes.Unavailable, "node %s is not connected (injected)", hd.target)
		})
		return err
	}
}

// settle waits until the nodes' events are acknowledged and their syncs
// ran, and syncs the sandbox once more: nothing from earlier ops arrives
// after it.
func (h *harness) settle(id uuid.UUID) {
	h.t.Helper()
	eventually(h.t, 10*time.Second, "events acknowledged", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, n := range h.nodes {
			if n.EventsPending() > 0 {
				return false
			}
		}
		return true
	})
	time.Sleep(200 * time.Millisecond)
	if err := h.eng().Sync(bg, id); err != nil {
		h.t.Fatal(err)
	}
}

// eng is the first instance's engine.
func (h *harness) eng() *Engine { return h.ins[0].eng }

// fakeResolve pins an image without a registry: a digest reference as it
// is, a tag to a digest made from its name.
func fakeResolve(_ context.Context, image string) (string, error) {
	ref, err := parseImageRef(image)
	if err != nil {
		return "", err
	}
	if ref.digest() {
		return ref.pinned(ref.ref), nil
	}
	return ref.pinned(fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(image)))), nil
}

// addNode joins a test node and waits until every instance has its node
// connection, it is ready, and its first inventory reconcile is done.
func (h *harness) addNode(name string, mod ...func(*testnode.Config)) *testnode.Node {
	t := h.t
	t.Helper()
	_, token, _, err := h.ins[0].fl.IssueJoinToken(bg, "test", name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testnode.Config{
		Name: name, JoinToken: token, ServerName: h.iss.ControlPlaneDomain(), TrustDomain: testTD,
		Addrs: []string{h.ins[0].srv.Addr().String()}, ObjectStore: h.objs, PendingDelay: 20 * time.Millisecond,
		Log: dbtest.Logger(t),
	}
	for _, m := range mod {
		m(&cfg)
	}
	n, err := testnode.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("node %s did not stop", name)
		}
	})
	h.mu.Lock()
	h.nodes[name] = n
	h.mu.Unlock()
	eventually(t, 20*time.Second, "node "+name+" connected and ready", func() bool {
		for _, in := range h.ins {
			if !in.srv.Connected(name) {
				return false
			}
		}
		nd, err := h.st.GetNode(bg, name)
		return err == nil && nd.Status == model.NodeReady && !nd.ReconciledAt.IsZero()
	})
	return n
}

func (h *harness) node(name string) *testnode.Node {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nodes[name]
}

// pods is a PodService client on the first instance's connection to node.
func (h *harness) pods(node string) podv1.PodServiceClient {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	cc, err := h.ins[0].srv.Conn(ctx, node)
	if err != nil {
		h.t.Fatal(err)
	}
	return podv1.NewPodServiceClient(cc)
}

// testImage is a pinned image: no registry is asked.
const testImage = "docker.io/library/alpine@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1"

// sandbox inserts a sandbox of the test tenant.
func (h *harness) sandbox(memoryMiB int) store.Sandbox {
	return h.sandboxWithID(uuid.New(), memoryMiB)
}

func (h *harness) sandboxWithID(id uuid.UUID, memoryMiB int) store.Sandbox {
	h.t.Helper()
	sb, created, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: id, Owner: testTenant, Image: testImage, VCPUs: 1,
		MemoryMiB: memoryMiB})
	if err != nil || !created {
		h.t.Fatalf("create sandbox: %v (created %v)", err, created)
	}
	return sb
}

func (h *harness) row(id uuid.UUID) store.Sandbox {
	h.t.Helper()
	sb, err := h.st.GetSandbox(bg, id)
	if err != nil {
		h.t.Fatalf("sandbox %s: %v", id, err)
	}
	return sb
}

// ensure runs Ensure on the first instance and fails the test on an
// error.
func (h *harness) ensure(id uuid.UUID) model.Lease {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	lease, err := h.eng().Ensure(ctx, id)
	if err != nil {
		h.t.Fatalf("ensure %s: %v", id, err)
	}
	return lease
}

// running creates a sandbox and ensures it.
func (h *harness) running(memoryMiB int) store.Sandbox {
	h.t.Helper()
	sb := h.sandbox(memoryMiB)
	h.ensure(sb.ID)
	return h.row(sb.ID)
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.d.Pool.Exec(bg, sql, args...); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

func (h *harness) scalar(dst any, sql string, args ...any) {
	h.t.Helper()
	if err := h.d.Pool.QueryRow(bg, sql, args...).Scan(dst); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

// waitState waits until the sandbox is in state without an op.
func (h *harness) waitState(id uuid.UUID, state string, within time.Duration) store.Sandbox {
	h.t.Helper()
	var sb store.Sandbox
	eventually(h.t, within, fmt.Sprintf("sandbox %s %s", id, state), func() bool {
		var err error
		sb, err = h.st.GetSandbox(bg, id)
		return err == nil && sb.ClientState == state && sb.Op == ""
	})
	return sb
}

// podState waits until node holds the pod in state.
func (h *harness) podState(node string, id uuid.UUID, state podv1.PodState) *podv1.PodInfo {
	h.t.Helper()
	var info *podv1.PodInfo
	eventually(h.t, 15*time.Second, fmt.Sprintf("pod %s %s on %s", id, state, node), func() bool {
		var err error
		info, err = h.pods(node).GetPod(bg, &podv1.GetPodRequest{PodId: id.String()})
		return err == nil && info.State == state
	})
	return info
}

// noPod waits until node does not hold the pod.
func (h *harness) noPod(node string, id uuid.UUID) {
	h.t.Helper()
	eventually(h.t, 15*time.Second, fmt.Sprintf("pod %s gone from %s", id, node), func() bool {
		_, err := h.pods(node).GetPod(bg, &podv1.GetPodRequest{PodId: id.String()})
		return status.Code(err) == codes.NotFound
	})
}

// booking returns the sandbox's booking, and false without one.
func (h *harness) booking(id uuid.UUID) (store.Booking, bool) {
	h.t.Helper()
	b, err := h.st.GetBooking(bg, id)
	if errors.Is(err, store.ErrNoRow) {
		return store.Booking{}, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return b, true
}

func (h *harness) tombstones(id uuid.UUID) []store.Tombstone {
	h.t.Helper()
	ts, err := h.st.TombstonesOfPod(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return ts
}

// deadInstance makes an instance row that is not live: seen long ago.
func (h *harness) deadInstance(id string) {
	h.exec(`INSERT INTO instances (id, last_seen) VALUES ($1, now() - interval '1 hour')
		ON CONFLICT (id) DO UPDATE SET last_seen = excluded.last_seen`, id)
}

// liveInstance makes an instance row that is live.
func (h *harness) liveInstance(id string) {
	h.exec(`INSERT INTO instances (id, last_seen) VALUES ($1, now())
		ON CONFLICT (id) DO UPDATE SET last_seen = now(), stopping = NULL`, id)
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func never(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("happened: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func wantState(t *testing.T, sb store.Sandbox, state string) {
	t.Helper()
	if sb.ClientState != state {
		t.Fatalf("sandbox %s is %s (%q), want %s", sb.ID, sb.ClientState, sb.StatusDescription, state)
	}
}

func wantErr(t *testing.T, err, kind error, text string) {
	t.Helper()
	if err == nil || (kind != nil && !errors.Is(err, kind)) || !strings.Contains(err.Error(), text) {
		t.Fatalf("got error %v, want %v containing %q", err, kind, text)
	}
}

// gate holds the first call of method on node until release is called;
// arrived is closed when that call reached the node. Later calls pass.
func (h *harness) gate(node, method string) (arrived <-chan struct{}, release func()) {
	in := make(chan struct{})
	out := make(chan struct{})
	var first, once sync.Once
	h.node(node).Script().SetHook(method, func(ctx context.Context, _ string, _ any) error {
		held := false
		first.Do(func() {
			held = true
			close(in)
		})
		if !held {
			return nil
		}
		select {
		case <-out:
		case <-ctx.Done():
		}
		return nil
	})
	return in, func() { once.Do(func() { close(out) }) }
}

// waitClosed waits for ch, or fails the test.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(15 * time.Second):
		t.Fatalf("not within 15s: %s", what)
	}
}
