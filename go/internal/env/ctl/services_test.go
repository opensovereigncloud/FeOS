// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

// serviceSpec is a spec with one image service.
func serviceSpec(replicas int, version string, extra string) string {
	return fmt.Sprintf(`{"services":{"web":{"image":%q,"command":["/app"],"replicas":%d,
		"env":{"VERSION":%q},"ports":[{"name":"http","port":8080}]%s}}}`, testImage, replicas, version, extra)
}

// An image service rolls to a new template one replica at a time, a new
// one first (surge 1): never more than one replica above its count, the
// old ones retire only once the new are ready; a new count adds or
// retires replicas (the highest first) and keeps the others.
func TestImageServiceRollingAndScale(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(2, "1", ""))
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 30*time.Second)
	v1 := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	if v1[0].Instance.Replica != 0 || v1[1].Instance.Replica != 1 || v1[0].Sandbox.IdleAfterS == nil ||
		*v1[0].Sandbox.IdleAfterS != 0 || v1[0].Sandbox.MemoryMiB != 512 || v1[0].Sandbox.VCPUs != 1 {
		t.Fatalf("v1 rows %+v", v1)
	}
	oldHash := v1[0].Instance.TemplateHash

	// The rolling: watch the rows while it goes.
	var mu sync.Mutex
	maxRows, sawRetiring := 0, false
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			rows, err := h.st.ListEnvInstanceRows(bg, env.ID)
			if err != nil {
				continue
			}
			live, newReady := 0, 0
			retiringOld := false
			for _, r := range rows {
				if r.Instance.Phase == model.PhaseRetiring {
					retiringOld = true
					continue
				}
				live++
				if r.Instance.TemplateHash != oldHash && r.Instance.Phase == model.PhaseReady {
					newReady++
				}
			}
			mu.Lock()
			maxRows = max(maxRows, live)
			if retiringOld {
				sawRetiring = true
				if newReady == 0 {
					t.Errorf("an old replica retired before a new one was ready")
				}
			}
			mu.Unlock()
		}
	}()
	r2 := h.apply(env.ID, serviceSpec(2, "2", ""))
	h.waitRelease(env.ID, r2, model.ReleaseRealised, 60*time.Second)
	close(stop)
	<-watched
	mu.Lock()
	if maxRows > 3 || !sawRetiring {
		t.Errorf("at most %d replicas at once (want 3), retiring seen %v", maxRows, sawRetiring)
	}
	mu.Unlock()
	v2 := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	for _, r := range v2 {
		if r.Instance.TemplateHash == oldHash || slices.ContainsFunc(v1, func(o store.EnvInstanceRow) bool { return o.Sandbox.ID == r.Sandbox.ID }) {
			t.Fatalf("an old replica after the rolling: %+v", r)
		}
		if r.Sandbox.PodTemplate.Env["VERSION"].Value != "2" {
			t.Errorf("template %+v", r.Sandbox.PodTemplate)
		}
	}
	eventually(t, 10*time.Second, "the old rows gone", func() bool { return len(h.rows(env.ID)) == 2 })
	for _, o := range v1 {
		if _, err := h.st.GetSandbox(bg, o.Sandbox.ID); err == nil {
			t.Errorf("old row %s stays", o.Sandbox.ID)
		}
	}

	// Scale up: the kept replicas stay, one is added.
	r3 := h.apply(env.ID, serviceSpec(3, "2", ""))
	h.waitRelease(env.ID, r3, model.ReleaseRealised, 30*time.Second)
	v3 := h.waitRows(env.ID, spec.KindService, "web", 3, 10*time.Second, "ready", ready)
	kept := 0
	for _, r := range v3 {
		if slices.ContainsFunc(v2, func(o store.EnvInstanceRow) bool { return o.Sandbox.ID == r.Sandbox.ID }) {
			kept++
			if r.Instance.Release != r3 {
				t.Errorf("a kept replica's release is %d", r.Instance.Release)
			}
		}
	}
	if kept != 2 || v3[2].Instance.Replica != 2 {
		t.Fatalf("after the scale up: %d kept, %+v", kept, v3)
	}

	// Scale down: the highest replicas retire.
	r4 := h.apply(env.ID, serviceSpec(1, "2", `,"idle":"15m"`))
	h.waitRelease(env.ID, r4, model.ReleaseRealised, 30*time.Second)
	v4 := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)
	if v4[0].Instance.Replica != 0 || v4[0].Sandbox.ID != v3[0].Sandbox.ID {
		t.Fatalf("after the scale down %+v", v4)
	}
	// idle is not in the template: kept, with its idle_after_s.
	if v4[0].Sandbox.IdleAfterS == nil || *v4[0].Sandbox.IdleAfterS != 900 {
		t.Errorf("idle_after_s %v", v4[0].Sandbox.IdleAfterS)
	}
	eventually(t, 10*time.Second, "the retired rows gone", func() bool { return len(h.rows(env.ID)) == 1 })

	// Healing: a stopped replica comes back.
	if err := h.st.InTx(bg, func(tx *store.Store) error {
		_, err := tx.Q().Exec(bg, `UPDATE sandboxes SET client_state = 'stopped' WHERE id = $1`, v4[0].Sandbox.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.waitRows(env.ID, spec.KindService, "web", 1, 30*time.Second, "running again", running)
}

// A container that exits is restarted with a growing backoff, and 3 exits
// within the window open crash_loop with the exits and the log tail; a
// new instance of a rolling release that exits 3 times fails the release
// (rollout_failed), retires the new instances and keeps the old serving.
func TestCrashLoopAndRolloutFailure(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 30*time.Second)
	row := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0]
	id := row.Sandbox.ID
	var gaps []time.Duration
	for i := 1; i <= 3; i++ {
		_ = n1.Script().AppendLog(id, "main", fmt.Sprintf("crash %d", i))
		eventually(t, 30*time.Second, "the container running", func() bool {
			p := h.pod("n1", id)
			return p != nil && len(p.Containers) == 1 && p.Containers[0].State.String() == "CONTAINER_STATE_RUNNING" &&
				running(h.rowsOf(env.ID, spec.KindService, "web")[0])
		})
		if err := n1.Script().ExitContainer(id, "main", 1); err != nil {
			t.Fatal(err)
		}
		var inst store.EnvInstance
		eventually(t, 10*time.Second, fmt.Sprintf("exit %d recorded", i), func() bool {
			inst, _ = h.st.GetEnvInstance(bg, id)
			return inst.Restarts == i && inst.Phase == model.PhaseExited
		})
		last := inst.Exits[len(inst.Exits)-1]
		if last.Code == nil || *last.Code != 1 || inst.LastExit == nil || *inst.LastExit != 1 ||
			!strings.HasPrefix(inst.Detail, "exited with code 1; restarting in ") {
			t.Fatalf("exit %+v", inst)
		}
		if !inst.NextRestartAt.IsZero() {
			gaps = append(gaps, inst.NextRestartAt.Sub(last.At))
		}
	}
	if len(gaps) != 3 || gaps[0] != 200*time.Millisecond || gaps[1] != 400*time.Millisecond || gaps[2] != 800*time.Millisecond {
		t.Errorf("backoffs %v", gaps)
	}
	inc := h.incident(env.ID, "service/web", model.ConditionCrashLoop, 10*time.Second)
	var ev evidence
	if err := json.Unmarshal(inc.Evidence, &ev); err != nil || len(ev.Exits) != 3 || len(ev.LogTail) != 3 ||
		inc.Release != r1 || inc.Summary != "web (release 1) exited 3 times within 10 minutes, last with code 1" {
		t.Fatalf("incident %+v %+v", inc, ev)
	}
	// Restarted once more, it is ready again.
	h.waitRows(env.ID, spec.KindService, "web", 1, 30*time.Second, "ready again", ready)

	// A release whose new instance crashes fails; the old one serves on.
	r2 := h.apply(env.ID, serviceSpec(1, "2", ""))
	var newID uuid.UUID
	for i := 1; i <= 3; i++ {
		eventually(t, 30*time.Second, "the new instance running", func() bool {
			for _, r := range h.rowsOf(env.ID, spec.KindService, "web") {
				if r.Sandbox.ID == id || !running(r) {
					continue
				}
				p := h.pod("n1", r.Sandbox.ID)
				if p != nil && len(p.Containers) == 1 && p.Containers[0].State.String() == "CONTAINER_STATE_RUNNING" &&
					r.Instance.Phase != model.PhaseExited {
					newID = r.Sandbox.ID
					return true
				}
			}
			return false
		})
		if err := n1.Script().ExitContainer(newID, "main", 2); err != nil {
			t.Fatal(err)
		}
		eventually(t, 10*time.Second, "the new instance's exit", func() bool {
			inst, err := h.st.GetEnvInstance(bg, newID)
			return err == nil && len(inst.Exits) == i
		})
	}
	rel := h.waitRelease(env.ID, r2, model.ReleaseFailed, 30*time.Second)
	if !strings.Contains(rel.Reason, "exited 3 times") {
		t.Errorf("reason %q", rel.Reason)
	}
	inc = h.incident(env.ID, "service/web", model.ConditionRolloutFailed, 10*time.Second)
	if err := json.Unmarshal(inc.Evidence, &ev); err != nil || len(ev.Exits) != 3 || inc.Release != r2 {
		t.Fatalf("rollout incident %+v %+v", inc, ev)
	}
	eventually(t, 30*time.Second, "the new instance gone, the old one serving", func() bool {
		rows := h.rows(env.ID)
		return len(rows) == 1 && rows[0].Sandbox.ID == id && ready(rows[0])
	})
	if e, _ := h.st.GetEnvironment(bg, env.ID); e.Realised != r1 {
		t.Errorf("realised %d", e.Realised)
	}
}

