// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// begin begins an op on the first instance without waiting for its node
// (as the loops do) and returns the handle or the begin's error.
func (h *harness) begin(id uuid.UUID, in OpIntent) (*OpHandle, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if in.Incarnation == uuid.Nil {
		in.Incarnation = h.row(id).Incarnation
	}
	return h.eng().runner.start(ctx, id, in, false)
}

func (h *harness) holds(id uuid.UUID) []store.Hold {
	h.t.Helper()
	hs, err := h.st.ListHolds(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return hs
}

// idleSince makes the sandbox's last activity an hour old.
func (h *harness) idleSince(id uuid.UUID) {
	h.exec("UPDATE sandboxes SET last_active_at = now() - interval '1 hour' WHERE id = $1", id)
}

func TestRecheckPerTrigger(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.IdlePause = time.Minute; c.HibernateAfter = time.Minute })
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	pause := func(trigger OpTrigger) error {
		hd, err := h.begin(sb.ID, OpIntent{Kind: OpPause, Trigger: trigger, Target: "n1"})
		if err != nil {
			return err
		}
		res, _ := hd.Wait(bg)
		if res.State != model.StatePaused {
			t.Fatalf("%s pause: %+v", trigger, res)
		}
		return nil
	}

	// An exec holds the sandbox: idle, drain and ensure pauses do not
	// pass; an explicit pause does (as the old REST pause).
	_, release, err := e.Acquire(bg, sb.ID, model.HoldExec)
	if err != nil {
		t.Fatal(err)
	}
	h.idleSince(sb.ID)
	for _, tr := range []OpTrigger{TriggerIdle, TriggerDrain, TriggerEnsure} {
		wantErr(t, pause(tr), model.ErrPrecondition, "")
	}
	if row := h.row(sb.ID); row.ClientState != model.StateRunning || row.Op != "" {
		t.Fatalf("a failed recheck wrote: %+v", row)
	}
	if err := pause(TriggerExplicit); err != nil {
		t.Fatal(err)
	}
	release()
	h.ensure(sb.ID)

	// A process holds it: an idle pause does not pass, a drain pause
	// does (process holds are ignored there) and keeps the hold.
	if err := e.HoldProcess(bg, sb.ID, "p1", 0); err != nil {
		t.Fatal(err)
	}
	h.idleSince(sb.ID)
	wantErr(t, pause(TriggerIdle), model.ErrPrecondition, "")
	if err := pause(TriggerDrain); err != nil {
		t.Fatal(err)
	}
	if hs := h.holds(sb.ID); len(hs) != 1 || hs[0].Holder != "process:p1" || !hs[0].Infinite {
		t.Fatalf("holds after a drain pause %+v", hs)
	}
	h.ensure(sb.ID)
	if err := e.ReleaseProcess(bg, sb.ID, "p1"); err != nil {
		t.Fatal(err)
	}

	// No hold: an idle pause needs the idle time to have passed.
	h.exec("UPDATE sandboxes SET last_active_at = now() WHERE id = $1", sb.ID)
	wantErr(t, pause(TriggerIdle), model.ErrPrecondition, "")
	h.idleSince(sb.ID)
	if err := pause(TriggerIdle); err != nil {
		t.Fatal(err)
	}

	// hibernate_after: paused for long enough.
	hib := func() error {
		hd, err := h.begin(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerHibernateAfter, Target: "n1"})
		if err != nil {
			return err
		}
		res, _ := hd.Wait(bg)
		if res.State != model.StateHibernated {
			t.Fatalf("hibernate: %+v", res)
		}
		return nil
	}
	wantErr(t, hib(), model.ErrPrecondition, "")
	h.exec("UPDATE sandboxes SET paused_at = now() - interval '1 hour' WHERE id = $1", sb.ID)
	if err := hib(); err != nil {
		t.Fatal(err)
	}
}

