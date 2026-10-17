// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

// The preempting resume: Ensure meeting a hibernate whose node says that a
// resume abandons it now begins a resume in its place (§5 of the resume
// design). The basic case is TestEnsurePreemptsHibernate; the cases of
// other owners are in instances_test.go and takeover_test.go.

// abandoned is feosd's answer to the HibernatePod of a hibernation a
// resume abandoned.
func abandoned(id uuid.UUID) string {
	return "pod " + id.String() + " is resumed: its hibernation was abandoned"
}

// TestEnsureKeepsDrainHibernates: a drain's hibernate, and Ensure's off a
// draining node, carry no_abandon: no ResumePod reaches their node, also
// when the node no longer drains, and Ensure answers that the sandbox is
// hibernating after its wait.
func TestEnsureKeepsDrainHibernates(t *testing.T) {
	t.Run("drain", func(t *testing.T) {
		h := newHarness(t)
		h.addNode("n1")
		e := h.eng()
		e.t.hibernateWait = time.Second
		sb := h.pausedLong("n1")
		resumes := h.watchCalls("n1", podv1.PodService_ResumePod_FullMethodName, false)
		upload := h.watchCalls("n1", testnode.HibernateUpload, true)
		hd, err := h.begin(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerDrain, Target: "n1"})
		if err != nil {
			t.Fatal(err)
		}
		waitClosed(t, upload.arrived, "the drain's upload")
		reqs := upload.hibernateRequests()
		if len(reqs) != 1 || !reqs[0].GetNoAbandon() || reqs[0].GetRequestId() != hd.opID.String() {
			t.Fatalf("the drain's request %v", reqs)
		}
		h.wantHibernation("n1", sb.ID, podv1.HibernationPhase_HIBERNATION_PHASE_UPLOADING_DISK, false, hd.opID.String())
		_, took, err := ensureFor(e, sb.ID, time.Minute)
		wantHibernating(t, sb.ID, took, err)
		if resumes.count() != 0 {
			t.Fatal("a ResumePod reached the node during a drain's hibernation")
		}
		upload.release()
		ctx, cancel := context.WithTimeout(bg, 30*time.Second)
		defer cancel()
		if res, err := hd.Wait(ctx); err != nil || res.State != model.StateHibernated {
			t.Fatalf("the drain's hibernate %+v %v", res, err)
		}
	})
	t.Run("ensure off a draining node", func(t *testing.T) {
		h := newHarness(t)
		h.addNode("n1")
		e := h.eng()
		e.t.hibernateWait = time.Second
		sb := h.pausedLong("n1")
		resumes := h.watchCalls("n1", podv1.PodService_ResumePod_FullMethodName, false)
		upload := h.watchCalls("n1", testnode.HibernateUpload, true)
		h.exec("UPDATE nodes SET draining = true WHERE name = 'n1'")
		_, took, err := ensureFor(e, sb.ID, time.Minute)
		wantHibernating(t, sb.ID, took, err)
		waitClosed(t, upload.arrived, "Ensure's upload")
		row := h.row(sb.ID)
		reqs := upload.hibernateRequests()
		if len(reqs) != 1 || !reqs[0].GetNoAbandon() || reqs[0].GetRequestId() != row.OpID.String() {
			t.Fatalf("Ensure's request off a draining node %v (op %s)", reqs, row.OpID)
		}
		// The drain ends: the hibernation stays one a resume does not
		// abandon.
		h.exec("UPDATE nodes SET draining = false WHERE name = 'n1'")
		h.wantHibernation("n1", sb.ID, podv1.HibernationPhase_HIBERNATION_PHASE_UPLOADING_DISK, false, row.OpID.String())
		_, took, err = ensureFor(e, sb.ID, time.Minute)
		wantHibernating(t, sb.ID, took, err)
		if resumes.count() != 0 {
			t.Fatal("a ResumePod reached the node during a drain's hibernation")
		}
		upload.release()
		h.waitState(sb.ID, model.StateHibernated, 15*time.Second)
	})
}

