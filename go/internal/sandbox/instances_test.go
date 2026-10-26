// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"sync"
	"testing"
	"time"

	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/podid"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

func TestStopReleasesOps(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	arrived, release := h.gate("n1", podv1.PodService_SuspendPod_FullMethodName)
	defer release()
	hd, err := h.begin(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, arrived, "the pause's call")
	// §7 step 3: no new ops; the instance's own are waited for until the
	// deadline, then released (step 4).
	ctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	row := h.row(sb.ID)
	wantState(t, row, model.StateUnknown)
	if row.Op != "" || row.StatusDescription != "op pause of stopping instance i1 was released" {
		t.Fatalf("released row %+v", row)
	}
	other := h.sandbox(512)
	_, err = e.Ensure(bg, other.ID)
	wantErr(t, err, model.ErrStopping, "instance i1 is stopping")
	// The op in flight ends fenced out: it writes nothing.
	release()
	res, err := hd.Wait(bg)
	if err != nil || !res.fenced {
		t.Fatalf("the released op's end %+v %v", res, err)
	}
}

func TestTakeoverAcrossInstances(t *testing.T) {
	h := newHarness(t)
	i2 := h.addInstance("i2")
	h.addNode("n1")
	sb := h.running(512)
	h.settle(sb.ID)
	// i1's pause takes long at the node (which holds the pod's lock
	// meanwhile), then i1 stops (stopping from now on): i2 takes the pause
	// over at its next use, and resumes the sandbox after it.
	n := h.node("n1")
	arrived := make(chan struct{}, 1)
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, func(context.Context, string, any) error {
		select {
		case arrived <- struct{}{}:
		default:
		}
		return nil
	})
	n.Script().SetDelay(podv1.PodService_SuspendPod_FullMethodName, 3*time.Second)
	hd, err := h.begin(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("i1's pause call did not arrive")
	}
	if err := h.ins[0].hb.SetStopping(bg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	lease, err := i2.eng.Ensure(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Node != "n1" {
		t.Fatalf("lease %+v", lease)
	}
	res, err := hd.Wait(ctx)
	if err != nil || !res.fenced {
		t.Fatalf("the first owner's end %+v %v", res, err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
}

// holdBegin makes the first begin of kind for id on e wait in Start, right
// before its transaction, until release: another instance acts between
// the caller's read and its begin. entered is closed when it waits. It is
// set before the goroutines that begin run.
func holdBegin(e *Engine, id podid.ID, kind OpKind) (entered <-chan struct{}, release func()) {
	in := make(chan struct{})
	out := make(chan struct{})
	var first, once sync.Once
	e.preBeginHook = func(_ context.Context, bid podid.ID, k OpKind) {
		if bid != id || k != kind {
			return
		}
		held := false
		first.Do(func() {
			held = true
			close(in)
		})
		if held {
			<-out
		}
	}
	return in, func() { once.Do(func() { close(out) }) }
}

// TestEnsureRacesAnotherInstance: another instance's op begins and ends
// between an Ensure's read and its begin (a resume of a paused sandbox, a
// create of a pending one); the begin's failed precondition makes Ensure
// read the row again, and both return the lease.
func TestEnsureRacesAnotherInstance(t *testing.T) {
	h := newHarness(t)
	i2 := h.addInstance("i2")
	h.addNode("n1")
	e := h.eng()
	race := func(id podid.ID, kind OpKind) {
		t.Helper()
		entered, release := holdBegin(e, id, kind)
		defer release()
		ctx, cancel := context.WithTimeout(bg, 60*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := e.Ensure(ctx, id)
			done <- err
		}()
		waitClosed(t, entered, "i1's "+string(kind))
		if _, err := i2.eng.Ensure(ctx, id); err != nil {
			t.Fatalf("i2: %v", err)
		}
		release()
		if err := <-done; err != nil {
			t.Fatalf("i1's Ensure after i2's %s: %v", kind, err)
		}
		row := h.row(id)
		wantState(t, row, model.StateRunning)
		h.podState("n1", id, podv1.PodState_POD_STATE_RUNNING)
	}
	sb := h.running(512)
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	race(sb.ID, OpResume)
	race(h.sandbox(512).ID, OpCreate)
}

// TestPauseWaitsForAnotherInstancesOp: a REST pause whose begin finds
// another instance's op (begun after its read) waits for that op and looks
// again, instead of failing on the transitional state.
func TestPauseWaitsForAnotherInstancesOp(t *testing.T) {
	h := newHarness(t)
	i2 := h.addInstance("i2")
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	entered, releaseBegin := holdBegin(e, sb.ID, OpPause)
	defer releaseBegin()
	arrived, releaseCall := h.gate("n1", podv1.PodService_SuspendPod_FullMethodName)
	defer releaseCall()
	paused := make(chan error, 1)
	go func() { paused <- e.Pause(bg, sb.ID) }()
	waitClosed(t, entered, "i1's pause begin")
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if _, err := i2.eng.runner.Start(ctx, sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1",
		Incarnation: sb.Incarnation}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, arrived, "i2's pause call")
	releaseBegin()
	never(t, 300*time.Millisecond, "the pause returned during i2's op", func() bool { return len(paused) > 0 })
	releaseCall()
	if err := <-paused; err != nil {
		t.Fatalf("pause: %v", err)
	}
	wantState(t, h.waitState(sb.ID, model.StatePaused, 10*time.Second), model.StatePaused)
}

// TestStopWaitsForBegins: Stop waits for a begin in progress, so that the
// op it begins is waited for and released with the instance's others,
// instead of committing after the release.
func TestStopWaitsForBegins(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	inBegin := make(chan struct{})
	gate := make(chan struct{})
	var once, opened sync.Once
	open := func() { opened.Do(func() { close(gate) }) }
	defer open()
	e.beginHook = func(ctx context.Context, id podid.ID, kind OpKind) {
		if id != sb.ID || kind != OpPause {
			return
		}
		once.Do(func() { close(inBegin) })
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	arrived, release := h.gate("n1", podv1.PodService_SuspendPod_FullMethodName)
	defer release()
	begun := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(bg, 30*time.Second)
		defer cancel()
		_, err := e.runner.start(ctx, sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1",
			Incarnation: sb.Incarnation}, false)
		begun <- err
	}()
	waitClosed(t, inBegin, "the pause's begin")
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(bg, 2*time.Second)
		defer cancel()
		stopped <- e.Stop(ctx)
	}()
	never(t, 300*time.Millisecond, "Stop returned during a begin", func() bool { return len(stopped) > 0 })
	open()
	if err := <-begun; err != nil {
		t.Fatal(err)
	}
	waitClosed(t, arrived, "the pause's call")
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateUnknown)
	if row.Op != "" || row.StatusDescription != "op pause of stopping instance i1 was released" {
		t.Fatalf("row after Stop %+v", row)
	}
}

