// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/env/envdeps"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// previewSpec has an attached service in sb beside stageSpec's objects.
func previewSpec(sb uuid.UUID) string {
	return strings.Replace(stageSpec, `"services": {`, `"services": {
    "dev": {"runtime": "attached", "sandbox": "`+sb.String()+`", "command": ["npm", "run", "dev"], "cwd": "/workspace/app",
      "ports": [{"name": "http", "port": 5173}], "env": {"API": {"service": "api", "port": "http", "key": "url"}}},`, 1)
}

func TestClone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	sb := h.sandbox(tenantA, true)
	src := h.create("preview", nil)
	srcID := uuid.MustParse(src.ID)
	h.putSecret("preview", "token", "v1")
	h.putSecret("preview", "token", "v2")
	withDev := strings.Replace(previewSpec(sb), `"root": {"path": "/", "service": "web", "port": "http"}`,
		`"root": {"path": "/", "service": "web", "port": "http"}, "dev": {"path": "/dev", "service": "dev", "port": "http"}`, 1)
	if _, err := h.svc.Apply(ctx, tenantA, "preview", ApplyRequest{Spec: json.RawMessage(withDev),
		Annotations: map[string]string{"fecode.dev/revision": "r7"}}); err != nil {
		t.Fatal(describe(err))
	}

	// Without the secret, and asking for data: both refused at once.
	_, err := h.svc.Clone(ctx, tenantA, "preview", CloneRequest{Name: "demo", Data: true})
	var ise *InvalidSpecError
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "data", "copying data is not available") ||
		!hasFinding(ise.Findings, spec.SeverityError, "services.api.env.TOKEN", "the new environment has no secret token") ||
		!hasFinding(ise.Findings, spec.SeverityInfo, "services.dev", "attached service dev is not cloned") ||
		!hasFinding(ise.Findings, spec.SeverityInfo, "routes.dev", "route dev is not cloned") || len(ise.Findings) != 4 {
		t.Fatalf("clone without the secret: %v", describe(err))
	}
	if ise.Findings[2].Remedy != `pass secrets: {"token": {"copy": true}} or a value` ||
		ise.Error() != "the spec has 2 errors, the first: data: copying data is not available" {
		t.Fatalf("remedy %q, %v", ise.Findings[2].Remedy, ise)
	}
	if _, err := h.svc.GetEnv(ctx, tenantA, "demo"); err == nil {
		t.Fatal("a refused clone left an environment")
	}

	// Copied: the latest version, sealed again for the new environment.
	id := uuid.New()
	st, err := h.svc.Clone(ctx, tenantA, "preview", CloneRequest{Name: "demo", ID: id.String(),
		Labels: map[string]string{"fecode.class": "demo"}, Secrets: map[string]CloneSecret{"token": {Copy: true}},
		Author: "a"})
	if err != nil {
		t.Fatal(describe(err))
	}
	if st.ID != id.String() || st.Release != 1 || st.Rollout.State != model.ReleasePending || st.Labels["fecode.class"] != "demo" {
		t.Fatalf("clone status %+v", st)
	}
	r, err := h.st.GetRelease(ctx, id, 1)
	if err != nil || r.Origin != model.OriginClone || r.OriginRef != srcID.String()+"/1" ||
		r.Annotations["fecode.dev/revision"] != "r7" || r.Author != "a" {
		t.Fatalf("release 1 %+v %v", r, err)
	}
	sp, _ := parseStored(r.Spec)
	if _, ok := sp.Services["dev"]; ok || sp.Routes["dev"].Path != "" || sp.Secrets["token"].Version != 1 ||
		sp.Services["api"].ImageAsWritten != "ghcr.io/acme/api:1" {
		t.Fatalf("cloned spec %s", r.Spec)
	}
	sec, err := h.st.GetSecret(ctx, id, "token", 1)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := h.sealer.Open(sec.Sealed, envdeps.SecretAAD(id, "token", 1)); err != nil || string(v) != "v2" {
		t.Fatalf("copied secret %q %v", v, err)
	}
	if _, err := h.sealer.Open(sec.Sealed, envdeps.SecretAAD(srcID, "token", 1)); err == nil {
		t.Fatal("the copy opens under the source's AAD")
	}

	// A given spec, a set value, another release; a taken name.
	given := strings.Replace(stageSpec, `"replicas": 2`, `"replicas": 1`, 1)
	one := 1
	value := "fresh"
	st, err = h.svc.Clone(ctx, tenantA, src.ID, CloneRequest{Name: "demo2", Release: &one, Spec: json.RawMessage(given),
		Secrets: map[string]CloneSecret{"token": {Value: &value}}})
	if err != nil {
		t.Fatal(describe(err))
	}
	e2 := h.env("demo2")
	sec, _ = h.st.GetSecret(ctx, e2.ID, "token", 1)
	if v, err := h.sealer.Open(sec.Sealed, envdeps.SecretAAD(e2.ID, "token", 1)); err != nil || string(v) != "fresh" {
		t.Fatalf("set secret %q %v", v, err)
	}
	sp2, _ := h.svc.releaseSpec(ctx, e2, 1)
	if *sp2.Services["api"].Replicas != 1 {
		t.Fatalf("given spec %+v", sp2.Services["api"])
	}
	_, err = h.svc.Clone(ctx, tenantA, "preview", CloneRequest{Name: "demo", Secrets: map[string]CloneSecret{"token": {Copy: true}}})
	wantStatus(t, err, http.StatusConflict, "an environment named demo exists")
	_, err = h.svc.Clone(ctx, tenantA, "preview", CloneRequest{Name: "demo3", Secrets: map[string]CloneSecret{
		"token": {Copy: true, Value: &value}, "absent": {Copy: true}}})
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "secrets.token", "is {\"copy\": true} or") ||
		!hasFinding(ise.Findings, spec.SeverityError, "secrets.absent", "has no secret absent to copy") {
		t.Fatalf("bad secrets %v", describe(err))
	}
	nine := 9
	_, err = h.svc.Clone(ctx, tenantA, "preview", CloneRequest{Name: "demo3", Release: &nine})
	wantStatus(t, err, http.StatusNotFound, "no such release")
	_, err = h.svc.Clone(ctx, tenantB, "preview", CloneRequest{Name: "demo3"})
	wantStatus(t, err, http.StatusNotFound, "no such environment")
	h.create("bare", nil)
	_, err = h.svc.Clone(ctx, tenantA, "bare", CloneRequest{Name: "demo3"})
	if !errors.As(err, &ise) || !hasFinding(ise.Findings, spec.SeverityError, "release", "has no release to clone") {
		t.Fatalf("clone of nothing %v", describe(err))
	}
}

func TestStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	sb := h.sandbox(tenantA, true)
	st := h.create("stage", map[string]string{"k": "v"})
	e := h.env("stage")
	h.putSecret("stage", "token", "x")
	h.apply("stage", previewSpec(sb), 0)
	h.exec("UPDATE env_releases SET state = 'realised', started_at = now(), finished_at = now() WHERE env_id = $1", e.ID)
	h.exec("UPDATE environments SET realised = 1 WHERE id = $1", e.ID)

	api0 := h.instance(e.ID, spec.KindService, "api", 0, 1, model.PhaseReady, model.StateRunning, "n1", "fd6b::11")
	h.instance(e.ID, spec.KindService, "api", 1, 1, model.PhaseExited, model.StateRunning, "n2", "fd6b::12")
	h.instance(e.ID, spec.KindService, "web", 0, 1, model.PhaseReady, model.StatePaused, "n1", "")
	pg := h.instance(e.ID, spec.KindAddon, "pg", 0, 1, model.PhaseReady, model.StateRunning, "n1", "fd6b::5")
	h.exec("UPDATE env_instances SET provisioned_hash = 'p', restarts = 2, last_exit = 1, ready_since = now() WHERE sandbox_id = $1", pg)
	h.exec("UPDATE sandboxes SET status_description = 'no capacity' WHERE id = $1", api0)
	pending := h.instance(e.ID, spec.KindAddon, "q", 0, 1, model.PhaseStarting, model.StatePending, "", "")
	h.exec("UPDATE sandboxes SET status_description = 'no capacity on any node' WHERE id = $1", pending)
	if _, err := h.st.InsertAttached(ctx, store.EnvAttached{EnvID: e.ID, Service: "dev", SandboxID: sb,
		Phase: model.PhaseStarting, Detail: "cwd /workspace/app does not exist yet"}); err != nil {
		t.Fatal(err)
	}
	code := 0
	if _, err := h.st.InsertJobRun(ctx, store.EnvJobRun{EnvID: e.ID, Job: "migrate", TemplateHash: "h", Release: 1,
		Attempt: 1, Phase: model.RunFailed}); err != nil {
		t.Fatal(err)
	}
	run, err := h.st.InsertJobRun(ctx, store.EnvJobRun{EnvID: e.ID, Job: "migrate", TemplateHash: "h", Release: 1,
		Attempt: 2, Phase: model.RunSucceeded, ExitCode: &code})
	if err != nil {
		t.Fatal(err)
	}
	h.exec("UPDATE env_job_runs SET finished_at = now() WHERE id = $1", run.ID)
	if _, _, err := h.st.OpenIncident(ctx, store.EnvIncident{EnvID: e.ID, Object: "service/api", Release: 1,
		Condition: model.ConditionCrashLoop, Summary: "api exited 3 times",
		Evidence: json.RawMessage(`{"exits":[{"at":"2026-10-18T14:05:01Z","code":1}],"logTail":["boom"]}`)}); err != nil {
		t.Fatal(err)
	}

	got, err := h.svc.GetEnv(ctx, tenantA, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Release != 1 || got.Realised != 1 || got.Rollout.State != model.ReleaseRealised || got.Rollout.StartedAt == "" ||
		got.OpenIncidents != 1 || got.Labels["k"] != "v" {
		t.Fatalf("status %+v", got)
	}
	names := []string{}
	for _, o := range got.Objects {
		names = append(names, o.Kind+"/"+o.Name)
	}
	if strings.Join(names, " ") != "service/api service/dev service/web addon/pg addon/q job/migrate" {
		t.Fatalf("objects %v", names)
	}
	api := got.Objects[0]
	if api.Runtime != spec.RuntimeImage || api.Desired != 2 || api.Ready != 1 || len(api.Instances) != 2 ||
		api.Instances[0].ID != api0.String() || api.Instances[0].Node != "n1" || api.Instances[0].Address != "fd6b::11" ||
		api.Instances[0].Detail != "" || api.Instances[1].Phase != model.PhaseExited {
		t.Fatalf("api %+v", api)
	}
	dev := got.Objects[1]
	if dev.Runtime != spec.RuntimeAttached || dev.Sandbox != sb.String() || dev.Phase != model.PhaseStarting ||
		dev.Detail != "cwd /workspace/app does not exist yet" {
		t.Fatalf("dev %+v", dev)
	}
	if web := got.Objects[2]; web.Ready != 0 || web.Desired != 1 || web.Instances[0].State != model.StatePaused {
		t.Fatalf("web %+v", web)
	}
	pgo := got.Objects[3]
	if pgo.Type != "postgres" || pgo.Version != "17" || pgo.Plan != "small" || pgo.Phase != model.PhaseReady ||
		!pgo.Provisioned || pgo.Instances[0].Restarts != 2 || *pgo.Instances[0].LastExit != 1 || pgo.Instances[0].ReadySince == "" {
		t.Fatalf("pg %+v", pgo)
	}
	if q := got.Objects[4]; q.Phase != model.PhaseStarting || q.Provisioned || q.Instances[0].Detail != "no capacity on any node" {
		t.Fatalf("q %+v", q)
	}
	job := got.Objects[5]
	if job.LastRun == nil || job.LastRun.Attempt != 2 || job.LastRun.Phase != model.RunSucceeded || *job.LastRun.ExitCode != 0 ||
		job.LastRun.FinishedAt == "" {
		t.Fatalf("job %+v", job.LastRun)
	}
	if len(got.Routes) != 2 || got.Routes[0] != (RouteStatus{Name: "api", Path: "/api", Service: "api", Port: 8080}) ||
		got.Routes[1].Port != 3000 {
		t.Fatalf("routes %+v", got.Routes)
	}
	// The JSON has every field of its kind.
	b, _ := json.Marshal(got)
	for _, want := range []string{`"lastRun":{"release":1,"attempt":2,"phase":"succeeded","exitCode":0`,
		`"kind":"service","name":"dev","runtime":"attached","sandbox":"` + sb.String() + `","phase":"starting","restarts":0,"lastExit":null`,
		`"openIncidents":1`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("status JSON lacks %s:\n%s", want, b)
		}
	}

	// Incidents.
	list, err := h.svc.Incidents(ctx, tenantA, "stage", false)
	if err != nil || len(list.Incidents) != 1 || list.Incidents[0].Object != "service/api" ||
		list.Incidents[0].Evidence.LogTail[0] != "boom" || *list.Incidents[0].Evidence.Exits[0].Code != 1 {
		t.Fatalf("incidents %+v %v", list, err)
	}
	inc, err := h.svc.Incident(ctx, tenantA, "stage", mustInt64(t, list.Incidents[0].ID))
	if err != nil || inc.Condition != model.ConditionCrashLoop || inc.ClosedAt != "" {
		t.Fatalf("incident %+v %v", inc, err)
	}
	_, err = h.svc.Incident(ctx, tenantA, "stage", 999)
	wantStatus(t, err, http.StatusNotFound, "no such incident")
	h.exec("UPDATE env_incidents SET closed_at = now() WHERE env_id = $1", e.ID)
	if list, _ := h.svc.Incidents(ctx, tenantA, "stage", true); len(list.Incidents) != 0 {
		t.Fatalf("open %+v", list)
	}

	// A failed latest release: the objects are the realised release's,
	// the rollout the latest's.
	changed := strings.Replace(previewSpec(sb), `"replicas": 2`, `"replicas": 4`, 1)
	h.apply("stage", changed, 1)
	h.exec("UPDATE env_releases SET state = 'failed', reason = 'rollout timed out' WHERE env_id = $1 AND number = 2", e.ID)
	got, _ = h.svc.GetEnv(ctx, tenantA, "stage")
	if got.Rollout.Release != 2 || got.Rollout.State != model.ReleaseFailed || got.Rollout.Reason != "rollout timed out" ||
		got.Objects[0].Desired != 2 {
		t.Fatalf("failed latest %+v", got)
	}
	h.exec("UPDATE env_releases SET state = 'rolling' WHERE env_id = $1 AND number = 2", e.ID)
	if got, _ := h.svc.GetEnv(ctx, tenantA, "stage"); got.Objects[0].Desired != 4 {
		t.Fatalf("rolling latest %+v", got.Objects[0])
	}
	list2, _ := h.svc.ListEnvs(ctx, tenantA, nil)
	if list2.Environments[0].State != model.ReleaseRolling || list2.Environments[0].OpenIncidents != 0 {
		t.Fatalf("list %+v", list2)
	}
}

