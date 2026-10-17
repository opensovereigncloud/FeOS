// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

func TestSyncAppliesObservations(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)

	// unknown → whatever the pod is.
	h.exec("UPDATE sandboxes SET client_state = 'unknown', status_description = 'node n1 down', gen = gen + 1 WHERE id = $1", sb.ID)
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if row.StatusDescription != "" {
		t.Fatalf("description kept %q", row.StatusDescription)
	}

	// SUSPENDED outside an op → paused, paused_at set, booking gone.
	if _, err := h.pods("n1").SuspendPod(bg, &podv1.SuspendPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StatePaused, 10*time.Second)
	if row := h.row(sb.ID); row.PausedAt.IsZero() || row.RunID != "" {
		t.Fatalf("paused by Sync %+v", row)
	}
	if _, ok := h.booking(sb.ID); ok {
		t.Fatal("paused but booked")
	}

	// The record of a hibernated sandbox is gone from its holder: node
	// NULL, still hibernated.
	h.startOp(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	h.settle(sb.ID)
	n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, nil)
	h.exec("UPDATE sandboxes SET status_description = 'kept' WHERE id = $1", sb.ID)
	if _, err := h.pods("n1").DeletePod(bg, &podv1.DeletePodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the record holder's loss applied", func() bool { return h.row(sb.ID).Node == "" })
	row = h.row(sb.ID)
	wantState(t, row, model.StateHibernated)
	if row.StatusDescription != "kept" || row.CheckpointHead == "" {
		t.Fatalf("hibernated row %+v", row)
	}
	// A Sync of a row without a node has nothing to ask.
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	// A Sync of a sandbox that is gone does nothing.
	if err := e.Sync(bg, uuid.New()); err != nil {
		t.Fatal(err)
	}
}

func TestSyncDeferredBehindOwnOp(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	var gets atomic.Int64
	n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, func(context.Context, string, any) error {
		gets.Add(1)
		return nil
	})
	in := make(chan struct{})
	out := make(chan struct{})
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, func(context.Context, string, any) error {
		close(in)
		<-out
		return nil
	})
	hd, err := h.begin(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, in, "the pause's call")
	// A Sync while the op runs is deferred to the op's finish: it asks
	// the node nothing now.
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 0 {
		t.Fatal("a deferred Sync asked the node")
	}
	close(out)
	if _, err := hd.Wait(bg); err != nil {
		t.Fatal(err)
	}
	// After the finish: one GetPod of the op's settle, then the Sync's.
	eventually(t, 10*time.Second, "the deferred Sync ran", func() bool { return gets.Load() >= 2 })
}

func TestSyncDeferredBehindOtherInstancesOp(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	h.liveInstance("other")
	row := h.opOfDeadOwner(sb, OpPause, "other")
	if err := n.Script().FailPod(sb.ID, "crashed"); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	never(t, 500*time.Millisecond, "a Sync wrote under another instance's op", func() bool {
		return h.row(sb.ID).ClientState != model.StatePausing
	})
	e.mu.Lock()
	_, deferred := e.deferred[sb.ID]
	e.mu.Unlock()
	if !deferred {
		t.Fatal("the Sync is not deferred")
	}
	// The other instance's op ends (it writes running): the notification
	// makes this instance sync the sandbox, which finds it failed.
	h.exec(`UPDATE sandboxes SET op = '', op_id = NULL, op_owner = '', op_deadline = NULL, op_target = '', op_from_state = '',
		client_state = 'running', gen = gen + 1 WHERE id = $1 AND op_id = $2`, sb.ID, row.OpID)
	h.exec("SELECT pg_notify('ferun_sandbox', $1)", sb.ID.String())
	h.waitState(sb.ID, model.StateFailed, 10*time.Second)
}

func TestSyncWithoutConnectionAsksForReconcile(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	h.exec("INSERT INTO nodes (name, status) VALUES ('far', 'ready')")
	h.exec("UPDATE sandboxes SET node = 'far', gen = gen + 1 WHERE id = $1", sb.ID)
	ch, unsubscribe := h.d.Subscribe(db.ChanReconcile)
	defer unsubscribe()
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-ch:
		if p != "far" {
			t.Fatalf("ferun_reconcile %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ferun_reconcile")
	}
	// At most once per 5 s per node from this instance.
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-ch:
		if p == "far" {
			t.Fatal("a second ferun_reconcile within 5 s")
		}
	case <-time.After(300 * time.Millisecond):
	}
	if row := h.row(sb.ID); row.ClientState != model.StateRunning || row.Node != "far" {
		t.Fatalf("a Sync without a connection wrote: %+v", row)
	}
}