// TestEnsurePreemptsACapacityHibernate: a hibernate that Ensure began
// because the node had no room for the resume carries no no_abandon: once
// a slot frees, another Ensure preempts it, and both get a lease on the
// node; the first one's hibernate ends fenced out.
func TestEnsurePreemptsACapacityHibernate(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.pausedLong("n1")
	upload := h.watchCalls("n1", testnode.HibernateUpload, true)
	h.exec("UPDATE nodes SET slots = 0 WHERE name = 'n1'")
	type answer struct {
		lease model.Lease
		err   error
	}
	first := make(chan answer, 1)
	go func() {
		lease, _, err := ensureFor(e, sb.ID, time.Minute)
		first <- answer{lease, err}
	}()
	waitClosed(t, upload.arrived, "the capacity hibernate's upload")
	row := h.row(sb.ID)
	reqs := upload.hibernateRequests()
	if len(reqs) != 1 || reqs[0].GetNoAbandon() || reqs[0].GetRequestId() != row.OpID.String() {
		t.Fatalf("the capacity hibernate's request %v (op %s)", reqs, row.OpID)
	}
	h.exec("UPDATE nodes SET slots = 50 WHERE name = 'n1'")
	if lease := h.ensure(sb.ID); lease.Node != "n1" {
		t.Fatalf("the second Ensure's lease %+v", lease)
	}
	select {
	case a := <-first:
		if a.err != nil || a.lease.Node != "n1" {
			t.Fatalf("the first Ensure: %+v %v", a.lease, a.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the first Ensure did not return")
	}
	h.activityNamed(sb.ID, "lifecycle_end", "hibernate on node n1: abandoned")
	wantState(t, h.row(sb.ID), model.StateRunning)
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
}

// TestEnsurePreemptWaitsForItsCall: a hibernation that an earlier call
// left on the node is not the row's op's: while the op's own call has not
// reached it, Ensure does not preempt and asks the node about once per
// preemptRecheck; once the call joins the hibernation, Ensure preempts,
// the abandon covers both calls, and no second hibernation runs.
func TestEnsurePreemptWaitsForItsCall(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	e.t.preemptRecheck = 200 * time.Millisecond
	sb := h.pausedLong("n1")
	upload := h.watchCalls("n1", testnode.HibernateUpload, true)
	// An older hibernation of the pod, as a call that was cut leaves it.
	pc := h.pods("n1")
	older := make(chan error, 1)
	go func() {
		_, err := pc.HibernatePod(bg, &podv1.HibernatePodRequest{PodId: sb.ID.String(), RequestId: "older"})
		older <- err
	}()
	waitClosed(t, upload.arrived, "the older hibernation's upload")
	// A hibernate-after op whose call is held before it reaches the node.
	call := h.watchCalls("n1", podv1.PodService_HibernatePod_FullMethodName, true)
	hd, err := h.begin(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerHibernateAfter, Target: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, call.arrived, "the op's call")
	resumes := h.watchCalls("n1", podv1.PodService_ResumePod_FullMethodName, false)
	var gets atomic.Int64
	n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, func(context.Context, string, any) error {
		gets.Add(1)
		return nil
	})
	done := make(chan error, 1)
	go func() {
		lease, _, err := ensureFor(e, sb.ID, time.Minute)
		if err == nil && lease.Node != "n1" {
			t.Errorf("lease %+v", lease)
		}
		done <- err
	}()
	const held = 1500 * time.Millisecond
	never(t, held, "a ResumePod while the op's call had not joined", func() bool { return resumes.count() > 0 || len(done) > 0 })
	if g := gets.Load(); g < 3 || g > int64(held/e.t.preemptRecheck)+4 {
		t.Fatalf("%d GetPods in %v", g, held)
	}
	h.wantHibernation("n1", sb.ID, podv1.HibernationPhase_HIBERNATION_PHASE_UPLOADING_DISK, true, "older")
	call.release()
	if err := <-done; err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := <-older; status.Code(err) != codes.Aborted || status.Convert(err).Message() != abandoned(sb.ID) {
		t.Fatalf("the older call's answer: %v", err)
	}
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if res, err := hd.Wait(ctx); err != nil || !res.fenced {
		t.Fatalf("the op's end %+v %v", res, err)
	}
	never(t, 500*time.Millisecond, "a second hibernation", func() bool { return upload.count() > 1 })
	wantState(t, h.row(sb.ID), model.StateRunning)
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	if resumes.count() != 1 {
		t.Fatalf("%d ResumePods", resumes.count())
	}
}

// TestEnsurePreemptGetPodFails: a GetPod of the preempt that fails makes
// Ensure look again within preemptRecheck, not after the whole wait.
func TestEnsurePreemptGetPodFails(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	sb, hd, _ := h.hibernatingAtUpload("n1")
	var failed atomic.Bool
	n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, func(context.Context, string, any) error {
		if failed.Swap(true) {
			return nil
		}
		return status.Error(codes.Unavailable, "the node is busy (injected)")
	})
	lease, took, err := ensureFor(e, sb.ID, time.Minute)
	if err != nil || lease.Node != "n1" {
		t.Fatalf("ensure: %+v %v", lease, err)
	}
	if !failed.Load() {
		t.Fatal("no GetPod failed")
	}
	if took > e.t.hibernateWait/3 {
		t.Fatalf("the preempt took %v", took)
	}
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if res, err := hd.Wait(ctx); err != nil || !res.fenced {
		t.Fatalf("the preempted hibernate's end %+v %v", res, err)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)
}

