// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// fakeEngine counts Sync and Ensure per sandbox; Ensure waits until gate
// is closed.
type fakeEngine struct {
	gate chan struct{}

	mu      sync.Mutex
	synced  map[podid.ID]int
	ensured map[podid.ID]int
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{gate: make(chan struct{}), synced: map[podid.ID]int{}, ensured: map[podid.ID]int{}}
}

func (f *fakeEngine) Sync(ctx context.Context, id podid.ID) error {
	f.mu.Lock()
	f.synced[id]++
	f.mu.Unlock()
	return nil
}

func (f *fakeEngine) Ensure(ctx context.Context, id podid.ID) (model.Lease, error) {
	f.mu.Lock()
	f.ensured[id]++
	f.mu.Unlock()
	select {
	case <-f.gate:
	case <-ctx.Done():
		return model.Lease{}, ctx.Err()
	}
	return model.Lease{SandboxID: id}, nil
}

func (f *fakeEngine) counts(id podid.ID) (synced, ensured int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.synced[id], f.ensured[id]
}

func (s *Service) wakingCount() int {
	s.wakingMu.Lock()
	defer s.wakingMu.Unlock()
	return len(s.waking)
}

// Five Wakes at once wake each asleep instance once, in the background:
// Sync, then Ensure; running, creating and failed instances are not
// woken. A hibernated instance asks the ingress to look again later.
func TestWake(t *testing.T) {
	fe := newFakeEngine()
	h := newHarness(t, harnessOpts{engine: fe})
	h.world()
	env := h.mkEnv("web", webSpec, true)
	paused := []podid.ID{
		h.mkInstance(env, "web", model.PhaseReady, model.StatePaused, 8080),
		h.mkInstance(env, "web", model.PhaseUnready, model.StatePaused, 8080),
	}
	others := []podid.ID{
		h.mkInstance(env, "web", model.PhaseReady, model.StateRunning, 8080),
		h.mkInstance(env, "web", model.PhaseStarting, model.StateCreating),
		h.mkInstance(env, "web", model.PhaseFailed, model.StateFailed),
	}
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")

	var wg sync.WaitGroup
	resps := make([]*wire.WakeResponse, 5)
	errs := make([]error, 5)
	for i := range resps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resps[i], errs[i] = c.api.Wake(bg, &wire.WakeRequest{Env: env.ID.String(), Service: "web"})
		}()
	}
	wg.Wait()
	for i := range resps {
		if errs[i] != nil || resps[i].Waking != 2 || resps[i].RetryAfterSeconds != 5 {
			t.Fatalf("wake %d: %+v %v", i, resps[i], errs[i])
		}
	}
	eventually(t, 5*time.Second, "each asleep instance ensured", func() bool {
		for _, id := range paused {
			if _, ensured := fe.counts(id); ensured != 1 {
				return false
			}
		}
		return true
	})
	close(fe.gate)
	eventually(t, 5*time.Second, "the wakes over", func() bool { return h.s.wakingCount() == 0 })
	for _, id := range paused {
		if synced, ensured := fe.counts(id); synced != 1 || ensured != 1 {
			t.Errorf("%s: synced %d, ensured %d", id, synced, ensured)
		}
	}
	for _, id := range others {
		if synced, ensured := fe.counts(id); synced+ensured != 0 {
			t.Errorf("%s was woken: synced %d, ensured %d", id, synced, ensured)
		}
	}

	// A hibernated instance: look again in 10 s.
	h.setSandbox(paused[0], func(s *store.Sandbox) { s.ClientState = model.StateHibernated })
	resp, err := c.api.Wake(bg, &wire.WakeRequest{Env: env.ID.String(), Service: "web"})
	if err != nil || resp.Waking != 2 || resp.RetryAfterSeconds != 10 {
		t.Fatalf("hibernated: %+v %v", resp, err)
	}
	eventually(t, 5*time.Second, "woken again", func() bool {
		_, ensured := fe.counts(paused[0])
		return ensured == 2
	})
}

