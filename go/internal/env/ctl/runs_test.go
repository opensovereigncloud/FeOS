// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// runSpec is a run of the test image with a deadline of d.
func runSpec(d time.Duration) RunSpec {
	return RunSpec{Image: testImage, Command: []string{"/build"}, VCPUs: 1, MemoryMiB: 512, Deadline: d, Hash: "h"}
}

// createRun creates a run of the tenant and returns it.
func (h *harness) createRun(rs RunSpec) store.Run {
	h.t.Helper()
	r, created, err := h.c.CreateRun(bg, testTenant, rs)
	if err != nil || !created {
		h.t.Fatalf("create run: %v (created %v)", err, created)
	}
	return r
}

// waitRun waits until the run is in phase, and returns it.
func (h *harness) waitRun(id podid.ID, phase string, within time.Duration) store.Run {
	h.t.Helper()
	var r store.Run
	eventually(h.t, within, "run "+id.String()+" "+phase, func() bool {
		var err error
		r, err = h.st.GetRun(bg, testTenant, id)
		return err == nil && r.Phase == phase
	})
	return r
}

// runningPod waits for a run's pod to run on n1 with its container, and
// returns the pod's spec.
func (h *harness) runningPod(id podid.ID) {
	h.t.Helper()
	eventually(h.t, 30*time.Second, "the run's pod runs", func() bool {
		sb, err := h.st.GetSandbox(bg, id)
		if err != nil || !steady(sb) {
			return false
		}
		p := h.pod("n1", id)
		return p != nil && len(p.Containers) == 1
	})
}

// podGone waits until a run's pod's row is gone.
func (h *harness) podGone(id podid.ID) {
	h.t.Helper()
	eventually(h.t, 30*time.Second, "the run's pod is deleted", func() bool {
		_, err := h.st.GetSandbox(bg, id)
		return errors.Is(err, model.ErrNotFound)
	})
}

// A run's pod is a row the tenant does not see, built from the run's
// template with its secrets and never idle; its exit 0 ends it succeeded
// with its result file and its log's tail, redacted of its secrets, and
// its pod goes. A create with its key and body answers with it; with
// another body it is refused.
func TestRunSucceeds(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	const token = "s3cr3t-token-value"
	rs := runSpec(time.Minute)
	rs.Env = map[string]string{"COMMIT": "abc123"}
	rs.Secrets = map[string]string{"GIT_TOKEN": token}
	rs.ImageCredentials = &model.RegistryCredentials{Username: "build", Password: "registry-password"}
	rs.ResultPath = "/run/fecode-build/digest"
	rs.Key, rs.Labels = "build-1", map[string]string{"project": "p1"}
	run := h.createRun(rs)
	if run.Phase != model.RunQueued || run.ImageRef != testImage || run.Ended() ||
		run.DeadlineAt.Sub(run.CreatedAt) != time.Minute {
		t.Fatalf("created %+v", run)
	}
	again, created, err := h.c.CreateRun(bg, testTenant, rs)
	if err != nil || created || again.ID != run.ID {
		t.Fatalf("the repeat: %+v %v %v", again, created, err)
	}
	other := rs
	other.Hash = "another body"
	if _, _, err := h.c.CreateRun(bg, testTenant, other); !errors.Is(err, ErrRunKeyConflict) {
		t.Fatalf("another body under the key: %v", err)
	}
	if list, err := h.st.ListSandboxes(bg, testTenant); err != nil || len(list) != 0 {
		t.Fatalf("the tenant sees %d sandboxes (%v)", len(list), err)
	}

	h.runningPod(run.ID)
	sb, _ := h.st.GetSandbox(bg, run.ID)
	if !sb.TenantRun || !sb.IsInstance() || sb.IdleAfterS == nil || *sb.IdleAfterS != 0 {
		t.Fatalf("the pod's row %+v", sb)
	}
	spec := h.pod("n1", run.ID).GetSpec().GetContainers()[0]
	if spec.GetEnv()["GIT_TOKEN"] != token || spec.GetEnv()["COMMIT"] != "abc123" ||
		!slices.Equal(spec.GetCommand(), []string{"/build"}) {
		t.Fatalf("the pod's container %+v", spec)
	}
	h.waitRun(run.ID, model.RunRunning, 10*time.Second)

	dir := filepath.Join(n1.PodDir(run.ID), "run", "fecode-build")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "digest"), []byte("sha256:0123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"cloning with " + token, "pushed"} {
		if err := n1.Script().AppendLog(run.ID, "main", l); err != nil {
			t.Fatal(err)
		}
	}
	if err := n1.Script().ExitContainer(run.ID, "main", 0); err != nil {
		t.Fatal(err)
	}
	done := h.waitRun(run.ID, model.RunSucceeded, 30*time.Second)
	if done.ExitCode == nil || *done.ExitCode != 0 || done.Reason != "" || done.Result != "sha256:0123\n" ||
		!slices.Equal(done.LogTail, []string{"cloning with [redacted]", "pushed"}) || done.FinishedAt.IsZero() ||
		done.StartedAt.IsZero() {
		t.Fatalf("ended %+v", done)
	}
	h.podGone(run.ID)
	if sealed, err := h.st.RunSealed(bg, run.ID); err != nil || sealed != nil {
		t.Errorf("the ended run keeps its secrets: %v %v", sealed, err)
	}
	if lines, err := h.c.RunLog(bg, testTenant, run.ID, 1); err != nil || !slices.Equal(lines, []string{"pushed"}) {
		t.Errorf("the ended run's log %v %v", lines, err)
	}
	// Cancelling a run that ended changes nothing.
	if r, err := h.c.CancelRun(bg, testTenant, run.ID); err != nil || r.Phase != model.RunSucceeded {
		t.Errorf("cancel after the end: %+v %v", r, err)
	}
	if err := h.c.DeleteRun(bg, testTenant, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetRun(bg, testTenant, run.ID); !errors.Is(err, store.ErrNoRow) {
		t.Errorf("the deleted run stays: %v", err)
	}
}

