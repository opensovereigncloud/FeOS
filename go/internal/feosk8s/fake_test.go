// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package feosk8s

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ironcore-dev/feos/go/internal/kube"
)

// fakeKube is the Kubernetes API of the objects the controller uses:
// nodes, pods and secrets, with label selectors of k=v terms and JSON
// merge patches.
type fakeKube struct {
	mu      sync.Mutex
	token   string
	nodes   map[string]kube.Node
	pods    map[string]kube.Pod    // namespace/name
	secrets map[string]kube.Secret // namespace/name
	writes  []string               // the mutating calls, "METHOD path"
}

func newFakeKube(token string) *fakeKube {
	return &fakeKube{token: token, nodes: map[string]kube.Node{}, pods: map[string]kube.Pod{},
		secrets: map[string]kube.Secret{}}
}

func (k *fakeKube) addNode(name string, labels map[string]string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.nodes[name] = kube.Node{Metadata: kube.ObjectMeta{Name: name, UID: "uid-" + name, Labels: labels}}
}

func (k *fakeKube) setUnschedulable(name string, v bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := k.nodes[name]
	n.Spec.Unschedulable = v
	k.nodes[name] = n
}

func (k *fakeKube) deleteNode(name string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.nodes, name)
}

func (k *fakeKube) node(name string) kube.Node {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.nodes[name]
}

func (k *fakeKube) pod(ns, name string) (kube.Pod, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.pods[ns+"/"+name]
	return p, ok
}

func (k *fakeKube) putPod(p kube.Pod) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pods[p.Metadata.Namespace+"/"+p.Metadata.Name] = p
}

func (k *fakeKube) secret(ns, name string) (kube.Secret, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	s, ok := k.secrets[ns+"/"+name]
	return s, ok
}

func (k *fakeKube) putSecret(s kube.Secret) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.secrets[s.Metadata.Namespace+"/"+s.Metadata.Name] = s
}

func (k *fakeKube) takeWrites() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	w := k.writes
	k.writes = nil
	return w
}

// matches reports whether labels match a selector of k=v and k terms.
func matches(selector string, labels map[string]string) bool {
	if selector == "" {
		return true
	}
	for _, term := range strings.Split(selector, ",") {
		key, val, eq := strings.Cut(term, "=")
		have, ok := labels[key]
		if !ok || (eq && have != val) {
			return false
		}
	}
	return true
}

func kubeStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "status": "Failure", "code": code,
		"reason": reason, "message": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// mergePatch applies a JSON merge patch (RFC 7386) to v, through JSON.
func mergePatch[T any](v *T, patch []byte) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var doc, p map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		return err
	}
	merged := merge(doc, p)
	b, err = json.Marshal(merged)
	if err != nil {
		return err
	}
	var out T // a fresh value: decoding into v would keep the keys removed
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	*v = out
	return nil
}

func merge(doc, patch map[string]any) map[string]any {
	if doc == nil {
		doc = map[string]any{}
	}
	for k, pv := range patch {
		if pv == nil {
			delete(doc, k)
			continue
		}
		if pm, ok := pv.(map[string]any); ok {
			dm, _ := doc[k].(map[string]any)
			doc[k] = merge(dm, pm)
			continue
		}
		doc[k] = pv
	}
	return doc
}