// TestEnsureMeetsAnotherInstancesHibernate: Ensure meeting a hibernate of
// another live instance waits for it within the hibernate wait only: one
// that does not end by then is ErrBusy at once and goes on on its owner,
// and a later Ensure restores the sandbox; one that ends within the wait
// is waited for, and the sandbox restored.
func TestEnsureMeetsAnotherInstancesHibernate(t *testing.T) {
	h := newHarness(t)
	i2 := h.addInstance("i2")
	h.addNode("n1")
	e := h.eng()
	e.t.hibernateWait = 300 * time.Millisecond
	sb := h.running(512)
	hibernateOnI2 := func() *OpHandle {
		t.Helper()
		h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
		ctx, cancel := context.WithTimeout(bg, 30*time.Second)
		defer cancel()
		hd, err := i2.eng.runner.start(ctx, sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1",
			Incarnation: sb.Incarnation}, false)
		if err != nil {
			t.Fatal(err)
		}
		return hd
	}

	arrived, release := h.gate("n1", podv1.PodService_HibernatePod_FullMethodName)
	defer release()
	hd := hibernateOnI2()
	waitClosed(t, arrived, "i2's hibernate call")
	_, took, err := ensureFor(e, sb.ID, time.Minute)
	wantHibernating(t, sb.ID, took, err)
	if row := h.row(sb.ID); row.ClientState != model.StateHibernating || row.OpOwner != "i2" {
		t.Fatalf("i2's hibernate did not go on: %+v", row)
	}
	release()
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if res, err := hd.Wait(ctx); err != nil || res.State != model.StateHibernated {
		t.Fatalf("i2's hibernate's end %+v %v", res, err)
	}
	if lease := h.ensure(sb.ID); lease.Node != "n1" {
		t.Fatalf("restored on %s", lease.Node)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)

	// Within the wait: i1 waits for i2's hibernate, then restores.
	e.t.hibernateWait = 20 * time.Second
	arrived, release2 := h.gate("n1", podv1.PodService_HibernatePod_FullMethodName)
	defer release2()
	hibernateOnI2()
	waitClosed(t, arrived, "i2's hibernate call")
	done := make(chan error, 1)
	go func() {
		_, _, err := ensureFor(e, sb.ID, time.Minute)
		done <- err
	}()
	never(t, 500*time.Millisecond, "Ensure returned during i2's hibernate", func() bool { return len(done) > 0 })
	release2()
	if err := <-done; err != nil {
		t.Fatalf("Ensure after i2's hibernate within the wait: %v", err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	h.placements(sb.ID, "i1", 3) // the create and two restores
}

// TestEnsurePreemptsAnotherInstancesHibernate: a hibernate of another live
// instance that a resume abandons now is preempted too: i1's Ensure
// resumes the sandbox on its node, and i2's hibernate ends fenced out and
// writes nothing.
func TestEnsurePreemptsAnotherInstancesHibernate(t *testing.T) {
	h := newHarness(t)
	i2 := h.addInstance("i2")
	h.addNode("n1")
	sb := h.pausedLong("n1")
	upload := h.watchCalls("n1", testnode.HibernateUpload, true)
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	hd, err := i2.eng.runner.start(ctx, sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerHibernateAfter, Target: "n1",
		Incarnation: sb.Incarnation}, false)
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, upload.arrived, "i2's upload")
	lease, took, err := ensureFor(h.eng(), sb.ID, time.Minute)
	if err != nil || lease.Node != "n1" {
		t.Fatalf("ensure: %+v %v", lease, err)
	}
	if took > h.eng().t.hibernateWait/3 {
		t.Fatalf("the preempt took %v", took)
	}
	h.activityNamed(sb.ID, "lifecycle_start", "resume on node n1 (abandons the hibernate of instance i2)")
	if res, err := hd.Wait(ctx); err != nil || !res.fenced {
		t.Fatalf("i2's hibernate's end %+v %v", res, err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if row.Op != "" || row.CheckpointHead != "" {
		t.Fatalf("i2's fenced finish wrote: %+v", row)
	}
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
}

// TestRolloutThenResumeAbandons: an instance stops while its hibernate's
// upload runs; its op is released (unknown), and the hibernation goes on
// at the node, whose dropped call no longer stops it. Ensure on another
// instance syncs the sandbox (paused: the pod is suspended), resumes it,
// and the node abandons the hibernation: the sandbox runs from its
// snapshot, and no hibernation is left.
func TestRolloutThenResumeAbandons(t *testing.T) {
	h := newHarness(t)
	i2 := h.addInstance("i2")
	h.addNode("n1")
	sb := h.pausedLong("n1")
	upload := h.watchCalls("n1", testnode.HibernateUpload, true)
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if _, err := i2.eng.runner.start(ctx, sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerHibernateAfter, Target: "n1",
		Incarnation: sb.Incarnation}, false); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, upload.arrived, "i2's upload")
	sctx, scancel := context.WithTimeout(bg, 200*time.Millisecond)
	if err := i2.eng.Stop(sctx); err != nil {
		t.Fatal(err)
	}
	scancel()
	row := h.row(sb.ID)
	wantState(t, row, model.StateUnknown)
	if row.StatusDescription != "op hibernate of stopping instance i2 was released" {
		t.Fatalf("released row %+v", row)
	}
	i2.stop()
	h.wantHibernation("n1", sb.ID, podv1.HibernationPhase_HIBERNATION_PHASE_UPLOADING_DISK, true)
	if lease := h.ensure(sb.ID); lease.Node != "n1" {
		t.Fatalf("lease %+v", lease)
	}
	row = h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if row.CheckpointHead != "" {
		t.Fatalf("a checkpoint after an abandoned hibernation: %+v", row)
	}
	if origin := h.runOrigin(sb.ID); origin != "snapshot" {
		t.Fatalf("run origin %q", origin)
	}
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	if info.GetHibernation() != nil {
		t.Fatalf("a hibernation left: %v", info.GetHibernation())
	}
	never(t, 300*time.Millisecond, "a second hibernation", func() bool { return upload.count() > 1 })
}