func mustInt64(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if err := json.Unmarshal([]byte(s), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// doorMux serves the route door at its pattern.
func doorMux(svc *Service) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/environments/{env}/http/{path...}", svc.ServeRoute)
	return mux
}

func routeRequest(mux http.Handler, method, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Host = "stage.boxed.example"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func wantPlain(t *testing.T, w *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if w.Code != status || w.Body.String() != body {
		t.Fatalf("%d %q, want %d %q", w.Code, w.Body.String(), status, body)
	}
	if status >= 400 && w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type %q", w.Header().Get("Content-Type"))
	}
}

func TestRouteDoor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := h.ctx()
	mux := doorMux(h.svc)
	sb := h.sandbox(tenantA, true)
	st := h.create("stage", nil)
	e := h.env("stage")
	h.create("prod", nil)
	tok, err := h.svc.CreateRouteToken(ctx, tenantA, "stage")
	if err != nil {
		t.Fatal(err)
	}
	prodTok, _ := h.svc.CreateRouteToken(ctx, tenantA, "prod")

	// The token: required, the environment's own; a tenant token is no
	// route token.
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/", ""), 401, "route token required\n")
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/", "not-a-token"), 401, "route token required\n")
	tenantTok := "tenant-token"
	if _, err := h.st.CreateTenantToken(ctx, store.HashToken(tenantTok), tenantA, "t"); err != nil {
		t.Fatal(err)
	}
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/", tenantTok), 401, "route token required\n")
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/", prodTok.Token), 404, "no such environment\n")
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/nope/http/", tok.Token), 404, "no such environment\n")
	// No release, no route.
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/x", tok.Token), 404, "no route for /x\n")

	h.putSecret("stage", "token", "x")
	h.apply("stage", previewSpec(sb), 0)
	// Nothing runs: 503, Retry-After 5.
	w := routeRequest(mux, "GET", "/api/v1/environments/stage/http/api/v", tok.Token)
	wantPlain(t, w, 503, "no ready instance of api\n")
	if w.Header().Get("Retry-After") != "5" {
		t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
	}

	a0 := h.instance(e.ID, spec.KindService, "api", 0, 1, model.PhaseReady, model.StateRunning, "n1", "fd6b::11")
	a1 := h.instance(e.ID, spec.KindService, "api", 1, 1, model.PhaseReady, model.StateRunning, "n2", "fd6b::12")
	h.instance(e.ID, spec.KindService, "api", 2, 1, model.PhaseRetiring, model.StateRunning, "n2", "fd6b::13")
	h.instance(e.ID, spec.KindService, "api", 3, 1, model.PhaseStarting, model.StateRunning, "n2", "fd6b::14")
	web := h.instance(e.ID, spec.KindService, "web", 0, 1, model.PhaseReady, model.StatePaused, "n1", "")

	// /api strips its prefix; ready instances take turns; the query and
	// the Host stay, the bearer goes to the door, which strips it.
	seen := map[uuid.UUID]int{}
	for range 4 {
		w := routeRequest(mux, "GET", "/api/v1/environments/stage/http/api/reports/2026?month=10", tok.Token)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		f := h.ports.last(t)
		if f.port != 8080 || f.path != "/reports/2026" || f.rawPath != "" || f.query != "month=10" ||
			f.host != "stage.boxed.example" {
			t.Fatalf("forward %+v", f)
		}
		seen[f.sandbox]++
	}
	if seen[a0] != 2 || seen[a1] != 2 {
		t.Fatalf("round robin %v (want %s and %s twice)", seen, a0, a1)
	}
	// Segment-aware: /apix is the root's; the root does not strip; a
	// paused instance is woken by the forward (scale from zero).
	for _, c := range []struct{ path, up string }{
		{"/apix", "/apix"}, {"/", "/"}, {"/api", "/"}, {"/api/", "/"}, {"/static/app.js", "/static/app.js"},
	} {
		w := routeRequest(mux, "GET", "/api/v1/environments/"+st.ID+"/http"+c.path, tok.Token)
		f := h.ports.last(t)
		if w.Code != 200 || f.path != c.up {
			t.Fatalf("%s: %d %+v", c.path, w.Code, f)
		}
		if strings.HasPrefix(c.path, "/apix") || c.path == "/" || strings.HasPrefix(c.path, "/static") {
			if f.sandbox != web || f.port != 3000 {
				t.Fatalf("%s went to %+v", c.path, f)
			}
		}
	}
	// An escaped segment stays escaped upstream.
	routeRequest(mux, "GET", "/api/v1/environments/stage/http/api/a%2Fb/c%20d", tok.Token)
	if f := h.ports.last(t); f.path != "/a/b/c d" || f.rawPath != "/a%2Fb/c%20d" {
		t.Fatalf("escaped %+v", f)
	}
	// A route to an attached service goes to its sandbox.
	h.apply("stage", strings.Replace(previewSpec(sb), `"root": {"path": "/", "service": "web", "port": "http"}`,
		`"root": {"path": "/", "service": "web", "port": "http"}, "dev": {"path": "/dev", "service": "dev", "port": "http"}`, 1), 1)
	routeRequest(mux, "POST", "/api/v1/environments/stage/http/dev/src/main.ts", tok.Token)
	if f := h.ports.last(t); f.sandbox != sb || f.port != 5173 || f.path != "/dev/src/main.ts" {
		t.Fatalf("attached %+v", f)
	}
	// The realised release's routes serve while a later one rolls.
	h.exec("UPDATE environments SET realised = 1 WHERE id = $1", e.ID)
	if w := routeRequest(mux, "GET", "/api/v1/environments/stage/http/dev/x", tok.Token); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if f := h.ports.last(t); f.sandbox != web {
		t.Fatalf("realised release's routes: %+v", f)
	}

	// .. segments, also escaped (the mux redirects plain ones itself).
	r := httptest.NewRequest("GET", "/api/v1/environments/stage/http/api/%2e%2e/admin", nil)
	r.Header.Set("Authorization", "Bearer "+tok.Token)
	r.SetPathValue("env", "stage")
	w = httptest.NewRecorder()
	h.svc.ServeRoute(w, r)
	wantPlain(t, w, 400, "the path has a .. segment\n")
	r = httptest.NewRequest("GET", "/api/v1/environments/stage/http/api/%2e/admin", nil)
	r.Header.Set("Authorization", "Bearer "+tok.Token)
	r.SetPathValue("env", "stage")
	w = httptest.NewRecorder()
	h.svc.ServeRoute(w, r)
	wantPlain(t, w, 400, "the path has a . segment\n")
	// A percent sign is a character like any other.
	routeRequest(mux, "GET", "/api/v1/environments/stage/http/api/q/100%25", tok.Token)
	if f := h.ports.last(t); f.path != "/q/100%" || f.rawPath != "" {
		t.Fatalf("percent %+v", f)
	}

	// The token was used; revoked, it opens nothing.
	rt, _ := h.st.ResolveRouteToken(ctx, store.HashToken(tok.Token))
	if rt.LastUsedAt.IsZero() {
		t.Fatal("last_used_at not set")
	}
	if err := h.svc.RevokeRouteTokens(ctx, tenantA, "stage"); err != nil {
		t.Fatal(err)
	}
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/stage/http/", tok.Token), 401, "route token required\n")
	// A deleting environment is gone for the door.
	if err := h.svc.DeleteEnv(ctx, tenantA, "prod"); err != nil {
		t.Fatal(err)
	}
	wantPlain(t, routeRequest(mux, "GET", "/api/v1/environments/prod/http/", prodTok.Token), 404, "no such environment\n")

	// Without the port door: 503.
	off := newHarness(t, harnessOptions{noPorts: true})
	off.create("stage", nil)
	off.putSecret("stage", "token", "x")
	off.apply("stage", stageSpec, 0)
	offTok, _ := off.svc.CreateRouteToken(off.ctx(), tenantA, "stage")
	wantPlain(t, routeRequest(doorMux(off.svc), "GET", "/api/v1/environments/stage/http/", offTok.Token), 503,
		"not available on this instance\n")
}

