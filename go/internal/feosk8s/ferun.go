// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package feosk8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// FeRun's node statuses (internal/model).
const (
	nodeJoined  = "joined"
	nodeReady   = "ready"
	nodeDown    = "down"
	nodeRemoved = "removed"
)

// descDrained is the status description FeRun's drain loop writes on a
// draining node that holds nothing but hibernated, lost and pending
// sandboxes, with no op in flight (internal/sandbox, drainNode).
const descDrained = "drained"

// descCannotDrain begins the description of a drain that cannot go on.
const descCannotDrain = "cannot drain"

// ferunTimeout bounds one request to FeRun.
const ferunTimeout = 30 * time.Second

// FeRunNode is a node as FeRun's admin API lists it: the fields the
// controller reads.
type FeRunNode struct {
	Node              string `json:"node"`
	Status            string `json:"status"`
	StatusDescription string `json:"statusDescription"`
	Draining          bool   `json:"draining"`
	IdentityForked    bool   `json:"identityForked"`
}

// JoinToken is a join token as FeRun lists it, without its secret.
type JoinToken struct {
	ID        string `json:"id"`
	Node      string `json:"node"`
	CreatedBy string `json:"createdBy"`
	ExpiresAt string `json:"expiresAt"`
	UsedAt    string `json:"usedAt"`
}

// NewJoinToken is a join token just made: Token is its secret, shown once.
type NewJoinToken struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
	Node      string `json:"node"`
}

// FeRunError is an answer of FeRun other than 2xx.
type FeRunError struct {
	Status  int
	Message string
}

func (e *FeRunError) Error() string { return fmt.Sprintf("ferun: %d: %s", e.Status, e.Message) }

// isFeRunStatus reports whether err is FeRun's answer with status code.
func isFeRunStatus(err error, code int) bool {
	var e *FeRunError
	return errors.As(err, &e) && e.Status == code
}

// FeRun is a client of FeRun's admin API (/api/v1/admin/…), with Basic
// credentials of ferund's --admin-users.
type FeRun struct {
	base      string
	credsFile string
	hc        *http.Client
}

// NewFeRun returns a client of the FeRun at baseURL (http(s)://host[:port]),
// authenticating with the "user:password" in credsFile, read again at
// every request so that a rotated secret takes effect. hc nil: a client of
// its own.
func NewFeRun(baseURL, credsFile string, hc *http.Client) (*FeRun, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("the FeRun URL %q is not an http(s) URL", baseURL)
	}
	if hc == nil {
		hc = &http.Client{}
	}
	return &FeRun{base: strings.TrimSuffix(baseURL, "/"), credsFile: credsFile, hc: hc}, nil
}

// credentials reads the user and password.
func (f *FeRun) credentials() (user, password string, err error) {
	b, err := os.ReadFile(f.credsFile)
	if err != nil {
		return "", "", fmt.Errorf("read the FeRun credentials: %w", err)
	}
	user, password, ok := strings.Cut(strings.TrimSpace(string(b)), ":")
	if !ok || user == "" || password == "" {
		return "", "", fmt.Errorf("the FeRun credentials in %s are not user:password", f.credsFile)
	}
	return user, password, nil
}

// do sends a request with an optional JSON body and decodes a 2xx answer
// into out (when not nil).
func (f *FeRun) do(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, ferunTimeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.base+path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	user, password, err := f.credentials()
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, password)
	req.Header.Set("Accept", "application/json")
	// As FeRun's UI and its other clients do; it keeps a 401 from asking
	// for a browser's dialog.
	req.Header.Set("X-Requested-With", "ferun")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read the answer: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("%s %s: %w", method, path, &FeRunError{Status: resp.StatusCode, Message: e.Error})
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s %s: decode the answer: %w", method, path, err)
	}
	return nil
}

func nodePath(node string) string { return "/api/v1/admin/nodes/" + url.PathEscape(node) }

// ListNodes is GET /api/v1/admin/nodes.
func (f *FeRun) ListNodes(ctx context.Context) ([]FeRunNode, error) {
	var out struct {
		Nodes []FeRunNode `json:"nodes"`
	}
	if err := f.do(ctx, http.MethodGet, "/api/v1/admin/nodes", nil, &out); err != nil {
		return nil, err
	}
	return out.Nodes, nil
}

// Drain is POST /api/v1/admin/nodes/{node}/drain; it answers the node.
func (f *FeRun) Drain(ctx context.Context, node string) (FeRunNode, error) {
	var n FeRunNode
	err := f.do(ctx, http.MethodPost, nodePath(node)+"/drain", nil, &n)
	return n, err
}

// Undrain is POST /api/v1/admin/nodes/{node}/undrain; it answers the node.
func (f *FeRun) Undrain(ctx context.Context, node string) (FeRunNode, error) {
	var n FeRunNode
	err := f.do(ctx, http.MethodPost, nodePath(node)+"/undrain", nil, &n)
	return n, err
}

// RemoveNode is DELETE /api/v1/admin/nodes/{node}?force=true: the node is
// removed even with sandboxes on it, which then go through the down sweep
// and the lost grace at once.
func (f *FeRun) RemoveNode(ctx context.Context, node string) error {
	return f.do(ctx, http.MethodDelete, nodePath(node)+"?force=true", nil, nil)
}

// CreateJoinToken is POST /api/v1/admin/join-tokens, a token pinned to
// node, valid for ttl.
func (f *FeRun) CreateJoinToken(ctx context.Context, node string, ttl time.Duration) (NewJoinToken, error) {
	in := struct {
		TTLSeconds int64  `json:"ttlSeconds"`
		Node       string `json:"node"`
	}{int64(ttl / time.Second), node}
	var t NewJoinToken
	err := f.do(ctx, http.MethodPost, "/api/v1/admin/join-tokens", in, &t)
	return t, err
}

// ListJoinTokens is GET /api/v1/admin/join-tokens, without secrets.
func (f *FeRun) ListJoinTokens(ctx context.Context) ([]JoinToken, error) {
	var out struct {
		JoinTokens []JoinToken `json:"joinTokens"`
	}
	if err := f.do(ctx, http.MethodGet, "/api/v1/admin/join-tokens", nil, &out); err != nil {
		return nil, err
	}
	return out.JoinTokens, nil
}

// RevokeJoinToken is DELETE /api/v1/admin/join-tokens/{id}.
func (f *FeRun) RevokeJoinToken(ctx context.Context, id string) error {
	return f.do(ctx, http.MethodDelete, "/api/v1/admin/join-tokens/"+url.PathEscape(id), nil, nil)
}
