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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// errVMBusy is feosd's answer while Firecracker still writes a snapshot
// that a suspend gave up on, marked as feosd marks it.
var errVMBusy = busyStatus("the VM is busy: Firecracker still writes the snapshot that " +
	"the last suspend of VM x gave up on; the VM is as that suspend found it once it is done")

// busyStatus is an UNAVAILABLE with msg and feosd's busy marker.
func busyStatus(msg string) error {
	p := status.New(codes.Unavailable, msg).Proto()
	p.Details = append(p.Details, &anypb.Any{TypeUrl: vmBusyDetail})
	return status.ErrorProto(p)
}

// failWith makes every call of method on node fail with err until the
// test sets another hook.
func (h *harness) failWith(node, method string, err error) {
	h.node(node).Script().SetHook(method, func(context.Context, string, any) error { return err })
}

// wantBusy checks that err is ErrBusy with a retry time of busyRetry, and
// says the VM is busy.
func wantBusy(t *testing.T, err error) {
	t.Helper()
	var later interface{ RetryAfter() time.Duration }
	if !errors.Is(err, model.ErrBusy) || !errors.As(err, &later) || later.RetryAfter() != busyRetry ||
		!strings.Contains(err.Error(), "the VM is busy") {
		t.Fatalf("got error %v, want ErrBusy after %v saying the VM is busy", err, busyRetry)
	}
}

// wantPostponed checks that the sandbox is in state, without an op, and
// that its description says why the op is postponed.
func wantPostponed(t *testing.T, sb store.Sandbox, state, op string) {
	t.Helper()
	wantState(t, sb, state)
	if sb.Op != "" || !strings.HasPrefix(sb.StatusDescription, op+" postponed: the VM is busy: Firecracker") {
		t.Fatalf("op %q, description %q, want %q postponed for a busy VM", sb.Op, sb.StatusDescription, op)
	}
}

// TestBusyVM: a node call that feosd answers UNAVAILABLE "the VM is busy",
// marked with its status detail, changed nothing and is tried again later: the sandbox says so ("<op>
// postponed: the VM is busy: …"), a policy backs off as after any failure,
// and a caller gets ErrBusy with a retry time. Any other UNAVAILABLE is a
// failure, as before, whatever its message says.
func TestBusyVM(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.IdlePause = 50 * time.Millisecond
		c.HibernateAfter = time.Minute
	})
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	suspend := podv1.PodService_SuspendPod_FullMethodName

	// The idle pause: postponed, and backed off.
	h.failWith("n1", suspend, errVMBusy)
	time.Sleep(100 * time.Millisecond)
	if err := e.idlePause(bg); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the pause ended", func() bool { return h.row(sb.ID).Op == "" })
	row := h.row(sb.ID)
	wantPostponed(t, row, model.StateRunning, "pause")
	if row.PolicyFailed != store.PolicyIdle || row.PolicyFailures != 1 || row.PolicyNotBefore.Sub(row.UpdatedAt) != 30*time.Second {
		t.Fatalf("backoff %q %d %v", row.PolicyFailed, row.PolicyFailures, row.PolicyNotBefore.Sub(row.UpdatedAt))
	}
	// The explicit pause: ErrBusy, to be asked again.
	wantBusy(t, e.Pause(bg, sb.ID))
	wantPostponed(t, h.row(sb.ID), model.StateRunning, "pause")
	// Another UNAVAILABLE is a failure, and so is one that says the VM is
	// busy without feosd's marker.
	h.failWith("n1", suspend, status.Error(codes.Unavailable, "diskd does not answer"))
	err := e.Pause(bg, sb.ID)
	if err == nil || errors.Is(err, model.ErrBusy) || !strings.Contains(err.Error(), "pause failed: diskd does not answer") {
		t.Fatalf("pause: %v", err)
	}
	h.failWith("n1", suspend, status.Error(codes.Unavailable, "the VM is busy: or so it says"))
	err = e.Pause(bg, sb.ID)
	if err == nil || errors.Is(err, model.ErrBusy) || !strings.Contains(err.Error(), "pause failed: the VM is busy: or so it says") {
		t.Fatalf("pause: %v", err)
	}
	// Free again, it pauses.
	h.node("n1").Script().SetHook(suspend, nil)
	if err := e.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(sb.ID), model.StatePaused)

	// The hibernate-after: postponed, paused, and backed off.
	h.failWith("n1", podv1.PodService_HibernatePod_FullMethodName, errVMBusy)
	h.exec("UPDATE sandboxes SET paused_at = now() - interval '2 minutes' WHERE id = $1", sb.ID)
	if err := e.hibernateAfter(bg); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the hibernate ended", func() bool { return h.row(sb.ID).Op == "" })
	row = h.row(sb.ID)
	wantPostponed(t, row, model.StatePaused, "hibernation")
	if row.PolicyFailed != store.PolicyHibernateAfter || row.PolicyFailures != 1 {
		t.Fatalf("backoff %q %d", row.PolicyFailed, row.PolicyFailures)
	}

	// A repair whose StopPod meets a busy VM leaves the pod running, and
	// its processes with their holds: it starts nothing.
	h.node("n1").Script().SetHook(podv1.PodService_HibernatePod_FullMethodName, nil)
	h.ensure(sb.ID)
	if err := e.HoldProcess(bg, sb.ID, "job", 0); err != nil {
		t.Fatal(err)
	}
	h.failWith("n1", podv1.PodService_StopPod_FullMethodName, errVMBusy)
	var starts atomic.Int32
	h.node("n1").Script().SetHook(podv1.PodService_StartPod_FullMethodName, func(context.Context, string, any) error {
		starts.Add(1)
		return nil
	})
	res := h.startOp(sb.ID, OpIntent{Kind: OpRepair, Trigger: TriggerRestart, Target: "n1"})
	if res.State != model.StateRunning {
		t.Fatalf("repair: %+v", res)
	}
	wantPostponed(t, h.row(sb.ID), model.StateRunning, "repair")
	if hs := h.holds(sb.ID); len(hs) != 1 {
		t.Fatalf("process holds after a postponed repair %+v", hs)
	}
	if n := starts.Load(); n != 0 {
		t.Fatalf("a postponed repair called StartPod %d times", n)
	}
	h.node("n1").Script().SetHook(podv1.PodService_StartPod_FullMethodName, nil)
	if err := e.ReleaseProcess(bg, sb.ID, "job"); err != nil {
		t.Fatal(err)
	}

	// Ensure of a stopped sandbox on a draining node, whose hibernation
	// meets a busy VM: ErrBusy, and the sandbox stays.
	h.node("n1").Script().SetHook(podv1.PodService_StopPod_FullMethodName, nil)
	if _, err := h.pods("n1").StopPod(bg, &podv1.StopPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateStopped, 10*time.Second)
	h.failWith("n1", podv1.PodService_HibernatePod_FullMethodName, errVMBusy)
	h.exec("UPDATE nodes SET draining = true WHERE name = 'n1'")
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err = e.Ensure(ctx, sb.ID)
	wantBusy(t, err)
	wantPostponed(t, h.row(sb.ID), model.StateStopped, "hibernation")
}