func (k *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+k.token {
		kubeStatus(w, http.StatusUnauthorized, "Unauthorized", "")
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if r.Method != http.MethodGet {
		k.writes = append(k.writes, r.Method+" "+r.URL.Path)
	}
	body, _ := io.ReadAll(r.Body)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	sel := r.URL.Query().Get("labelSelector")
	switch {
	case len(parts) == 3 && parts[2] == "nodes" && r.Method == http.MethodGet:
		items := []kube.Node{}
		for _, n := range k.nodes {
			if matches(sel, n.Metadata.Labels) {
				items = append(items, n)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case len(parts) == 4 && parts[2] == "nodes":
		n, ok := k.nodes[parts[3]]
		if !ok {
			kubeStatus(w, http.StatusNotFound, "NotFound", "nodes "+parts[3]+" not found")
			return
		}
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				kubeStatus(w, http.StatusUnsupportedMediaType, "UnsupportedMediaType", "")
				return
			}
			if err := mergePatch(&n, body); err != nil {
				kubeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
				return
			}
			k.nodes[n.Metadata.Name] = n
		default:
			kubeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
			return
		}
		writeJSON(w, http.StatusOK, n)
	case len(parts) >= 5 && parts[2] == "namespaces" && parts[4] == "pods":
		k.servePods(w, r, parts[3], parts[5:], sel, body)
	case len(parts) >= 5 && parts[2] == "namespaces" && parts[4] == "secrets":
		k.serveSecrets(w, r, parts[3], parts[5:], body)
	default:
		kubeStatus(w, http.StatusNotFound, "NotFound", "no route "+r.URL.Path)
	}
}

func (k *fakeKube) servePods(w http.ResponseWriter, r *http.Request, ns string, rest []string, sel string, body []byte) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		items := []kube.Pod{}
		for _, p := range k.pods {
			if p.Metadata.Namespace == ns && matches(sel, p.Metadata.Labels) {
				items = append(items, p)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case len(rest) == 0 && r.Method == http.MethodPost:
		var p kube.Pod
		if err := json.Unmarshal(body, &p); err != nil {
			kubeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		key := ns + "/" + p.Metadata.Name
		if _, ok := k.pods[key]; ok {
			kubeStatus(w, http.StatusConflict, "AlreadyExists", "pods "+p.Metadata.Name+" already exists")
			return
		}
		p.Metadata.Namespace = ns
		p.Status.Phase = "Running"
		k.pods[key] = p
		writeJSON(w, http.StatusCreated, p)
	case len(rest) == 1 && r.Method == http.MethodDelete:
		key := ns + "/" + rest[0]
		p, ok := k.pods[key]
		if !ok {
			kubeStatus(w, http.StatusNotFound, "NotFound", "pods "+rest[0]+" not found")
			return
		}
		delete(k.pods, key)
		writeJSON(w, http.StatusOK, p)
	default:
		kubeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
	}
}

func (k *fakeKube) serveSecrets(w http.ResponseWriter, r *http.Request, ns string, rest []string, body []byte) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodPost:
		var s kube.Secret
		if err := json.Unmarshal(body, &s); err != nil {
			kubeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
			return
		}
		key := ns + "/" + s.Metadata.Name
		if _, ok := k.secrets[key]; ok {
			kubeStatus(w, http.StatusConflict, "AlreadyExists", "secrets "+s.Metadata.Name+" already exists")
			return
		}
		s.Metadata.Namespace = ns
		k.secrets[key] = s
		writeJSON(w, http.StatusCreated, s)
	case len(rest) == 1:
		key := ns + "/" + rest[0]
		s, ok := k.secrets[key]
		if !ok {
			kubeStatus(w, http.StatusNotFound, "NotFound", "secrets "+rest[0]+" not found")
			return
		}
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			if err := mergePatch(&s, body); err != nil {
				kubeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
				return
			}
			k.secrets[key] = s
		default:
			kubeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
			return
		}
		writeJSON(w, http.StatusOK, s)
	default:
		kubeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
	}
}

// fakeToken is a join token the fake FeRun made.
type fakeToken struct {
	JoinToken
	secret string
}

// fakeFeRun is FeRun's admin API as the controller uses it.
type fakeFeRun struct {
	mu     sync.Mutex
	user   string
	pass   string
	now    func() time.Time
	nodes  map[string]FeRunNode
	tokens []fakeToken
	seq    int
	writes []string
	// downOnDrain: a drain finds the node down (it went down after the
	// list).
	downOnDrain bool
}

func newFakeFeRun(now func() time.Time) *fakeFeRun {
	return &fakeFeRun{user: "feos-k8s", pass: "s3cret", now: now, nodes: map[string]FeRunNode{}}
}

func (f *fakeFeRun) setNode(n FeRunNode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[n.Node] = n
}

func (f *fakeFeRun) node(name string) (FeRunNode, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[name]
	return n, ok
}

func (f *fakeFeRun) takeWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.writes
	f.writes = nil
	return w
}

func (f *fakeFeRun) liveTokens(node string) []fakeToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeToken
	for _, t := range f.tokens {
		exp, _ := time.Parse(time.RFC3339, t.ExpiresAt)
		if t.Node == node && t.UsedAt == "" && exp.After(f.now()) {
			out = append(out, t)
		}
	}
	return out
}

