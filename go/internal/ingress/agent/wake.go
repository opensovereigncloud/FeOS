// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// wakeKey is a service of an environment.
type wakeKey struct{ env, service string }

// wakeEntry is the last Wake of a service: whether it is in flight and
// when it was sent.
type wakeEntry struct {
	inflight bool
	sent     time.Time
}

// wakeForget is how long a service's last Wake is remembered.
const wakeForget = time.Minute

// Wake asks the control plane to wake the asleep instances of a service
// of an environment (its ID), without blocking: the call runs on its own
// goroutine. At most one call per service is in flight, and a service
// is woken again at most every 5 s however often Wake is called, so the
// data path calls it on every pick that finds the service asleep. Without
// a connection the wake is dropped; the next pick after the agent
// connected sends it.
func (a *Agent) Wake(env, service string) {
	a.mu.Lock()
	s, ctx := a.cur, a.runCtx
	if s == nil || ctx == nil {
		a.mu.Unlock()
		return
	}
	k := wakeKey{env, service}
	e := a.wakes[k]
	now := time.Now()
	if e != nil && (e.inflight || now.Sub(e.sent) < a.tune.wakeEvery) {
		a.mu.Unlock()
		return
	}
	if e == nil {
		e = &wakeEntry{}
		a.wakes[k] = e
	}
	e.inflight, e.sent = true, now
	a.goroutines.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.goroutines.Done()
		cctx, cancel := context.WithTimeout(ctx, a.tune.wakeTimeout)
		resp, err := s.client.Wake(cctx, &wire.WakeRequest{Env: env, Service: service})
		cancel()
		a.mu.Lock()
		e.inflight = false
		a.mu.Unlock()
		if err != nil {
			a.rl.warn("wake", "ingress agent: wake failed", "env", env, "service", service, "err", err)
			return
		}
		a.log.Debug("ingress agent: waking", "env", env, "service", service, "waking", resp.Waking, "retryAfter", resp.RetryAfterSeconds)
	}()
}

// sweepWakes forgets the Wake calls that ended long ago.
func (a *Agent) sweepWakes(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, e := range a.wakes {
		if !e.inflight && now.Sub(e.sent) > wakeForget {
			delete(a.wakes, k)
		}
	}
}