// TestEnsurePreemptRefused: a node that refuses the preempting resume (no
// slot), or drains, keeps the hibernate: Ensure begins at most one resume
// per hibernate and call, answers that the sandbox is hibernating after
// its wait, and the hibernation goes on to its end.
func TestEnsurePreemptRefused(t *testing.T) {
	for _, c := range []struct {
		name, sql  string
		begins     int64
		looksAtPod bool
	}{
		{"slots", "UPDATE nodes SET slots = 0 WHERE name = 'n1'", 1, true},
		{"draining", "UPDATE nodes SET draining = true WHERE name = 'n1'", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			n := h.addNode("n1")
			e := h.eng()
			e.t.hibernateWait = 1500 * time.Millisecond
			e.t.preemptRecheck = 100 * time.Millisecond
			sb, hd, upload := h.hibernatingAtUpload("n1")
			var begins, gets atomic.Int64
			e.preBeginHook = func(_ context.Context, id uuid.UUID, kind OpKind) {
				if id == sb.ID && kind == OpResume {
					begins.Add(1)
				}
			}
			n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, func(context.Context, string, any) error {
				gets.Add(1)
				return nil
			})
			resumes := h.watchCalls("n1", podv1.PodService_ResumePod_FullMethodName, false)
			h.exec(c.sql)
			_, took, err := ensureFor(e, sb.ID, time.Minute)
			wantHibernating(t, sb.ID, took, err)
			if begins.Load() != c.begins || resumes.count() != 0 {
				t.Fatalf("%d preempting begins, %d ResumePods", begins.Load(), resumes.count())
			}
			if (gets.Load() > 0) != c.looksAtPod {
				t.Fatalf("%d GetPods", gets.Load())
			}
			if row := h.row(sb.ID); row.Op != string(OpHibernate) || row.OpID != hd.opID || row.ClientState != model.StateHibernating {
				t.Fatalf("the hibernate did not go on: %+v", row)
			}
			if _, ok := h.booking(sb.ID); ok {
				t.Fatal("a refused preempt booked")
			}
			upload.release()
			ctx, cancel := context.WithTimeout(bg, 30*time.Second)
			defer cancel()
			if res, err := hd.Wait(ctx); err != nil || res.State != model.StateHibernated {
				t.Fatalf("the hibernate's end %+v %v", res, err)
			}
		})
	}
}

