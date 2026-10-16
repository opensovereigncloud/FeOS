// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

func TestEnsureCreates(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.sandbox(512)
	lease := h.ensure(sb.ID)
	if lease.Node != "n1" || lease.SandboxID != sb.ID || lease.Incarnation != sb.Incarnation {
		t.Fatalf("lease %+v", lease)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if !row.Materialised || row.ImageRef != testImage || row.RunID == "" || row.Node != "n1" {
		t.Fatalf("row after create %+v", row)
	}
	b, ok := h.booking(sb.ID)
	if !ok || b.Node != "n1" || !b.Gated {
		t.Fatalf("booking %+v %v", b, ok)
	}
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	if podIncarnation(info) != sb.Incarnation || info.GetSpec().GetLabels()[LabelTenant] != testTenant {
		t.Fatalf("pod labels %v", info.GetSpec().GetLabels())
	}
	// Running: Ensure returns at once with the same lease.
	if again := h.ensure(sb.ID); again != lease {
		t.Fatalf("lease %+v then %+v", lease, again)
	}
}

func TestEnsureErrorsByState(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	ensure := func(id uuid.UUID) error {
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		_, err := e.Ensure(ctx, id)
		return err
	}
	// Unknown sandbox.
	wantErr(t, ensure(uuid.New()), model.ErrNotFound, "")

	// A stopped sandbox whose node is down.
	sb := h.running(512)
	h.settle(sb.ID)
	h.exec("UPDATE sandboxes SET client_state = 'stopped', gen = gen + 1 WHERE id = $1", sb.ID)
	h.exec("UPDATE nodes SET status = 'down' WHERE name = 'n1'")
	wantErr(t, ensure(sb.ID), model.ErrNodeUnavailable, "node n1 is down")
	h.exec("UPDATE nodes SET status = 'ready' WHERE name = 'n1'")

	// An unknown sandbox whose node is not connected.
	n.Partition(true)
	eventually(t, 10*time.Second, "n1 disconnected", func() bool { return !h.ins[0].srv.Connected("n1") })
	h.exec("UPDATE sandboxes SET client_state = 'unknown', status_description = 'node n1 down since then', gen = gen + 1 WHERE id = $1", sb.ID)
	wantErr(t, ensure(sb.ID), model.ErrNodeUnavailable, "node n1 is unavailable: node n1 down since then")
	// A running one too.
	h.exec("UPDATE sandboxes SET client_state = 'running', gen = gen + 1 WHERE id = $1", sb.ID)
	wantErr(t, ensure(sb.ID), model.ErrNodeUnavailable, "node n1 is not connected to instance i1")
	n.Partition(false)
	eventually(t, 10*time.Second, "n1 back", func() bool { return h.ins[0].srv.Connected("n1") })
	h.ensure(sb.ID)
}

func TestEnsureRestorePlacesOnceMore(t *testing.T) {
	h := newHarness(t)
	n1 := h.addNode("n1")
	h.addNode("n2")
	sb := h.hibernatedOn("n1")
	h.settle(sb.ID)
	// The restore on the record holder finds the record gone and fails:
	// hibernated without a node, and Ensure places once more elsewhere.
	n1.Script().SetHook(podv1.PodService_RestorePod_FullMethodName, func(context.Context, string, any) error {
		_, err := h.pods("n1").DeletePod(bg, &podv1.DeletePodRequest{PodId: sb.ID.String()})
		if err != nil {
			return err
		}
		return status.Error(codes.NotFound, "no pod")
	})
	lease := h.ensure(sb.ID)
	if lease.Node != "n2" {
		t.Fatalf("restored on %s", lease.Node)
	}
	var restores int
	h.scalar(&restores, "SELECT count(*) FROM placements WHERE pod_id = $1", sb.ID)
	if restores != 3 { // the create, the restore on n1, the one on n2
		t.Fatalf("%d placements", restores)
	}
}

func TestPauseAndResumeREST(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	if err := e.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(sb.ID), model.StatePaused)
	// Nothing to do for paused, hibernated and pending.
	if err := e.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Pause(bg, h.sandbox(512).ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)
	// A failed sandbox cannot be paused.
	if err := n.Script().FailPod(sb.ID, "crashed"); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateFailed, 10*time.Second)
	wantErr(t, e.Pause(bg, sb.ID), nil, "sandbox is failed")
	// A pause whose SuspendPod fails is an error.
	h.ensure(sb.ID)
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.Internal, "snapshot failed")
	})
	wantErr(t, e.Pause(bg, sb.ID), nil, "pause on node n1 failed: pause failed: snapshot failed")
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, nil)
	// A pause that takes longer than the wait goes on.
	e.t.pauseWait = time.Second
	n.Script().SetDelay(podv1.PodService_SuspendPod_FullMethodName, 3*time.Second)
	if err := e.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StatePaused, 10*time.Second)
	if _, _, err := e.State(bg, uuid.New()); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("state of an unknown sandbox: %v", err)
	}
	if st, node, err := e.State(bg, sb.ID); err != nil || st != model.StatePaused || node != "n1" {
		t.Fatalf("state %q %q %v", st, node, err)
	}
}