// TestSuspendedAfterThePause: a SuspendPod that feosd finishes after the
// pause gave up on it leaves the sandbox running ("pause failed"); the
// pod's event then syncs it paused, paused from now: not since an earlier
// pause, which would hibernate it at once.
func TestSuspendedAfterThePause(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.HibernateAfter = time.Minute })
	n := h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	// Paused and resumed an hour ago.
	if res := h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"}); res.State != model.StatePaused {
		t.Fatalf("pause: %+v", res)
	}
	h.ensure(sb.ID)
	h.exec("UPDATE sandboxes SET paused_at = now() - interval '1 hour' WHERE id = $1", sb.ID)
	h.settle(sb.ID)

	// The node writes the snapshot past the pause's deadline.
	e.t.opDeadline = func(OpKind, int) time.Duration { return time.Second }
	n.Script().SetDelay(podv1.PodService_SuspendPod_FullMethodName, 3*time.Second)
	res := h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	e.t.opDeadline = OpDeadline
	if res.State != model.StateRunning || !strings.HasPrefix(res.Description, "pause failed") {
		t.Fatalf("pause past its deadline: %+v", res)
	}
	n.Script().SetDelay(podv1.PodService_SuspendPod_FullMethodName, 0)
	row := h.waitState(sb.ID, model.StatePaused, 15*time.Second)
	if row.StatusDescription != "" {
		t.Fatalf("description %q", row.StatusDescription)
	}
	if age := row.UpdatedAt.Sub(row.PausedAt); age < 0 || age > time.Second {
		t.Fatalf("paused at %v, updated at %v", row.PausedAt, row.UpdatedAt)
	}
	// Not paused for longer than the hibernate-after: no hibernation.
	if err := e.hibernateAfter(bg); err != nil {
		t.Fatal(err)
	}
	if r := h.row(sb.ID); r.Op != "" || r.ClientState != model.StatePaused {
		t.Fatalf("the hibernate-after began %q (%s)", r.Op, r.ClientState)
	}
}
