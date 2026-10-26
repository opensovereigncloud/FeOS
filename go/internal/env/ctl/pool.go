// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"log/slog"
	"sync"

	"github.com/ironcore-dev/feos/go/internal/podid"
)

// poolWorkers is how many tasks the pool runs at once (§4.2: 8 per
// instance).
const poolWorkers = 8

// pool runs the controller's slow work off its loops: Engine.Ensure of
// the rows that should run, and the start of an attached service in a
// sandbox it has to wake. At most poolWorkers tasks run at once; a task
// whose key is queued or running is not queued again (single flight per
// row). Each task runs with the context it was queued with, the loop's,
// which ends with the leader's term; a worker ends when the queue is
// empty.
type pool struct {
	log     *slog.Logger
	workers int

	mu      sync.Mutex
	queue   []task
	keys    map[string]bool
	running int
	wg      sync.WaitGroup
}

// task is a queued piece of work.
type task struct {
	ctx context.Context
	key string
	fn  func(ctx context.Context)
}

func newPool(workers int, log *slog.Logger) *pool {
	return &pool{log: log, workers: workers, keys: map[string]bool{}}
}

// run queues fn under key unless a task of key is queued or running; it
// reports whether it queued it.
func (p *pool) run(ctx context.Context, key string, fn func(ctx context.Context)) bool {
	if ctx.Err() != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.keys[key] {
		return false
	}
	p.keys[key] = true
	p.queue = append(p.queue, task{ctx: ctx, key: key, fn: fn})
	if p.running < p.workers {
		p.running++
		p.wg.Add(1)
		go p.work()
	}
	return true
}

// busy reports whether a task of key is queued or running.
func (p *pool) busy(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.keys[key]
}

// work runs queued tasks until the queue is empty.
func (p *pool) work() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.running--
			p.mu.Unlock()
			return
		}
		t := p.queue[0]
		p.queue = p.queue[1:]
		p.mu.Unlock()
		if t.ctx.Err() == nil {
			t.fn(t.ctx)
		}
		p.mu.Lock()
		delete(p.keys, t.key)
		p.mu.Unlock()
	}
}

// wait waits for the running tasks; their contexts end them.
func (p *pool) wait() { p.wg.Wait() }

// ensureKey is the pool's key of a row's Ensure.
func ensureKey(id podid.ID) string { return "ensure/" + id.String() }

// ensure queues Engine.Ensure of the row: it brings the row's pod up
// (create, start, resume, restore). Its failure is logged; the next pass
// sees the row as it is.
func (c *Controller) ensure(ctx context.Context, id podid.ID) {
	c.pool.run(ctx, ensureKey(id), func(ctx context.Context) {
		if _, err := c.d.Engine.Ensure(ctx, id); err != nil && ctx.Err() == nil {
			c.log.Debug("an instance did not come up", "sandbox", id, "err", err)
		}
	})
}
