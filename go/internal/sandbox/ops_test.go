// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// startOp begins an op on the first instance and waits for its end.
func (h *harness) startOp(id uuid.UUID, in OpIntent) OpResult {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	if in.Incarnation == uuid.Nil {
		in.Incarnation = h.row(id).Incarnation
	}
	hd, err := h.eng().runner.Start(ctx, id, in)
	if err != nil {
		h.t.Fatalf("start %s: %v", in.Kind, err)
	}
	res, err := hd.Wait(ctx)
	if err != nil {
		h.t.Fatalf("wait for %s: %v", in.Kind, err)
	}
	return res
}

func TestOpKinds(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.running(1024)
	if sb.LastActiveAt.IsZero() || sb.AvoidNodes == nil || len(sb.AvoidNodes) != 0 {
		t.Fatalf("after create %+v", sb)
	}

	// pause: SuspendPod; the run closes, the booking goes.
	res := h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	if res.State != model.StatePaused || res.Err != nil || res.Node != "n1" {
		t.Fatalf("pause: %+v", res)
	}
	row := h.row(sb.ID)
	if row.RunID != "" || row.PausedAt.IsZero() || row.Op != "" || row.OpID != uuid.Nil {
		t.Fatalf("paused row %+v", row)
	}
	if _, ok := h.booking(sb.ID); ok {
		t.Fatal("a paused sandbox is booked")
	}
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_SUSPENDED)

	// resume through Ensure: ResumePod, booked again, a new run.
	h.ensure(sb.ID)
	row = h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if row.RunID == "" {
		t.Fatal("no run after the resume")
	}
	if b, ok := h.booking(sb.ID); !ok || !b.Gated || b.Node != "n1" {
		t.Fatalf("booking after the resume %+v %v", b, ok)
	}

	// hibernate of a running sandbox is the drain's only: refused here.
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().runner.Start(ctx, sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1",
		Incarnation: row.Incarnation})
	wantErr(t, err, model.ErrPrecondition, "is running")

	// pause, then hibernate: a checkpoint of the node's object store.
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	res = h.startOp(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	if res.State != model.StateHibernated || res.Node != "n1" {
		t.Fatalf("hibernate: %+v", res)
	}
	row = h.row(sb.ID)
	if row.CheckpointHead == "" || row.CheckpointEpoch == 0 || !row.CheckpointMemory || row.CheckpointStore != h.objs.ID() ||
		row.CheckpointAt.IsZero() {
		t.Fatalf("hibernated row %+v", row)
	}
	if _, ok := h.booking(sb.ID); ok {
		t.Fatal("a hibernated sandbox is booked")
	}

	// restore through Ensure, on the node that holds the record.
	h.ensure(sb.ID)
	row = h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if row.Node != "n1" {
		t.Fatalf("restored on %q", row.Node)
	}

	// start: the pod failed; a Sync sees it, Ensure starts it on its disk.
	if err := h.node("n1").Script().FailPod(sb.ID, "the microVM crashed"); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateFailed, 10*time.Second)
	if d := h.row(sb.ID).StatusDescription; d != "the microVM crashed" {
		t.Fatalf("failed description %q", d)
	}
	h.ensure(sb.ID)
	wantState(t, h.row(sb.ID), model.StateRunning)

	// stopped: a StopPod outside ferund, then Ensure starts it again.
	if _, err := h.pods("n1").StopPod(bg, &podv1.StopPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateStopped, 10*time.Second)
	h.ensure(sb.ID)
	wantState(t, h.row(sb.ID), model.StateRunning)
}