func TestIdlePauseAdmissionInterleaved(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.IdlePause = time.Millisecond })
	h.addNode("n1")
	e := h.eng()

	// The admission holds the row while the idle pause's begin waits for
	// it: the pause's recheck, a later statement, sees the new hold.
	sb := h.running(512)
	h.idleSince(sb.ID)
	gate := make(chan struct{})
	locked := make(chan struct{}, 1)
	e.admitHook = func(context.Context, uuid.UUID) {
		select {
		case locked <- struct{}{}:
		default:
		}
		<-gate
	}
	type acquired struct {
		lease   model.Lease
		release func()
		err     error
	}
	acq := make(chan acquired, 1)
	go func() {
		l, r, err := e.Acquire(bg, sb.ID, model.HoldExec)
		acq <- acquired{l, r, err}
	}()
	<-locked
	paused := make(chan error, 1)
	go func() {
		_, err := h.begin(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerIdle, Target: "n1"})
		paused <- err
	}()
	time.Sleep(300 * time.Millisecond) // the begin waits for the row lock
	close(gate)
	a := <-acq
	if a.err != nil {
		t.Fatal(a.err)
	}
	wantErr(t, <-paused, model.ErrPrecondition, "")
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	a.release()
	e.admitHook = nil

	// The pause's begin holds the row while the admission waits: the
	// pause goes first, and the admission, which then finds the sandbox
	// pausing, waits for the op and resumes it before it holds it. A held
	// sandbox is never suspended under its exec.
	sb2 := h.running(512)
	h.idleSince(sb2.ID)
	gate2 := make(chan struct{})
	inBegin := make(chan struct{}, 1)
	e.beginHook = func(_ context.Context, id uuid.UUID, kind OpKind) {
		if id != sb2.ID || kind != OpPause {
			return
		}
		select {
		case inBegin <- struct{}{}:
		default:
		}
		<-gate2
	}
	go func() {
		_, err := h.begin(sb2.ID, OpIntent{Kind: OpPause, Trigger: TriggerIdle, Target: "n1"})
		paused <- err
	}()
	<-inBegin
	go func() {
		l, r, err := e.Acquire(bg, sb2.ID, model.HoldExec)
		acq <- acquired{l, r, err}
	}()
	time.Sleep(300 * time.Millisecond)
	e.beginHook = nil
	close(gate2)
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	a = <-acq
	if a.err != nil {
		t.Fatal(a.err)
	}
	defer a.release()
	h.podState("n1", sb2.ID, podv1.PodState_POD_STATE_RUNNING)
	row := h.row(sb2.ID)
	wantState(t, row, model.StateRunning)
	// The pause went first: the sandbox was paused, and resumed for the
	// admission.
	eventually(t, 10*time.Second, "the pause and the resume recorded", func() bool {
		var n int
		h.scalar(&n, `SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'lifecycle_end'
			AND detail->>'op' IN ('pause', 'resume')`, sb2.ID)
		return n == 2
	})
	if hs := h.holds(sb2.ID); len(hs) != 1 || hs[0].Kind != model.HoldExec {
		t.Fatalf("holds %+v", hs)
	}
}

func TestHoldCounting(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.idleSince(sb.ID)
	l1, r1, err := e.Acquire(bg, sb.ID, model.HoldExec)
	if err != nil {
		t.Fatal(err)
	}
	if l1.Node != "n1" || l1.Incarnation != sb.Incarnation {
		t.Fatalf("lease %+v", l1)
	}
	row := h.row(sb.ID)
	if time.Since(row.LastActiveAt) > time.Minute {
		t.Fatal("0 → 1 did not set last_active_at")
	}
	hs := h.holds(sb.ID)
	if len(hs) != 1 || hs[0].Holder != "i1:exec" || hs[0].InstanceID != "i1" || time.Until(hs[0].LeaseUntil) < 20*time.Second {
		t.Fatalf("holds %+v", hs)
	}
	_, r2, err := e.Acquire(bg, sb.ID, model.HoldExec)
	if err != nil {
		t.Fatal(err)
	}
	_, rf, err := e.Acquire(bg, sb.ID, model.HoldForward)
	if err != nil {
		t.Fatal(err)
	}
	if hs := h.holds(sb.ID); len(hs) != 2 {
		t.Fatalf("holds with exec and forward %+v", hs)
	}
	rf()
	rf() // a release is idempotent
	r1()
	if hs := h.holds(sb.ID); len(hs) != 1 || hs[0].Holder != "i1:exec" {
		t.Fatalf("holds with one exec left %+v", hs)
	}
	// The leader expired the hold meanwhile: the renewal puts it back.
	h.exec("DELETE FROM sandbox_holds WHERE sandbox_id = $1", sb.ID)
	e.renewHoldsOnce(bg)
	if hs := h.holds(sb.ID); len(hs) != 1 {
		t.Fatalf("holds after the renewal %+v", hs)
	}
	h.idleSince(sb.ID)
	r2()
	if hs := h.holds(sb.ID); len(hs) != 0 {
		t.Fatalf("holds after the last release %+v", hs)
	}
	if time.Since(h.row(sb.ID).LastActiveAt) > time.Minute {
		t.Fatal("1 → 0 did not set last_active_at")
	}
	e.holdsMu.Lock()
	left := len(e.holds)
	e.holdsMu.Unlock()
	if left != 0 {
		t.Fatalf("%d hold entries kept", left)
	}

	// Acquire of a sandbox that is gone.
	_, _, err = e.Acquire(bg, uuid.New(), model.HoldExec)
	wantErr(t, err, model.ErrNotFound, "")
}

