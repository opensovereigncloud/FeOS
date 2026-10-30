// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// pathTenant is ferund's call that names a token's tenant.
const pathTenant = "/api/v1/tenant"

// tenantCacheMax bounds the cached tokens; beyond it the expired go, and
// then all.
const tenantCacheMax = 10000

// Tenants resolves a reader's token to its tenant through ferund (GET
// /api/v1/tenant with the same Authorization) and keeps a tenant it was
// told for TTL, by the SHA-256 of the header. Refusals are not kept.
type Tenants struct {
	ferunURL string
	hc       *http.Client
	ttl      time.Duration
	now      func() time.Time

	mu sync.Mutex
	m  map[[32]byte]tenantEntry
}

type tenantEntry struct {
	tenant  string
	expires time.Time
}

// NewTenants returns the resolver of ferund at ferunURL.
func NewTenants(ferunURL string, hc *http.Client, ttl time.Duration) *Tenants {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Tenants{ferunURL: strings.TrimSuffix(ferunURL, "/"), hc: hc, ttl: ttl, now: time.Now, m: map[[32]byte]tenantEntry{}}
}

// refusal is ferund's answer that is not a tenant, passed on as it came.
type refusal struct {
	status      int
	contentType string
	body        []byte
}

func (r *refusal) Error() string {
	return fmt.Sprintf("ferund answered %d: %s", r.status, strings.TrimSpace(string(r.body)))
}

// Resolve returns the tenant of the Authorization header auth. A 401 of
// ferund comes back as a *refusal; any other failure as an error.
func (t *Tenants) Resolve(ctx context.Context, auth string) (string, error) {
	h := sha256.Sum256([]byte(auth))
	now := t.now()
	t.mu.Lock()
	e, ok := t.m[h]
	t.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.tenant, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.ferunURL+pathTenant, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", auth)
	resp, err := t.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("ask ferund for the tenant: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return "", fmt.Errorf("ask ferund for the tenant: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return "", &refusal{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: b}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ask ferund for the tenant: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var a struct {
		Tenant string `json:"tenant"`
	}
	if err := json.Unmarshal(b, &a); err != nil || a.Tenant == "" {
		return "", fmt.Errorf("ask ferund for the tenant: no tenant in %q", b)
	}
	t.mu.Lock()
	if len(t.m) >= tenantCacheMax {
		for k, v := range t.m {
			if !now.Before(v.expires) {
				delete(t.m, k)
			}
		}
		if len(t.m) >= tenantCacheMax {
			clear(t.m)
		}
	}
	t.m[h] = tenantEntry{tenant: a.Tenant, expires: now.Add(t.ttl)}
	t.mu.Unlock()
	return a.Tenant, nil
}