// TestEnsureOldNodeWaits: a node whose feosd does not report its
// hibernations (ResumeWaitsForHibernation) is never preempted: Ensure asks
// it at most about once per preemptRecheck during its wait, then answers
// that the sandbox is hibernating, as before; a later Ensure restores it.
func TestEnsureOldNodeWaits(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1", func(c *testnode.Config) { c.ResumeWaitsForHibernation = true })
	e := h.eng()
	e.t.hibernateWait = 2 * time.Second
	e.t.preemptRecheck = 200 * time.Millisecond
	sb, hd, upload := h.hibernatingAtUpload("n1")
	if hb := h.hibernation("n1", sb.ID); hb != nil {
		t.Fatalf("an old node reports a hibernation %v", hb)
	}
	var gets atomic.Int64
	n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, func(context.Context, string, any) error {
		gets.Add(1)
		return nil
	})
	resumes := h.watchCalls("n1", podv1.PodService_ResumePod_FullMethodName, false)
	_, took, err := ensureFor(e, sb.ID, time.Minute)
	wantHibernating(t, sb.ID, took, err)
	if g := gets.Load(); g < 2 || g > int64(e.t.hibernateWait/e.t.preemptRecheck)+4 {
		t.Fatalf("%d GetPods during a wait of %v", g, e.t.hibernateWait)
	}
	if resumes.count() != 0 {
		t.Fatal("a ResumePod reached an old node")
	}
	upload.release()
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if res, err := hd.Wait(ctx); err != nil || res.State != model.StateHibernated {
		t.Fatalf("the hibernate's end %+v %v", res, err)
	}
	if lease := h.ensure(sb.ID); lease.Node != "n1" {
		t.Fatalf("restored on %s", lease.Node)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)
}

// TestEnsurePreemptTooLate: the hibernation passes its point of no return
// between the preempt's GetPod and its ResumePod. The ResumePod waits for
// it and restores the pod from its new checkpoint: the sandbox runs, the
// row has the checkpoint (merged by the resume's finish or the fenced
// hibernate's), and the run is restored from it.
func TestEnsurePreemptTooLate(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb, hd, upload := h.hibernatingAtUpload("n1")
	head := h.watchCalls("n1", testnode.HibernateHead, true)
	resumes := h.watchCalls("n1", podv1.PodService_ResumePod_FullMethodName, false)
	var once sync.Once
	e.preBeginHook = func(_ context.Context, id uuid.UUID, kind OpKind) {
		if id != sb.ID || kind != OpResume {
			return
		}
		once.Do(func() {
			upload.release()
			select {
			case <-head.arrived:
			case <-time.After(15 * time.Second):
				t.Error("the hibernation did not reach its head")
			}
		})
	}
	// Once the ResumePod has come, and the node shows the hibernation past
	// its point of no return, its head is put.
	pc := h.pods("n1")
	go func() {
		defer head.release()
		select {
		case <-resumes.arrived:
		case <-time.After(30 * time.Second):
			t.Error("no ResumePod")
			return
		}
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			info, err := pc.GetPod(bg, &podv1.GetPodRequest{PodId: sb.ID.String()})
			if hb := info.GetHibernation(); err == nil && hb.GetPhase() == podv1.HibernationPhase_HIBERNATION_PHASE_HEAD &&
				!hb.GetAbandonable() {
				return
			}
		}
		t.Error("the node did not show the hibernation past its point of no return")
	}()
	lease, _, err := ensureFor(e, sb.ID, time.Minute)
	if err != nil || lease.Node != "n1" {
		t.Fatalf("ensure: %+v %v", lease, err)
	}
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	res, err := hd.Wait(ctx)
	if err != nil || !res.fenced {
		t.Fatalf("the hibernate's end %+v %v", res, err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	cp := info.GetCheckpoint()
	if cp.GetHeadKey() == "" || row.CheckpointHead != cp.GetHeadKey() || row.CheckpointEpoch != int64(cp.GetEpoch()) ||
		row.CheckpointStore != h.objs.ID() {
		t.Fatalf("row %+v, the pod's checkpoint %v", row, cp)
	}
	if origin := h.runOrigin(sb.ID); origin != "checkpoint" {
		t.Fatalf("run origin %q", origin)
	}
}