func TestRoutePaths(t *testing.T) {
	routes := map[string]spec.Route{
		"root": {Path: "/"}, "api": {Path: "/api"}, "deep": {Path: "/api/v2/"}, "x": {Path: "/a b"},
	}
	for _, c := range []struct {
		raw  string
		want string
		n    int
	}{
		{"/", "/", 0}, {"/api", "/api", 1}, {"/api/", "/api", 1}, {"/apis", "/", 0}, {"/api/v2", "/api/v2/", 2},
		{"/api/v2/x", "/api/v2/", 2}, {"/api/v3", "/api", 1}, {"/a%20b/c", "/a b", 1}, {"/API", "/", 0},
	} {
		r, n, ok := matchRoute(routes, c.raw)
		if !ok || r.Path != c.want || n != c.n {
			t.Errorf("%s: %+v %d %v, want %s %d", c.raw, r, n, ok, c.want, c.n)
		}
	}
	if _, _, ok := matchRoute(map[string]spec.Route{"api": {Path: "/api"}}, "/web"); ok {
		t.Error("matched without a root")
	}
	for _, c := range []struct {
		raw, path, rawPath string
		n                  int
	}{
		{"/api/x", "/x", "", 1}, {"/api", "/", "", 1}, {"/api/a%2Fb", "/a/b", "/a%2Fb", 1}, {"/a/b/c", "/a/b/c", "", 0},
	} {
		p, rp, err := stripSegments(c.raw, c.n)
		if err != nil || p != c.path || rp != c.rawPath {
			t.Errorf("strip %s %d: %q %q %v", c.raw, c.n, p, rp, err)
		}
	}
	// Only a segment that is, or decodes once or twice to, . or .. is
	// refused; a % that starts no escape after a decode is a character.
	for _, c := range []struct{ raw, want string }{
		{"/a/../b", ".."}, {"/a/%2e%2e/b", ".."}, {"/a/%252e%252e/b", ".."}, {"/a%2F..%2Fb", ".."},
		{"/%252e%252e/x%25", ".."}, {"/%252e%2e/x", ".."}, {"/a/%2e/b", "."}, {"/a/%252E", "."},
		{"/a/..b/c", ""}, {"/a/b", ""}, {"/q/100%25", ""}, {"/search/50%25off", ""}, {"/a%2541", ""},
		{"/a/%25252e%25252e/b", ""}, {"/%2e%2e%zz", ""}, {"/files/a%25zz/..x", ""},
	} {
		if got := dotSegment(c.raw); got != c.want {
			t.Errorf("dot segment of %s: %q, want %q", c.raw, got, c.want)
		}
	}
}