func TestOutcomePendingAtDeadline(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	release := make(chan struct{})
	n.Script().SetBringUp(func(ctx context.Context, _ uuid.UUID) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	h.eng().t.opDeadline = func(OpKind, int) time.Duration { return 1500 * time.Millisecond }
	sb := h.sandbox(512)
	res := h.startOp(sb.ID, OpIntent{Kind: OpCreate, Trigger: TriggerEnsure, Candidates: []string{"n1"}, ImageRef: testImage})
	if res.State != model.StateCreating || !strings.HasPrefix(res.Description, "still starting on node n1 after") {
		t.Fatalf("create at the deadline: %+v", res)
	}
	row := h.row(sb.ID)
	if !row.Materialised || row.PendingSince.IsZero() || row.Op != "" {
		t.Fatalf("row %+v", row)
	}
	if b, ok := h.booking(sb.ID); !ok || b.Node != "n1" {
		t.Fatal("a creating sandbox is not booked")
	}
	// The pod comes up: Ensure waits for it (creating without an op) and
	// syncs.
	close(release)
	h.ensure(sb.ID)
	row = h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if !row.PendingSince.IsZero() {
		t.Fatal("pending_since kept after the pod ran")
	}
}

func TestPendingSinceAtFirstObservation(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	release := make(chan struct{})
	n.Script().SetBringUp(func(ctx context.Context, _ uuid.UUID) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	sb := h.sandbox(512)
	done := make(chan error, 1)
	go func() {
		_, err := h.eng().Ensure(bg, sb.ID)
		done <- err
	}()
	// While the create's op still waits for the pod, the waiter recorded
	// the first PENDING observation (a write of its own, no gen bump).
	eventually(t, 10*time.Second, "pending_since set during the op", func() bool {
		row := h.row(sb.ID)
		return row.Op == string(OpCreate) && !row.PendingSince.IsZero()
	})
	row := h.row(sb.ID)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after := h.row(sb.ID)
	if after.Gen <= row.Gen {
		t.Fatalf("gen %d then %d", row.Gen, after.Gen)
	}
	wantState(t, after, model.StateRunning)
}

func TestOutcomeFailedEnsureReturns(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	n.Script().SetBringUp(func(context.Context, uuid.UUID) error { return errors.New("image pull failed") })
	sb := h.sandbox(512)
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, nil, "create on node n1 failed: image pull failed")
	row := h.row(sb.ID)
	wantState(t, row, model.StateFailed)
	if row.StatusDescription != "image pull failed" || row.RunID != "" {
		t.Fatalf("failed row %+v", row)
	}
	// Ensure did not go on with a start after the failed create.
	var starts int
	h.scalar(&starts, "SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'lifecycle_start' AND detail->>'op' = 'start'", sb.ID)
	if starts != 0 {
		t.Fatalf("%d starts after a failed create", starts)
	}
	if _, ok := h.booking(sb.ID); ok {
		t.Fatal("a failed sandbox is booked")
	}
}

func TestOutcomePauseAndHibernateFailures(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	sb := h.running(512)

	// A pause whose SuspendPod fails leaves the pod running.
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.Internal, "snapshot failed")
	})
	res := h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	if res.State != model.StateRunning || res.Description != "pause failed: snapshot failed" {
		t.Fatalf("failed pause: %+v", res)
	}
	if b, ok := h.booking(sb.ID); !ok || b.Node != "n1" {
		t.Fatal("a running sandbox lost its booking")
	}
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, nil)

	// A hibernation that fails leaves the pod suspended.
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	n.Script().SetHook(podv1.PodService_HibernatePod_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.Unavailable, "upload failed")
	})
	res = h.startOp(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	if res.State != model.StatePaused || res.Description != "hibernation failed: upload failed" {
		t.Fatalf("failed hibernate: %+v", res)
	}
	if h.row(sb.ID).CheckpointHead != "" {
		t.Fatal("a failed hibernation wrote a checkpoint")
	}
}