func TestEnsureWaitsForPendingPod(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1", func(c *testnode.Config) { c.PendingDelay = 20 * time.Millisecond })
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	// The pod restarts outside ferund and takes its time: PENDING without
	// an op; Ensure waits for it and syncs.
	release := make(chan struct{})
	n.Script().SetBringUp(func(ctx context.Context, _ uuid.UUID) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	if _, err := h.pods("n1").StopPod(bg, &podv1.StopPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pods("n1").StartPod(bg, &podv1.StartPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateCreating, 10*time.Second)
	if h.row(sb.ID).PendingSince.IsZero() {
		t.Fatal("observed PENDING without pending_since")
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.Ensure(bg, sb.ID)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Ensure returned while the pod was PENDING: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if !row.PendingSince.IsZero() {
		t.Fatal("pending_since kept")
	}

	// WaitRunning does the same for a caller whose guest call found the
	// pod still starting.
	release2 := make(chan struct{})
	n.Script().SetBringUp(func(ctx context.Context, _ uuid.UUID) error {
		select {
		case <-release2:
		case <-ctx.Done():
		}
		return nil
	})
	if _, err := h.pods("n1").StopPod(bg, &podv1.StopPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pods("n1").StartPod(bg, &podv1.StartPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(release2)
	}()
	lease, err := e.WaitRunning(bg, sb.ID)
	if err != nil || lease.Node != "n1" {
		t.Fatalf("WaitRunning %+v %v", lease, err)
	}
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
}

func TestSyncRetriesOnceWhenTheRowChanged(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	// Every GetPod of the Sync meets a row that changed since its read.
	var gets atomic.Int64
	bump := func(limit int64) {
		n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, func(context.Context, string, any) error {
			if gets.Add(1) <= limit {
				h.exec("UPDATE sandboxes SET gen = gen + 1 WHERE id = $1", sb.ID)
			}
			return nil
		})
	}
	h.exec("UPDATE sandboxes SET client_state = 'unknown', gen = gen + 1 WHERE id = $1", sb.ID)
	bump(1)
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 2 {
		t.Fatalf("%d observations", gets.Load())
	}
	wantState(t, h.row(sb.ID), model.StateRunning)

	// Changed at both reads: it gives up, a later event or sweep catches
	// up.
	gets.Store(0)
	h.exec("UPDATE sandboxes SET client_state = 'unknown', gen = gen + 1 WHERE id = $1", sb.ID)
	bump(10)
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 2 {
		t.Fatalf("%d observations", gets.Load())
	}
	wantState(t, h.row(sb.ID), model.StateUnknown)
	n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, nil)
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)
}

// TestSyncMergesTheCheckpointOfARunningPod: a pod that runs on a checkpoint
// newer than its row's (restored by a resume that waited for a hibernation
// past its point of no return) gives the row that checkpoint.
func TestSyncMergesTheCheckpointOfARunningPod(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.hibernatedOn("n1")
	h.ensure(sb.ID)
	h.settle(sb.ID)
	cp := h.row(sb.ID)
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	if info.GetCheckpoint().GetHeadKey() != cp.CheckpointHead || cp.CheckpointHead == "" {
		t.Fatalf("the restored pod's checkpoint %v, the row's %q", info.GetCheckpoint(), cp.CheckpointHead)
	}
	h.exec(`UPDATE sandboxes SET checkpoint_head = 'older', checkpoint_epoch = checkpoint_epoch - 1, checkpoint_store = '',
		gen = gen + 1 WHERE id = $1`, sb.ID)
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if row.CheckpointHead != cp.CheckpointHead || row.CheckpointEpoch != cp.CheckpointEpoch || row.CheckpointStore != h.objs.ID() {
		t.Fatalf("row after the Sync %+v, want the checkpoint of %+v", row, cp)
	}
}
