// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import "sync"

// full is the cap a request found taken when it entered a budget.
type full int

const (
	// fits: the request is counted.
	fits full = iota
	// envFull: its environment's share is taken.
	envFull
	// tenantFull: its tenant's share is taken.
	tenantFull
	// allFull: the cap of the whole instance is taken.
	allFull
)

// budget counts requests against a share per tenant, within it a share
// per environment, and a cap for the whole instance, so that one tenant
// cannot take what the others need, nor one of its environments what
// its others need: the held requests (MaxHeldPerEnv, MaxHeldPerTenant,
// MaxHeld) and the forwarded ones (MaxInflightPerEnv,
// MaxInflightPerTenant, MaxInflight).
type budget struct {
	perEnv, perTenant, total int

	mu      sync.Mutex
	envs    map[string]int
	tenants map[string]int
	n       int
}

func newBudget(perEnv, perTenant, total int) *budget {
	return &budget{perEnv: perEnv, perTenant: perTenant, total: total, envs: map[string]int{}, tenants: map[string]int{}}
}

// slot is one request's place in a budget; only the request's own
// goroutine uses it.
type slot struct {
	tenant, env string
	counted     bool
}

// enter counts a request of the environment env of tenant unless it is
// counted already. It returns fits, or the cap that is taken; then the
// request is not counted.
func (b *budget) enter(s *slot, tenant, env string) full {
	if s.counted {
		return fits
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.envs[env] >= b.perEnv:
		return envFull
	case b.tenants[tenant] >= b.perTenant:
		return tenantFull
	case b.n >= b.total:
		return allFull
	}
	b.envs[env]++
	b.tenants[tenant]++
	b.n++
	s.tenant, s.env, s.counted = tenant, env, true
	return fits
}

// leave ends the request's count, if it has one.
func (b *budget) leave(s *slot) {
	if !s.counted {
		return
	}
	s.counted = false
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n--
	if b.envs[s.env]--; b.envs[s.env] <= 0 {
		delete(b.envs, s.env)
	}
	if b.tenants[s.tenant]--; b.tenants[s.tenant] <= 0 {
		delete(b.tenants, s.tenant)
	}
}

// count is how many requests are counted.
func (b *budget) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// of is how many requests of the environment env are counted.
func (b *budget) of(env string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.envs[env]
}

// ofTenant is how many requests of the tenant are counted.
func (b *budget) ofTenant(tenant string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tenants[tenant]
}