// fakeLister lists processes as the test says.
type fakeLister struct {
	mu      sync.Mutex
	running map[string]bool
	err     error
	calls   int
}

func (f *fakeLister) RunningProcesses(context.Context, model.Lease) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	out := map[string]bool{}
	for k, v := range f.running {
		out[k] = v
	}
	return out, f.err
}

func TestProcessHolds(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.IdlePause = 10 * time.Millisecond })
	n := h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	if err := e.HoldProcess(bg, sb.ID, "forever", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.HoldProcess(bg, sb.ID, "short", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	hs := h.holds(sb.ID)
	if len(hs) != 2 || !hs[0].Infinite || hs[1].Infinite || time.Until(hs[1].LeaseUntil) < 20*time.Second {
		t.Fatalf("process holds %+v", hs)
	}
	// A timeout-0 process keeps the sandbox awake past the idle time.
	h.idleSince(sb.ID)
	if err := e.idlePause(bg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	wantState(t, h.row(sb.ID), model.StateRunning)

	// The processes end by themselves: the process-holds loop releases
	// their holds, and the idle loop pauses the sandbox.
	lister := &fakeLister{running: map[string]bool{"forever": false}}
	if err := e.processHolds(bg); err != nil {
		t.Fatal(err)
	}
	e.SetProcessLister(lister)
	if err := e.processHolds(bg); err != nil {
		t.Fatal(err)
	}
	if lister.calls != 1 {
		t.Fatalf("lister called %d times", lister.calls)
	}
	if hs := h.holds(sb.ID); len(hs) != 0 {
		t.Fatalf("holds after the processes ended %+v", hs)
	}
	if err := e.idlePause(bg); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StatePaused, 10*time.Second)

	// A stop of the process releases its hold; a transition that ends the
	// processes (a failure seen by Sync) releases them all.
	h.ensure(sb.ID)
	if err := e.HoldProcess(bg, sb.ID, "a", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.HoldProcess(bg, sb.ID, "b", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.ReleaseProcess(bg, sb.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if hs := h.holds(sb.ID); len(hs) != 1 {
		t.Fatalf("holds after a stop %+v", hs)
	}
	if err := n.Script().FailPod(sb.ID, "crashed"); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateFailed, 10*time.Second)
	if hs := h.holds(sb.ID); len(hs) != 0 {
		t.Fatalf("holds after a failure %+v", hs)
	}

	// A start is a cold boot: the processes held before it are gone.
	if err := e.HoldProcess(bg, sb.ID, "c", 0); err != nil {
		t.Fatal(err)
	}
	h.ensure(sb.ID)
	if hs := h.holds(sb.ID); len(hs) != 0 {
		t.Fatalf("holds after a cold boot %+v", hs)
	}

	// An explicit pause and its resume keep them (the memory is restored).
	if err := e.HoldProcess(bg, sb.ID, "d", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	h.ensure(sb.ID)
	if hs := h.holds(sb.ID); len(hs) != 1 {
		t.Fatalf("holds after a pause and resume %+v", hs)
	}
	// Expired holds go.
	h.exec("UPDATE sandbox_holds SET lease_until = now() - interval '1 second' WHERE sandbox_id = $1", sb.ID)
	if err := e.expiredHolds(bg); err != nil {
		t.Fatal(err)
	}
	if hs := h.holds(sb.ID); len(hs) != 0 {
		t.Fatalf("expired holds kept %+v", hs)
	}
	// A hold for a sandbox that is gone.
	err := e.HoldProcess(bg, uuid.New(), "x", 0)
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("hold of a missing sandbox: %v", err)
	}
}