// useToken marks the token of secret used, as a join does.
func (f *fakeFeRun) useToken(secret string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, t := range f.tokens {
		if t.secret == secret && t.UsedAt == "" {
			f.tokens[i].UsedAt = f.now().UTC().Format(time.RFC3339)
			f.nodes[t.Node] = FeRunNode{Node: t.Node, Status: nodeJoined}
			return true
		}
	}
	return false
}

func ferunError(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func (f *fakeFeRun) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != f.user || p != f.pass {
		ferunError(w, http.StatusUnauthorized, "admin sign-in required")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+r.URL.RequestURI())
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/")
	parts := strings.Split(path, "/")
	switch {
	case path == "nodes" && r.Method == http.MethodGet:
		nodes := []FeRunNode{}
		for _, n := range f.nodes {
			nodes = append(nodes, n)
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "defaults": map[string]any{}})
	case len(parts) == 3 && parts[0] == "nodes" && r.Method == http.MethodPost:
		n, ok := f.nodes[parts[1]]
		if !ok {
			ferunError(w, http.StatusNotFound, "unknown node %s", parts[1])
			return
		}
		switch parts[2] {
		case "drain":
			if f.downOnDrain {
				n.Status = nodeDown
				f.nodes[n.Node] = n
			}
			if n.Status == nodeDown || n.Status == nodeRemoved {
				ferunError(w, http.StatusConflict, "node %s is %s", n.Node, n.Status)
				return
			}
			n.Draining = true
		case "undrain":
			n.Draining = false
			if d := n.StatusDescription; strings.HasPrefix(d, "draining") || d == descDrained ||
				strings.HasPrefix(d, descCannotDrain) {
				n.StatusDescription = ""
			}
		default:
			ferunError(w, http.StatusNotFound, "no such route")
			return
		}
		f.nodes[n.Node] = n
		writeJSON(w, http.StatusOK, n)
	case len(parts) == 2 && parts[0] == "nodes" && r.Method == http.MethodDelete:
		n, ok := f.nodes[parts[1]]
		if !ok {
			ferunError(w, http.StatusNotFound, "unknown node %s", parts[1])
			return
		}
		if r.URL.Query().Get("force") != "true" {
			ferunError(w, http.StatusConflict, "node %s has 1 sandboxes; move them or pass ?force=true", n.Node)
			return
		}
		n.Status = nodeRemoved
		f.nodes[n.Node] = n
		writeJSON(w, http.StatusOK, n)
	case path == "join-tokens" && r.Method == http.MethodPost:
		var in struct {
			TTLSeconds int64  `json:"ttlSeconds"`
			Node       string `json:"node"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.TTLSeconds <= 0 || in.Node == "" {
			ferunError(w, http.StatusBadRequest, "bad join token request")
			return
		}
		f.seq++
		t := fakeToken{JoinToken: JoinToken{ID: fmt.Sprintf("id-%d", f.seq), Node: in.Node, CreatedBy: f.user,
			ExpiresAt: f.now().Add(time.Duration(in.TTLSeconds) * time.Second).UTC().Format(time.RFC3339)},
			secret: fmt.Sprintf("feos-join-secret%d.pin", f.seq)}
		f.tokens = append(f.tokens, t)
		writeJSON(w, http.StatusCreated, NewJoinToken{ID: t.ID, Token: t.secret, ExpiresAt: t.ExpiresAt, Node: t.Node})
	case path == "join-tokens" && r.Method == http.MethodGet:
		out := []JoinToken{}
		for _, t := range f.tokens {
			out = append(out, t.JoinToken)
		}
		writeJSON(w, http.StatusOK, map[string]any{"joinTokens": out})
	case len(parts) == 2 && parts[0] == "join-tokens" && r.Method == http.MethodDelete:
		revoked := false
		for i, t := range f.tokens {
			if t.ID == parts[1] {
				f.tokens = append(f.tokens[:i], f.tokens[i+1:]...)
				revoked = true
				break
			}
		}
		writeJSON(w, http.StatusOK, map[string]bool{"revoked": revoked})
	default:
		ferunError(w, http.StatusNotFound, "no such route: %s %s", r.Method, r.URL.Path)
	}
}
