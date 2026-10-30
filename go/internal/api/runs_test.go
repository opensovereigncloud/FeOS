// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/gardenlinux/feos/go/internal/apierr"
	envs "github.com/gardenlinux/feos/go/internal/env"
)

var (
	_ Runs = (*fakeRuns)(nil)
	_ Runs = (*envs.Service)(nil)
)

// fakeRuns answers the run routes from a map, and records the calls.
type fakeRuns struct {
	mu    sync.Mutex
	runs  map[string]envs.RunStatus
	keys  map[string]string // key → run ID
	calls []string
	lines int
}

func (f *fakeRuns) call(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeRuns) CreateRun(_ context.Context, tenant, key string, req envs.CreateRunRequest) (envs.RunStatus, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runs == nil {
		f.runs, f.keys = map[string]envs.RunStatus{}, map[string]string{}
	}
	if id, ok := f.keys[key]; ok && key != "" {
		return f.runs[id], false, nil
	}
	r := envs.RunStatus{ID: "a0000000001", Image: req.Image, Phase: "queued", Labels: req.Labels}
	f.runs[r.ID], f.keys[key] = r, r.ID
	f.calls = append(f.calls, "CreateRun "+tenant+" "+key)
	return r, true, nil
}

func (f *fakeRuns) ListRuns(_ context.Context, tenant string, labels map[string]string) (envs.RunList, error) {
	f.call("ListRuns " + tenant + " " + labels["project"])
	return envs.RunList{Runs: []envs.RunStatus{}}, nil
}

func (f *fakeRuns) get(id string) (envs.RunStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return envs.RunStatus{}, apierr.New(http.StatusNotFound, "no such run")
	}
	return r, nil
}

func (f *fakeRuns) GetRun(_ context.Context, _, id string) (envs.RunStatus, error) { return f.get(id) }

func (f *fakeRuns) RunLogs(_ context.Context, _, id string, lines int) (envs.RunLogs, error) {
	if _, err := f.get(id); err != nil {
		return envs.RunLogs{}, err
	}
	f.mu.Lock()
	f.lines = lines
	f.mu.Unlock()
	return envs.RunLogs{Lines: []string{"done"}}, nil
}

func (f *fakeRuns) CancelRun(_ context.Context, _, id string) (envs.RunStatus, error) {
	r, err := f.get(id)
	r.Phase = "cancelled"
	return r, err
}

func (f *fakeRuns) DeleteRun(_ context.Context, _, id string) error {
	_, err := f.get(id)
	return err
}

// The run routes: a create is 201, a repeat with its Idempotency-Key 200
// with the same run, a key outside the alphabet 400; get, logs (lines
// passed), cancel and delete adapt one call each; a tenant token only.
func TestRunRoutes(t *testing.T) {
	e := newEnv(t, envOptions{})
	tok := bearerOf(e.tenantToken)
	create := func(key string) *requestResult {
		h := http.Header{"Authorization": {tok}}
		if key != "" {
			h.Set("Idempotency-Key", key)
		}
		w := e.serve(request{method: "POST", path: "/api/v1/runs", header: h,
			body: map[string]any{"image": "builder:1", "labels": map[string]string{"project": "p1"}}})
		return &requestResult{code: w.Code, body: w.Body.String()}
	}
	first := create("build-1")
	if first.code != http.StatusCreated {
		t.Fatalf("create: %d %s", first.code, first.body)
	}
	if again := create("build-1"); again.code != http.StatusOK || again.body != first.body {
		t.Fatalf("repeat: %d %s", again.code, again.body)
	}
	if bad := create("no spaces"); bad.code != http.StatusBadRequest {
		t.Fatalf("bad key: %d %s", bad.code, bad.body)
	}
	expect(t, e.do("GET", "/api/v1/runs/a0000000001", tok, nil), http.StatusOK, "")
	expect(t, e.do("GET", "/api/v1/runs/zzz", tok, nil), http.StatusNotFound, errBody("no such run"))
	expect(t, e.do("GET", "/api/v1/runs/a0000000001/logs?lines=5", tok, nil), http.StatusOK, `{"lines":["done"]}`+"\n")
	if e.runs.lines != 5 {
		t.Errorf("logs asked for %d lines", e.runs.lines)
	}
	expect(t, e.do("GET", "/api/v1/runs?label=project=p1", tok, nil), http.StatusOK, `{"runs":[]}`+"\n")
	expect(t, e.do("GET", "/api/v1/runs?label=nokey", tok, nil), http.StatusBadRequest, "")
	w := e.do("POST", "/api/v1/runs/a0000000001/cancel", tok, nil)
	expect(t, w, http.StatusOK, "")
	expect(t, e.do("DELETE", "/api/v1/runs/a0000000001", tok, nil), http.StatusNoContent, "")
	expect(t, e.do("GET", "/api/v1/runs", "", nil), http.StatusUnauthorized, "")
}

// requestResult is a response's status and body.
type requestResult struct {
	code int
	body string
}