// Another exit code fails the run (exit); a missing result file is its
// detail.
func TestRunFails(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	rs := runSpec(time.Minute)
	rs.ResultPath = "/run/none"
	run := h.createRun(rs)
	h.runningPod(run.ID)
	if err := n1.Script().AppendLog(run.ID, "main", "error: boom"); err != nil {
		t.Fatal(err)
	}
	if err := n1.Script().ExitContainer(run.ID, "main", 3); err != nil {
		t.Fatal(err)
	}
	done := h.waitRun(run.ID, model.RunFailed, 30*time.Second)
	if done.Reason != model.RunReasonExit || done.ExitCode == nil || *done.ExitCode != 3 ||
		!slices.Equal(done.LogTail, []string{"error: boom"}) || !strings.HasPrefix(done.Detail, "its result /run/none") {
		t.Fatalf("ended %+v", done)
	}
	h.podGone(run.ID)
}

// A run whose deadline passes ends deadline_exceeded, its tail kept.
func TestRunDeadline(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	run := h.createRun(runSpec(3 * time.Second))
	h.runningPod(run.ID)
	if err := n1.Script().AppendLog(run.ID, "main", "still building"); err != nil {
		t.Fatal(err)
	}
	done := h.waitRun(run.ID, model.RunDeadlineExceeded, 30*time.Second)
	if done.ExitCode != nil || done.Detail != "its deadline of 3s passed" ||
		!slices.Equal(done.LogTail, []string{"still building"}) {
		t.Fatalf("ended %+v", done)
	}
	h.podGone(run.ID)
}

// A cancel ends a running run cancelled, its tail kept and its pod
// deleted; a delete of a running run cancels it and removes it.
func TestRunCancel(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	run := h.createRun(runSpec(time.Minute))
	h.runningPod(run.ID)
	if err := n1.Script().AppendLog(run.ID, "main", "step 1"); err != nil {
		t.Fatal(err)
	}
	r, err := h.c.CancelRun(bg, testTenant, run.ID)
	if err != nil || r.Phase != model.RunCancelled || !slices.Equal(r.LogTail, []string{"step 1"}) {
		t.Fatalf("cancelled %+v %v", r, err)
	}
	h.podGone(run.ID)

	second := h.createRun(runSpec(time.Minute))
	h.runningPod(second.ID)
	if err := h.c.DeleteRun(bg, testTenant, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetRun(bg, testTenant, second.ID); !errors.Is(err, store.ErrNoRow) {
		t.Errorf("the deleted run stays: %v", err)
	}
	h.podGone(second.ID)
	if _, err := h.c.CancelRun(bg, testTenant, second.ID); !errors.Is(err, ErrNoObject) {
		t.Errorf("cancel of a deleted run: %v", err)
	}
}

// A run whose pod is lost with its node fails node_lost and is not run
// again.
func TestRunNodeLost(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	run := h.createRun(runSpec(time.Minute))
	h.runningPod(run.ID)
	if err := n1.Script().ForgetPod(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.Sync(bg, run.ID); err != nil { // finds it lost, sooner than the engine's loops
		t.Fatal(err)
	}
	done := h.waitRun(run.ID, model.RunFailed, 30*time.Second)
	if done.Reason != model.RunReasonNodeLost || done.Detail != "its pod was lost with node n1" {
		t.Fatalf("ended %+v", done)
	}
	h.podGone(run.ID)
	if p := h.pod("n1", run.ID); p != nil {
		t.Error("the lost run runs again")
	}
}

// The sweep removes the runs that ended RunRetention ago, and only those.
func TestRunSweep(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	old := h.createRun(runSpec(time.Minute))
	fresh := h.createRun(runSpec(time.Minute))
	for _, id := range []podid.ID{old.ID, fresh.ID} {
		if _, err := h.c.CancelRun(bg, testTenant, id); err != nil {
			t.Fatal(err)
		}
	}
	h.exec("UPDATE runs SET finished_at = now() - interval '8 days' WHERE id = $1", old.ID)
	if err := h.c.sweepRuns(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetRun(bg, testTenant, old.ID); !errors.Is(err, store.ErrNoRow) {
		t.Errorf("the old run stays: %v", err)
	}
	if _, err := h.st.GetRun(bg, testTenant, fresh.ID); err != nil {
		t.Errorf("the fresh run went: %v", err)
	}
}

// A run whose image its registry refuses to the pod's create fails
// (image).
func TestRunImageRefused(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	h.eng.SetImageAccess(func(context.Context, string, model.RegistryCredentials) error {
		return errors.New("401: denied")
	})
	run := h.createRun(runSpec(time.Minute))
	done := h.waitRun(run.ID, model.RunFailed, 30*time.Second)
	if done.Reason != model.RunReasonImage || !strings.Contains(done.Detail, "denied") {
		t.Fatalf("ended %+v", done)
	}
	h.podGone(run.ID)
}
