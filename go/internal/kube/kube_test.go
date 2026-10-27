// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recorder answers every request with a fixed answer and records the
// requests.
type recorder struct {
	mu     sync.Mutex
	reqs   []*http.Request
	bodies []string
	status int
	answer string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.bodies = append(r.bodies, string(b))
	status, answer := r.status, r.answer
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, answer)
}

func (r *recorder) set(status int, answer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.answer = status, answer
}

func (r *recorder) last() (*http.Request, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqs[len(r.reqs)-1], r.bodies[len(r.bodies)-1]
}

func newClient(t *testing.T, rec *recorder) (*Client, string) {
	t.Helper()
	srv := httptest.NewTLSServer(rec)
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Server: srv.URL + "/", TokenFile: tok, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c, tok
}

func TestTokenIsReadAtEveryRequest(t *testing.T) {
	rec := &recorder{status: http.StatusOK, answer: `{"items":[{"metadata":{"name":"n1"},"spec":{"unschedulable":true}}]}`}
	c, tok := newClient(t, rec)
	nodes, err := c.ListNodes(context.Background(), "a=b,c")
	if err != nil || len(nodes) != 1 || nodes[0].Metadata.Name != "n1" || !nodes[0].Spec.Unschedulable {
		t.Fatalf("nodes %+v, %v", nodes, err)
	}
	req, _ := rec.last()
	if req.Header.Get("Authorization") != "Bearer one" || req.URL.Path != "/api/v1/nodes" ||
		req.URL.Query().Get("labelSelector") != "a=b,c" {
		t.Fatalf("request %s %v", req.URL, req.Header)
	}
	if err := os.WriteFile(tok, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListNodes(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if req, _ := rec.last(); req.Header.Get("Authorization") != "Bearer two" || req.URL.RawQuery != "" {
		t.Fatalf("after the rotation: %s %v", req.URL, req.Header)
	}
}

func TestStatusErrors(t *testing.T) {
	rec := &recorder{status: http.StatusConflict,
		answer: `{"kind":"Status","status":"Failure","reason":"AlreadyExists","message":"pods \"p\" already exists","code":409}`}
	c, _ := newClient(t, rec)
	err := c.CreatePod(context.Background(), Pod{Metadata: ObjectMeta{Name: "p", Namespace: "feos"}})
	if !IsAlreadyExists(err) || IsNotFound(err) || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err %v", err)
	}
	if req, _ := rec.last(); req.Method != http.MethodPost || req.URL.Path != "/api/v1/namespaces/feos/pods" ||
		req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request %s %s", req.Method, req.URL)
	}

	rec.set(http.StatusNotFound, `{"kind":"Status","reason":"NotFound","code":404}`)
	if _, err := c.GetSecret(context.Background(), "feos", "s"); !IsNotFound(err) {
		t.Fatalf("err %v", err)
	}
	// A delete of a pod that is gone is done.
	if err := c.DeletePod(context.Background(), "feos", "p"); err != nil {
		t.Fatal(err)
	}
	rec.set(http.StatusForbidden, `not json`)
	err = c.DeletePod(context.Background(), "feos", "p")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden || se.Reason != "Forbidden" {
		t.Fatalf("err %v", err)
	}
}

func TestPatches(t *testing.T) {
	rec := &recorder{status: http.StatusOK, answer: `{}`}
	c, _ := newClient(t, rec)
	v := "requested"
	if err := c.PatchNodeAnnotations(context.Background(), "n1", map[string]*string{"a/b": &v, "c": nil}); err != nil {
		t.Fatal(err)
	}
	req, body := rec.last()
	if req.Method != http.MethodPatch || req.URL.Path != "/api/v1/nodes/n1" ||
		req.Header.Get("Content-Type") != "application/merge-patch+json" ||
		body != `{"metadata":{"annotations":{"a/b":"requested","c":null}}}` {
		t.Fatalf("patch %s %s %s", req.Method, req.URL, body)
	}
	if err := c.PatchSecretData(context.Background(), "feos", "join", map[string][]byte{"n1": []byte("tok"), "n2": nil}); err != nil {
		t.Fatal(err)
	}
	req, body = rec.last()
	if req.URL.Path != "/api/v1/namespaces/feos/secrets/join" || body != `{"data":{"n1":"dG9r","n2":null}}` {
		t.Fatalf("patch %s %s", req.URL, body)
	}
}

func TestSecretData(t *testing.T) {
	rec := &recorder{status: http.StatusOK, answer: `{"metadata":{"name":"join"},"data":{"n1":"dG9r"}}`}
	c, _ := newClient(t, rec)
	s, err := c.GetSecret(context.Background(), "feos", "join")
	if err != nil || string(s.Data["n1"]) != "tok" {
		t.Fatalf("secret %+v, %v", s, err)
	}
	b, err := json.Marshal(Secret{Data: map[string][]byte{"n1": []byte("tok")}})
	if err != nil || !strings.Contains(string(b), `"n1":"dG9r"`) {
		t.Fatalf("%s %v", b, err)
	}
}

func TestInCluster(t *testing.T) {
	env := map[string]string{"KUBERNETES_SERVICE_HOST": "fd00::1", "KUBERNETES_SERVICE_PORT": "443"}
	cfg, err := InCluster(func(k string) string { return env[k] })
	if err != nil || cfg.Server != "https://[fd00::1]:443" || cfg.TokenFile != tokenFile || cfg.CAFile != caFile {
		t.Fatalf("%+v %v", cfg, err)
	}
	if _, err := InCluster(func(string) string { return "" }); err == nil {
		t.Fatal("no error outside a cluster")
	}
	if _, err := New(Config{Server: "not a url"}); err == nil {
		t.Fatal("a bad URL is taken")
	}
	if _, err := New(Config{Server: "https://x", CAFile: filepath.Join(t.TempDir(), "none")}); err == nil {
		t.Fatal("a missing CA is taken")
	}
}
