// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package kube is a minimal client of the Kubernetes API: JSON over HTTPS
// with a service account's token, for the few objects feos-k8s reads and
// writes (ADR 0020). It knows only the fields it uses; what it decodes and
// patches is the API's own JSON.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The service account's files in a pod.
const (
	ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	tokenFile         = ServiceAccountDir + "/token"
	caFile            = ServiceAccountDir + "/ca.crt"
	namespaceFile     = ServiceAccountDir + "/namespace"
)

// requestTimeout bounds one request to the API.
const requestTimeout = 30 * time.Second

// maxBody bounds the answer read of one request.
const maxBody = 32 << 20

// Config is how to reach the API.
type Config struct {
	// Server is the API's base URL, https://host:port.
	Server string
	// TokenFile holds the bearer token. It is read at every request: the
	// kubelet rotates a projected token in place. Empty: no token.
	TokenFile string
	// HTTPClient carries the requests; nil: one that trusts CAFile.
	HTTPClient *http.Client
	// CAFile is the API's CA, for the default client; empty: the system's.
	CAFile string
}

// InCluster is the configuration of a pod: the API from
// KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT, the service
// account's token and CA. getenv is os.Getenv outside tests.
func InCluster(getenv func(string) string) (Config, error) {
	host, port := getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return Config{}, errors.New("not in a cluster: KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")
	}
	return Config{Server: "https://" + net.JoinHostPort(host, port), TokenFile: tokenFile, CAFile: caFile}, nil
}

// InClusterNamespace is the namespace of the pod's service account, or "".
func InClusterNamespace() string {
	b, err := os.ReadFile(namespaceFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Client is a client of the API.
type Client struct {
	base      *url.URL
	tokenFile string
	hc        *http.Client
}

// New returns a client of cfg.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimSuffix(cfg.Server, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("the API server %q is not a URL", cfg.Server)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.CAFile != "" {
			pem, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read the API's CA: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("no certificate in %s", cfg.CAFile)
			}
			tc.RootCAs = pool
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = tc
		hc = &http.Client{Transport: tr}
	}
	return &Client{base: u, tokenFile: cfg.TokenFile, hc: hc}, nil
}

// StatusError is an answer of the API other than 2xx: its code and the
// Status object's reason and message.
type StatusError struct {
	Code    int
	Reason  string
	Message string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("kubernetes: %d %s: %s", e.Code, e.Reason, e.Message)
	}
	return fmt.Sprintf("kubernetes: %d %s", e.Code, e.Reason)
}

// IsNotFound reports whether err is a 404 of the API.
func IsNotFound(err error) bool { return hasCode(err, http.StatusNotFound) }

// IsAlreadyExists reports whether err is a 409 AlreadyExists of the API.
func IsAlreadyExists(err error) bool {
	var e *StatusError
	return errors.As(err, &e) && e.Code == http.StatusConflict && e.Reason == "AlreadyExists"
}

func hasCode(err error, code int) bool {
	var e *StatusError
	return errors.As(err, &e) && e.Code == code
}

// Content types of the requests.
const (
	contentJSON       = "application/json"
	contentMergePatch = "application/merge-patch+json"
)

// do sends a request with an optional JSON body and decodes a 2xx answer
// into out (when not nil).
func (c *Client) do(ctx context.Context, method, path string, query url.Values, contentType string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	// path's segments are escaped already.
	target := c.base.String() + path
	if q := query.Encode(); q != "" {
		target += "?" + q
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", contentJSON)
	if in != nil {
		req.Header.Set("Content-Type", contentType)
	}
	if c.tokenFile != "" {
		tok, err := os.ReadFile(c.tokenFile)
		if err != nil {
			return fmt.Errorf("read the service account's token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%s %s: read the answer: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var st struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &st)
		if st.Reason == "" {
			st.Reason = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("%s %s: %w", method, path, &StatusError{Code: resp.StatusCode, Reason: st.Reason, Message: st.Message})
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s %s: decode the answer: %w", method, path, err)
	}
	return nil
}

func selectorQuery(selector string) url.Values {
	q := url.Values{}
	if selector != "" {
		q.Set("labelSelector", selector)
	}
	return q
}

// ListNodes lists the nodes that match a label selector ("": all).
func (c *Client) ListNodes(ctx context.Context, selector string) ([]Node, error) {
	var l struct {
		Items []Node `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/nodes", selectorQuery(selector), "", nil, &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// GetNode reads a node.
func (c *Client) GetNode(ctx context.Context, name string) (Node, error) {
	var n Node
	err := c.do(ctx, http.MethodGet, "/api/v1/nodes/"+url.PathEscape(name), nil, "", nil, &n)
	return n, err
}

// PatchNodeAnnotations sets a node's annotations with a JSON merge patch:
// a nil value removes the annotation.
func (c *Client) PatchNodeAnnotations(ctx context.Context, name string, annotations map[string]*string) error {
	patch := map[string]any{"metadata": map[string]any{"annotations": annotations}}
	return c.do(ctx, http.MethodPatch, "/api/v1/nodes/"+url.PathEscape(name), nil, contentMergePatch, patch, nil)
}

func podsPath(namespace string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods"
}

// ListPods lists the pods of a namespace that match a label selector.
func (c *Client) ListPods(ctx context.Context, namespace, selector string) ([]Pod, error) {
	var l struct {
		Items []Pod `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, podsPath(namespace), selectorQuery(selector), "", nil, &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// CreatePod creates a pod in its namespace.
func (c *Client) CreatePod(ctx context.Context, p Pod) error {
	return c.do(ctx, http.MethodPost, podsPath(p.Metadata.Namespace), nil, contentJSON, p, nil)
}

// DeletePod deletes a pod directly (not an eviction), with the grace
// period of its spec. A pod that does not exist is no error.
func (c *Client) DeletePod(ctx context.Context, namespace, name string) error {
	err := c.do(ctx, http.MethodDelete, podsPath(namespace)+"/"+url.PathEscape(name), nil, "", nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

func secretsPath(namespace string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/secrets"
}

// GetSecret reads a secret.
func (c *Client) GetSecret(ctx context.Context, namespace, name string) (Secret, error) {
	var s Secret
	err := c.do(ctx, http.MethodGet, secretsPath(namespace)+"/"+url.PathEscape(name), nil, "", nil, &s)
	return s, err
}

// CreateSecret creates a secret in its namespace.
func (c *Client) CreateSecret(ctx context.Context, s Secret) error {
	return c.do(ctx, http.MethodPost, secretsPath(s.Metadata.Namespace), nil, contentJSON, s, nil)
}

// PatchSecretData sets keys of a secret's data with a JSON merge patch: a
// nil value removes the key; the other keys stay.
func (c *Client) PatchSecretData(ctx context.Context, namespace, name string, data map[string][]byte) error {
	d := make(map[string]any, len(data))
	for k, v := range data {
		if v == nil {
			d[k] = nil
		} else {
			d[k] = v // encoding/json writes []byte as base64, as the API wants it
		}
	}
	return c.do(ctx, http.MethodPatch, secretsPath(namespace)+"/"+url.PathEscape(name), nil, contentMergePatch,
		map[string]any{"data": d}, nil)
}
