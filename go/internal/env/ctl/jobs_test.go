// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

// jobSpec is a spec with a release job of command (and a service that
// waits for it).
func jobSpec(command string, retries int) string {
	return fmt.Sprintf(`{"jobs":{"migrate":{"image":%q,"command":["/app","migrate",%q],"retries":%d}},
		"services":{"api":{"image":%q,"command":["/app","serve"]}}}`, testImage, command, retries, testImage)
}

// finishRun waits for the job's running instance, gives its container
// log lines and makes it exit with code.
func finishRun(t *testing.T, h *harness, n *testnode.Node, envID uuid.UUID, code int, lines ...string) uuid.UUID {
	t.Helper()
	rows := h.waitRows(envID, spec.KindJob, "migrate", 1, 30*time.Second, "running", func(r store.EnvInstanceRow) bool {
		if !running(r) {
			return false
		}
		p := h.pod("n1", r.Sandbox.ID)
		return p != nil && len(p.Containers) == 1
	})
	id := rows[0].Sandbox.ID
	for _, l := range lines {
		if err := n.Script().AppendLog(id, "main", l); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Script().ExitContainer(id, "main", code); err != nil {
		t.Fatal(err)
	}
	return id
}

// A release job runs before the services, once per template: its exit 0
// is a succeeded run with its log kept; a failing one is retried and its
// failed last attempt fails the release (job_failed), which leaves the
// environment at its realised release until a new one succeeds.
func TestReleaseJobs(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, jobSpec("v1", 1))
	// The service waits for the job.
	never(t, time.Second, "the service started before the job ran", func() bool {
		return len(h.rowsOf(env.ID, spec.KindService, "api")) > 0
	})
	first := finishRun(t, h, n1, env.ID, 0, "applying 0001", "done")
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 30*time.Second)
	runs, err := h.st.ListJobRuns(bg, env.ID, "migrate", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %+v %v", runs, err)
	}
	run := runs[0]
	if run.Phase != model.RunSucceeded || run.ExitCode == nil || *run.ExitCode != 0 || run.Attempt != 1 || run.Release != r1 ||
		!slices.Equal(run.LogTail, []string{"applying 0001", "done"}) || run.FinishedAt.IsZero() || run.SandboxID != first {
		t.Fatalf("run %+v", run)
	}
	if _, err := h.st.GetSandbox(bg, first); err == nil {
		t.Error("the finished job's row stays")
	}
	if len(h.rowsOf(env.ID, spec.KindService, "api")) != 1 {
		t.Error("the service did not start after the job")
	}
	// A release that changes nothing of the job does not run it again.
	r2 := h.apply(env.ID, strings.Replace(jobSpec("v1", 1), `"serve"`, `"serve","--v2"`, 1))
	h.waitRelease(env.ID, r2, model.ReleaseRealised, 30*time.Second)
	if runs, _ := h.st.ListJobRuns(bg, env.ID, "migrate", 10); len(runs) != 1 {
		t.Fatalf("the unchanged job ran again: %+v", runs)
	}

	// A changed job that fails twice (retries 1) fails its release.
	r3 := h.apply(env.ID, jobSpec("v2", 1))
	finishRun(t, h, n1, env.ID, 3, "migrate: boom")
	eventually(t, 30*time.Second, "the second attempt", func() bool {
		runs, _ := h.st.ListJobRuns(bg, env.ID, "migrate", 10)
		return len(runs) == 3 && runs[0].Attempt == 2
	})
	finishRun(t, h, n1, env.ID, 3, "migrate: boom again")
	rel := h.waitRelease(env.ID, r3, model.ReleaseFailed, 30*time.Second)
	if rel.Reason != "job migrate failed its last attempt (2 of 2) with code 3" || rel.FinishedAt.IsZero() {
		t.Errorf("failed release %+v", rel)
	}
	inc := h.incident(env.ID, "job/migrate", model.ConditionJobFailed, 10*time.Second)
	var ev evidence
	if err := json.Unmarshal(inc.Evidence, &ev); err != nil || inc.Release != r3 || inc.Summary != rel.Reason ||
		!slices.Equal(ev.LogTail, []string{"migrate: boom again"}) || len(ev.Exits) != 1 || ev.Exits[0].Code == nil ||
		*ev.Exits[0].Code != 3 {
		t.Fatalf("incident %+v %+v %v", inc, ev, err)
	}
	// The failed release is not retried; the realised one keeps serving.
	never(t, 2*time.Second, "the failed job ran again", func() bool {
		runs, _ := h.st.ListJobRuns(bg, env.ID, "migrate", 10)
		return len(runs) != 3
	})
	if e, _ := h.st.GetEnvironment(bg, env.ID); e.Realised != r2 {
		t.Errorf("realised %d", e.Realised)
	}
	if rows := h.rowsOf(env.ID, spec.KindService, "api"); len(rows) != 1 || rows[0].Instance.Release != r2 {
		t.Errorf("the service after the failure %+v", rows)
	}

	// A new release that succeeds is realised and closes the incident.
	r4 := h.apply(env.ID, jobSpec("v3", 0))
	finishRun(t, h, n1, env.ID, 0)
	h.waitRelease(env.ID, r4, model.ReleaseRealised, 30*time.Second)
	if open := h.incidents(env.ID, true); len(open) != 0 {
		t.Errorf("open incidents %+v", open)
	}
	if rel := h.release(env.ID, r3); rel.State != model.ReleaseFailed {
		t.Errorf("the failed release became %s", rel.State)
	}
}