// Wake answers NOT_FOUND for a service that is not routed, an attached
// service, an unknown service, an unknown environment and one that is
// not public; INVALID_ARGUMENT for what is no ID or name; UNAVAILABLE
// before Run.
func TestWakeNotFound(t *testing.T) {
	fe := newFakeEngine()
	close(fe.gate)
	h := newHarness(t, harnessOpts{engine: fe})
	h.world()
	// web (image) behind /, dev (attached) behind /dev, worker (image)
	// without a route.
	raw, err := json.Marshal(spec.Spec{
		Services: map[string]spec.Service{
			"web":    {Runtime: spec.RuntimeImage, Image: "web@sha256:1", Ports: []spec.Port{{Name: "http", Port: 8080}}},
			"dev":    {Runtime: spec.RuntimeAttached, Sandbox: "sbx", Ports: []spec.Port{{Name: "http", Port: 3000}}},
			"worker": {Runtime: spec.RuntimeImage, Image: "worker@sha256:1", Ports: []spec.Port{{Name: "http", Port: 7070}}},
		},
		Routes: map[string]spec.Route{
			"root": {Path: "/", Service: "web", Port: spec.PortRef{Name: "http"}},
			"dev":  {Path: "/dev", Service: "dev", Port: spec.PortRef{Name: "http"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	public := h.mkEnv("pub", string(raw), true)
	worker := h.mkInstance(public, "worker", model.PhaseReady, model.StatePaused)
	private := h.mkEnv("priv", webSpec, false)
	hidden := h.mkInstance(private, "web", model.PhaseReady, model.StatePaused, 8080)
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	wake := func(env, service string) error {
		_, err := c.api.Wake(bg, &wire.WakeRequest{Env: env, Service: service})
		return err
	}
	pub := public.ID.String()
	wantCode(t, "unrouted", wake(pub, "worker"), codes.NotFound, "service worker of "+pub+" is not routed")
	wantCode(t, "attached", wake(pub, "dev"), codes.NotFound, "service dev of "+pub+" is not routed")
	wantCode(t, "unknown service", wake(pub, "nosuch"), codes.NotFound, "is not routed")
	unknown := uuid.NewString()
	wantCode(t, "unknown environment", wake(unknown, "web"), codes.NotFound, "no public environment "+unknown)
	wantCode(t, "not public", wake(private.ID.String(), "web"), codes.NotFound, "no public environment")
	wantCode(t, "no ID", wake("x", "web"), codes.InvalidArgument, "env")
	wantCode(t, "no name", wake(pub, "Bad_Name"), codes.InvalidArgument, "service")
	// A routed service without an asleep instance: nothing to wake.
	if resp, err := c.api.Wake(bg, &wire.WakeRequest{Env: pub, Service: "web"}); err != nil || resp.Waking != 0 ||
		resp.RetryAfterSeconds != 5 {
		t.Fatalf("web: %+v %v", resp, err)
	}
	// Deleting: not found.
	cur, err := h.st.GetEnvironment(bg, public.ID)
	if err != nil {
		t.Fatal(err)
	}
	after := cur
	after.Deleting = true
	if _, err := h.st.UpdateEnvironment(bg, cur, after); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "deleting", wake(pub, "web"), codes.NotFound, "no public environment")
	for _, id := range []podid.ID{worker, hidden} {
		if synced, ensured := fe.counts(id); synced+ensured != 0 {
			t.Errorf("%s was woken", id)
		}
	}

	// Before Run there is no context to wake on.
	idle := New(Config{InstanceID: "i2", TrustDomain: testTD, Log: h.s.cfg.Log}, Deps{DB: h.d, Issuer: h.iss, Engine: fe})
	ctx := nodelink.ContextWithIngress(bg, nodelink.IngressPeer{Name: "in-0", Serial: "ab", Conn: 1})
	_, err = idle.Wake(ctx, &wire.WakeRequest{Env: pub, Service: "web"})
	wantCode(t, "before Run", err, codes.Unavailable, "")
}