func TestOutcomeStaleRecord(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.sandbox(512)
	// A pod of this ID from an earlier incarnation is still on the node.
	old := uuid.New()
	stale := podSpec(store.Sandbox{ID: sb.ID, Owner: testTenant, Incarnation: old, VCPUs: 1, MemoryMiB: 512, ImageRef: testImage})
	if _, err := h.pods("n1").CreatePod(bg, &podv1.CreatePodRequest{PodId: sb.ID.String(), Spec: stale}); err != nil {
		t.Fatal(err)
	}
	// Hold the stale record's deletion, so that the tombstone is seen.
	deletes := make(chan struct{})
	h.node("n1").Script().SetHook(podv1.PodService_DeletePod_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		select {
		case <-deletes:
		case <-ctx.Done():
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, nil, "found a stale pod of incarnation "+old.String())
	row := h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if row.Node != "" || len(row.AvoidNodes) != 0 {
		t.Fatalf("pending row %+v", row)
	}
	// The stale record got a tombstone of its own incarnation, which keeps
	// the pod's ID off the node until it is processed.
	ts := h.tombstones(sb.ID)
	if len(ts) != 1 || ts[0].Incarnation != old || ts[0].Node != "n1" {
		t.Fatalf("tombstones %+v", ts)
	}
	p, err := h.eng().place(bg, row, OpCreate)
	if err != nil || len(p.candidates) != 0 || len(p.tombstoneOnly) != 1 {
		t.Fatalf("placement with the tombstone %+v %v", p, err)
	}
	close(deletes)
	// The next Ensure waits for (or has) the tombstone processed, then
	// creates.
	h.ensure(sb.ID)
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	if podIncarnation(info) != sb.Incarnation {
		t.Fatal("the stale pod survived")
	}
	if len(h.tombstones(sb.ID)) != 0 {
		t.Fatal("the tombstone stayed")
	}
}

func TestOutcomeTAPCollisionPlacesElsewhere(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	h.addNode("n2")
	// Binpack prefers the fuller node: book something on n1 so that it is
	// the first candidate.
	filler := h.running(4096)
	if filler.Node != "n1" {
		h.t.Skipf("filler placed on %s", filler.Node)
	}
	sb := h.sandbox(512)
	// A pod that ferund does not know holds the TAP prefix on n1.
	other, _ := uuid.Parse(sb.ID.String()[:8] + "-0000-4000-8000-000000000000")
	if other == sb.ID {
		t.Skip("collision with itself")
	}
	if _, err := h.pods("n1").CreatePod(bg, &podv1.CreatePodRequest{PodId: other.String(), Spec: &podv1.PodSpec{
		Resources:  &podv1.Resources{Vcpus: 1, MemoryMib: 256},
		Containers: []*podv1.ContainerSpec{{Name: "main", ImageRef: testImage}},
	}}); err != nil {
		t.Fatal(err)
	}
	lease := h.ensure(sb.ID)
	if lease.Node != "n2" {
		t.Fatalf("placed on %s", lease.Node)
	}
	row := h.row(sb.ID)
	if len(row.AvoidNodes) != 0 {
		t.Fatalf("avoid_nodes after a run %v", row.AvoidNodes)
	}
	var avoided int
	h.scalar(&avoided, "SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'sandbox_state' AND detail->>'description' LIKE '%placed elsewhere on the next use'", sb.ID)
	if avoided == 0 {
		t.Fatal("no pending state with the avoidance recorded")
	}
}

func TestOutcomeNotFound(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	// A materialised sandbox whose pod is gone: lost.
	sb := h.running(512)
	// The pod goes while the pause is under way.
	vanish := func(id uuid.UUID) func(context.Context, string, any) error {
		return func(context.Context, string, any) error {
			_, err := h.pods("n1").DeletePod(bg, &podv1.DeletePodRequest{PodId: id.String()})
			return err
		}
	}
	h.node("n1").Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, vanish(sb.ID))
	res := h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.node("n1").Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, nil)
	if res.State != model.StateLost || res.Description != "not on node n1" || res.Node != "n1" {
		t.Fatalf("pause of a gone pod: %+v", res)
	}
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	if !errors.Is(err, model.ErrLost) || err.Error() != "sandbox lost" {
		t.Fatalf("ensure of a lost sandbox: %v", err)
	}

	// One with a checkpoint: hibernated without a node, then restored.
	sb2 := h.running(512)
	h.startOp(sb2.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.startOp(sb2.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	h.ensure(sb2.ID)
	h.startOp(sb2.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.node("n1").Script().SetHook(podv1.PodService_ResumePod_FullMethodName, vanish(sb2.ID))
	res = h.startOp(sb2.ID, OpIntent{Kind: OpResume, Trigger: TriggerEnsure, Target: "n1"})
	h.node("n1").Script().SetHook(podv1.PodService_ResumePod_FullMethodName, nil)
	if res.State != model.StateHibernated || res.Node != "" {
		t.Fatalf("resume of a gone pod with a checkpoint: %+v", res)
	}
	h.ensure(sb2.ID)
	wantState(t, h.row(sb2.ID), model.StateRunning)

	// A create whose call fails before a pod exists with an error of the
	// pod's (not the node's): pending, the node not avoided.
	sb3 := h.sandbox(512)
	h.exec("UPDATE sandboxes SET image_ref = 'not a valid ref' WHERE id = $1", sb3.ID)
	h.node("n1").Script().SetHook(podv1.PodService_CreatePod_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.InvalidArgument, "bad image")
	})
	res = h.startOp(sb3.ID, OpIntent{Kind: OpCreate, Trigger: TriggerEnsure, Candidates: []string{"n1"}})
	if res.State != model.StatePending || res.Description != "create on node n1 failed: bad image" {
		t.Fatalf("failed create: %+v", res)
	}
	if row := h.row(sb3.ID); row.Node != "" || len(row.AvoidNodes) != 0 || row.Materialised {
		t.Fatalf("row after a failed create %+v", row)
	}
	if _, ok := h.booking(sb3.ID); ok {
		t.Fatal("a pending sandbox is booked")
	}
}