// Readiness checks move an instance's phase: http (through the port
// door to the pod's port), tcp and exec; starting → ready on the first
// success, ready → unready after failureThreshold failures, back on a
// success; the initial delay counts.
func TestReadinessPhases(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	var status atomic.Int32
	status.Store(503)
	var hosts sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts.Store(r.Host, r.URL.Path)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", `,"readiness":{"http":{"port":"http","path":"/readyz"},
		"period":"1s","failureThreshold":2,"initialDelay":"1s"}`))
	row := h.waitRows(env.ID, spec.KindService, "web", 1, 30*time.Second, "running", running)[0]
	n1.Script().SetPodPortAddr(row.Sandbox.ID, 8080, strings.TrimPrefix(srv.URL, "http://"))
	eventually(t, 30*time.Second, "a failing check's detail", func() bool {
		inst, _ := h.st.GetEnvInstance(bg, row.Sandbox.ID)
		return inst.Phase == model.PhaseStarting && strings.Contains(inst.Detail, "answered 503")
	})
	if rel := h.release(env.ID, r1); rel.State != model.ReleaseRolling {
		t.Errorf("release %s while its instance is starting", rel.State)
	}
	status.Store(200)
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 30*time.Second)
	if p, ok := hosts.Load("localhost:8080"); !ok || p != "/readyz" {
		t.Error("the probe did not ask for localhost:8080/readyz")
	}
	status.Store(500)
	eventually(t, 30*time.Second, "unready", func() bool {
		inst, _ := h.st.GetEnvInstance(bg, row.Sandbox.ID)
		return inst.Phase == model.PhaseUnready && inst.ProbeFailures >= 2 && inst.ReadySince.IsZero()
	})
	status.Store(204)
	eventually(t, 30*time.Second, "ready again", func() bool {
		inst, _ := h.st.GetEnvInstance(bg, row.Sandbox.ID)
		return inst.Phase == model.PhaseReady && inst.ProbeFailures == 0 && !inst.ReadySince.IsZero()
	})

	// tcp: nothing listens → starting; a listener → ready.
	r2 := h.apply(env.ID, serviceSpec(1, "2", `,"readiness":{"tcp":{"port":8080},"period":"1s"}`))
	var tcpRow store.EnvInstanceRow
	eventually(t, 30*time.Second, "the tcp instance", func() bool {
		for _, r := range h.rowsOf(env.ID, spec.KindService, "web") {
			if r.Sandbox.ID != row.Sandbox.ID && running(r) {
				tcpRow = r
				return true
			}
		}
		return false
	})
	n1.Script().SetPodPortAddr(tcpRow.Sandbox.ID, 8080, "127.0.0.1:1")
	eventually(t, 30*time.Second, "the tcp check failing", func() bool {
		inst, _ := h.st.GetEnvInstance(bg, tcpRow.Sandbox.ID)
		return strings.Contains(inst.Detail, "tcp port 8080")
	})
	n1.Script().SetPodPortAddr(tcpRow.Sandbox.ID, 8080, strings.TrimPrefix(srv.URL, "http://"))
	h.waitRelease(env.ID, r2, model.ReleaseRealised, 30*time.Second)

	// exec: exit 1, then 0.
	var code atomic.Int32
	code.Store(1)
	n1.Script().SetExec(func(_ context.Context, _ uuid.UUID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		if len(req.Command) > 0 && req.Command[0] == "check" {
			return code.Load(), true, nil
		}
		return 0, false, nil
	})
	r3 := h.apply(env.ID, serviceSpec(1, "3", `,"readiness":{"exec":{"command":["check","--deep"]},"period":"1s"}`))
	eventually(t, 30*time.Second, "the exec check failing", func() bool {
		for _, r := range h.rowsOf(env.ID, spec.KindService, "web") {
			if strings.Contains(r.Instance.Detail, "check exited 1") {
				return true
			}
		}
		return false
	})
	code.Store(0)
	h.waitRelease(env.ID, r3, model.ReleaseRealised, 30*time.Second)
}

// crash makes an instance's container exit n times (each once the
// container runs again), its restarts counting from restarts.
func crash(t *testing.T, h *harness, n *testnode.Node, envID, id uuid.UUID, restarts, times int) {
	t.Helper()
	for i := 1; i <= times; i++ {
		eventually(t, 30*time.Second, "the container running", func() bool {
			p := h.pod("n1", id)
			inst, err := h.st.GetEnvInstance(bg, id)
			return p != nil && len(p.Containers) == 1 && p.Containers[0].State.String() == "CONTAINER_STATE_RUNNING" &&
				err == nil && inst.Phase != model.PhaseExited
		})
		if err := n.Script().ExitContainer(id, "main", 1); err != nil {
			t.Fatal(err)
		}
		eventually(t, 10*time.Second, fmt.Sprintf("exit %d recorded", i), func() bool {
			inst, _ := h.st.GetEnvInstance(bg, id)
			return inst.Restarts == restarts+i && inst.Phase == model.PhaseExited
		})
	}
}

// A crash loop's incident closes once the object has been ready for the
// crash window, and a later crash loop of the same release opens a new
// one.
func TestCrashLoopCloses(t *testing.T) {
	h := newHarness(t, options{timings: func(t *timings) { t.crashWindow = 12 * time.Second }})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 30*time.Second)
	id := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0].Sandbox.ID
	crash(t, h, n1, env.ID, id, 0, 3)
	first := h.incident(env.ID, "service/web", model.ConditionCrashLoop, 10*time.Second)
	eventually(t, 60*time.Second, "the crash loop closed", func() bool {
		for _, i := range h.incidents(env.ID, false) {
			if i.ID == first.ID {
				return !i.ClosedAt.IsZero()
			}
		}
		return false
	})
	inst, _ := h.st.GetEnvInstance(bg, id)
	if inst.Restarts != 0 {
		t.Errorf("restarts %d after the crash window", inst.Restarts)
	}
	crash(t, h, n1, env.ID, id, 0, 3)
	second := h.incident(env.ID, "service/web", model.ConditionCrashLoop, 10*time.Second)
	if second.ID == first.ID || second.Release != r1 {
		t.Fatalf("the second crash loop %+v, the first %+v", second, first)
	}
}

// Incidents that nothing else closes: crash_loop, volume_unavailable and
// addon_unhealthy of objects that neither T nor R has close; a crash
// loop of an older release closes once a later release is realised.
func TestIncidentsClose(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 30*time.Second)
	open := func(object, condition string) int64 {
		i, _, err := h.st.OpenIncident(bg, store.EnvIncident{EnvID: env.ID, Object: object, Release: r1, Condition: condition,
			Summary: "test", Evidence: []byte("{}")})
		if err != nil {
			t.Fatal(err)
		}
		return i.ID
	}
	gone := []int64{open("addon/cache", model.ConditionVolumeUnavailable), open("service/old", model.ConditionCrashLoop),
		open("addon/cache", model.ConditionAddonUnhealthy)}
	stays := open("service/web", model.ConditionCrashLoop)
	closed := func(id int64) bool {
		for _, i := range h.incidents(env.ID, false) {
			if i.ID == id {
				return !i.ClosedAt.IsZero()
			}
		}
		return false
	}
	eventually(t, 30*time.Second, "the removed objects' incidents closed", func() bool {
		h.c.notify(bg, HintReconcile)
		return closed(gone[0]) && closed(gone[1]) && closed(gone[2])
	})
	if closed(stays) {
		t.Fatal("the crash loop of an object T has closed")
	}
	r2 := h.apply(env.ID, serviceSpec(1, "2", ""))
	h.waitRelease(env.ID, r2, model.ReleaseRealised, 30*time.Second)
	if !closed(stays) {
		t.Error("the crash loop of release 1 stays open after release 2 was realised")
	}
}