func TestEnsureResumeRefusedForCapacity(t *testing.T) {
	t.Run("object store", testResumeRefusedHibernates)
	t.Run("no object store", testResumeRefused)
}

func testResumeRefusedHibernates(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.running(512)
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	// n1 is full now: the resume is refused under the node's lock; with an
	// object store there the sandbox is hibernated and restored on another
	// node.
	h.exec("UPDATE nodes SET slots = 0 WHERE name = 'n1'")
	h.addNode("n2")
	lease := h.ensure(sb.ID)
	if lease.Node != "n2" {
		t.Fatalf("resumed on %s", lease.Node)
	}
	row := h.row(sb.ID)
	if row.CheckpointHead == "" {
		t.Fatal("not hibernated on the way")
	}

}

// testResumeRefused: without an object store, the refusal is the caller's.
func testResumeRefused(t *testing.T) {
	h2 := newHarness(t)
	h2.addNode("bare", func(c *testnode.Config) { c.ObjectStore = nil })
	sb2 := h2.running(512)
	h2.startOp(sb2.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "bare"})
	h2.exec("UPDATE nodes SET slots = 0 WHERE name = 'bare'")
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	_, err := h2.eng().Ensure(ctx, sb2.ID)
	wantErr(t, err, model.ErrNoCapacity, "no capacity on node bare for sandbox "+sb2.ID.String()+" (retryable): no slots")
	if row := h2.row(sb2.ID); row.ClientState != model.StatePaused || row.Op != "" {
		t.Fatalf("row after a refused resume %+v", row)
	}
	if _, ok := h2.booking(sb2.ID); ok {
		t.Fatal("a refused admission booked")
	}
}

// TestRecreateWhileOldOpRuns: a sandbox deleted and created again with the
// same ID while its old incarnation's create is slow on this instance: the
// new row's Ensure begins an op of its own at once instead of waiting for
// the old one, and the old pod goes when its op ends.
func TestRecreateWhileOldOpRuns(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	h.addNode("n2")
	e := h.eng()
	id := uuid.New()
	old := h.sandboxWithID(id, 512)
	arrived := make(chan string, 2)
	held := make(chan struct{})
	hold := func(node string) testnode.Hook {
		return func(ctx context.Context, _ string, req any) error {
			r, ok := req.(*podv1.CreatePodRequest)
			if !ok || r.GetSpec().GetLabels()[LabelIncarnation] != old.Incarnation.String() {
				return nil
			}
			arrived <- node
			select {
			case <-held:
			case <-ctx.Done():
			}
			return nil
		}
	}
	for _, name := range []string{"n1", "n2"} {
		h.node(name).Script().SetHook(podv1.PodService_CreatePod_FullMethodName, hold(name))
	}
	first := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(bg, 60*time.Second)
		defer cancel()
		_, err := e.Ensure(ctx, id)
		first <- err
	}()
	var oldNode string
	select {
	case oldNode = <-arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("the old incarnation's create did not arrive")
	}
	if _, _, err := e.Delete(bg, testTenant, id); err != nil {
		t.Fatal(err)
	}
	sb := h.sandboxWithID(id, 512)
	ctx, cancel := context.WithTimeout(bg, 20*time.Second)
	defer cancel()
	lease, err := e.Ensure(ctx, id)
	close(held)
	if err != nil {
		t.Fatalf("ensure of the new incarnation: %v", err)
	}
	if lease.Incarnation != sb.Incarnation || lease.Node == oldNode {
		t.Fatalf("lease %+v (the old create went to %s)", lease, oldNode)
	}
	<-first
	// The old op ends fenced out; its pod goes through its tombstone.
	h.noPod(oldNode, id)
	if info := h.podState(lease.Node, id, podv1.PodState_POD_STATE_RUNNING); podIncarnation(info) != sb.Incarnation {
		t.Fatal("the new pod is not the new incarnation's")
	}
}