// A run past its job's timeout fails; one whose node reports no exit
// status fails with that detail; a manual job runs only when asked.
func TestJobTimeoutAndManual(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"jobs":{"migrate":{"image":%q,"timeout":"1s"},
		"backfill":{"image":%q,"on":"manual"}}}`, testImage, testImage))
	rel := h.waitRelease(env.ID, r1, model.ReleaseFailed, 30*time.Second)
	if !strings.Contains(rel.Reason, "timed out after 1s") {
		t.Errorf("reason %q", rel.Reason)
	}
	if runs, _ := h.st.ListJobRuns(bg, env.ID, "backfill", 10); len(runs) != 0 {
		t.Errorf("the manual job ran by itself: %+v", runs)
	}
	r2 := h.apply(env.ID, fmt.Sprintf(`{"jobs":{"migrate":{"image":%q,"command":["x"]},
		"backfill":{"image":%q,"on":"manual"}}}`, testImage, testImage))
	finishRun(t, h, n1, env.ID, -1)
	rel = h.waitRelease(env.ID, r2, model.ReleaseFailed, 30*time.Second)
	if !strings.Contains(rel.Reason, "the node did not report an exit status") {
		t.Errorf("reason %q", rel.Reason)
	}
	if err := h.c.RunJob(bg, env.ID, "nope"); err != ErrNoObject {
		t.Errorf("run an unknown job: %v", err)
	}
	// The target is the realised release: none, so no job to run.
	if err := h.c.RunJob(bg, env.ID, "backfill"); err != ErrNoObject {
		t.Errorf("run a job of no target: %v", err)
	}
	r3 := h.apply(env.ID, fmt.Sprintf(`{"jobs":{"backfill":{"image":%q,"on":"manual"}}}`, testImage))
	h.waitRelease(env.ID, r3, model.ReleaseRealised, 30*time.Second)
	if err := h.c.RunJob(bg, env.ID, "backfill"); err != nil {
		t.Fatal(err)
	}
	// Asked again while it runs: refused, never a second run at once.
	if err := h.c.RunJob(bg, env.ID, "backfill"); !errors.Is(err, model.ErrBusy) ||
		err.Error() != fmt.Sprintf("job backfill runs already (release %d, attempt 1)", r3) {
		t.Fatalf("a second run while one runs: %v", err)
	}
	var id uuid.UUID
	eventually(t, 30*time.Second, "the manual run", func() bool {
		for _, r := range h.rowsOf(env.ID, spec.KindJob, "backfill") {
			if running(r) {
				id = r.Sandbox.ID
				return h.pod("n1", id) != nil
			}
		}
		return false
	})
	if err := n1.Script().ExitContainer(id, "main", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the manual run succeeded", func() bool {
		r, err := h.st.LatestJobRun(bg, env.ID, "backfill")
		return err == nil && r.Phase == model.RunSucceeded && r.Attempt == 1 && r.Release == r3
	})
	// Once it ended, it runs again.
	if err := h.c.RunJob(bg, env.ID, "backfill"); err != nil {
		t.Fatalf("a run after the first ended: %v", err)
	}
	if runs, _ := h.st.ListJobRuns(bg, env.ID, "backfill", 10); len(runs) != 2 {
		t.Errorf("runs %+v", runs)
	}
}