func TestOutcomeUnknown(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	e.t.opDeadline = func(OpKind, int) time.Duration { return time.Second }
	e.t.settleGrace = 300 * time.Millisecond
	block := func(ctx context.Context, _ string, _ any) error {
		<-ctx.Done()
		return status.FromContextError(ctx.Err()).Err()
	}
	sb := h.sandbox(512)
	n.Script().SetHook(podv1.PodService_CreatePod_FullMethodName, block)
	n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, block)
	res := h.startOp(sb.ID, OpIntent{Kind: OpCreate, Trigger: TriggerEnsure, Candidates: []string{"n1"}, ImageRef: testImage})
	if res.State != model.StateUnknown || !strings.HasPrefix(res.Description, "create on node n1: outcome unknown (") {
		t.Fatalf("create without an answer: %+v", res)
	}
	row := h.row(sb.ID)
	if row.Node != "n1" {
		t.Fatalf("unknown row lost its node: %+v", row)
	}
	if b, ok := h.booking(sb.ID); !ok || b.Node != "n1" {
		t.Fatal("the booking of an unknown sandbox changed")
	}
	// The node answers again: Sync resolves it (no pod was made).
	n.Script().SetHook(podv1.PodService_CreatePod_FullMethodName, nil)
	n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, nil)
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	row = h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if row.Node != "" {
		t.Fatalf("pending with a node %+v", row)
	}
	e.t.opDeadline = OpDeadline
	h.ensure(sb.ID)
}

func TestOpDeadlines(t *testing.T) {
	cases := []struct {
		kind OpKind
		mem  int
		want time.Duration
	}{
		{OpCreate, 512, 7*time.Minute + 5*time.Second},
		{OpStart, 65536, 7*time.Minute + 5*time.Second},
		{OpResume, 1025, 2*time.Minute + 4*time.Second},
		{OpRestore, 2048, 7*time.Minute + 5*time.Second + 16*time.Second},
		{OpPause, 512, 2*time.Minute + 4*time.Second},
		{OpHibernate, 65536, 15*time.Minute + 64*16*time.Second},
		{OpRepair, 1024, 7*time.Minute + 5*time.Second + 8*time.Second},
	}
	for _, c := range cases {
		if got := OpDeadline(c.kind, c.mem); got != c.want {
			t.Errorf("%s of %d MiB: %v, want %v", c.kind, c.mem, got, c.want)
		}
	}
}

func podReq(id uuid.UUID) *podv1.GetPodRequest { return &podv1.GetPodRequest{PodId: id.String()} }

// TestCreateFailsWithInternal: a create whose call fails with INTERNAL
// (an error that may pass) does not keep the sandbox off its node, and
// Ensure's error names the op once.
func TestCreateFailsWithInternal(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	sb := h.sandbox(512)
	var failed atomic.Bool
	n.Script().SetHook(podv1.PodService_CreatePod_FullMethodName, func(context.Context, string, any) error {
		if failed.Swap(true) {
			return nil
		}
		return status.Error(codes.Internal, "diskd is busy")
	})
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	if err == nil || err.Error() != "create on node n1 failed: diskd is busy" {
		t.Fatalf("ensure after a failed create: %v", err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if len(row.AvoidNodes) != 0 {
		t.Fatalf("avoid_nodes after INTERNAL %v", row.AvoidNodes)
	}
	// The next use creates it on the same node.
	if lease := h.ensure(sb.ID); lease.Node != "n1" {
		t.Fatalf("lease %+v", lease)
	}
}
