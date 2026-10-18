// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/tools"
)

// An attached service (§4.6) is a supervised process in the main
// container of a sandbox of the tenant. The supervisor looks at it every
// second in env-probe while the sandbox runs (it never wakes a paused one
// for that) and starts, restarts (with the backoff of §4.4), replaces (a
// changed template), checks and names (hosts block) its process; the
// reconcile step records it and, when its process must be (re)started in a
// sandbox that does not run, wakes the sandbox with an exec hold for the
// start and the first readiness.

// stepAttached keeps the attached services of T (§4.3 step 5).
func (c *Controller) stepAttached(ctx context.Context, s *stepState) {
	v := s.v
	for _, name := range slices.Sorted(maps.Keys(v.target.Services)) {
		svc := v.target.Services[name]
		if runtimeOf(svc) != spec.RuntimeAttached {
			continue
		}
		p := c.planAttached(v, name, svc)
		if !p.ok() {
			s.wait("attached service %s: %s", name, p.why())
			continue
		}
		a, ok := v.attached[name]
		if !ok {
			c.recordAttached(ctx, s, p)
			continue
		}
		if c.d.Runner == nil {
			c.attachedDetail(ctx, v.env.ID, name, "attached services need the tools' runner, which this instance lacks")
			s.wait("attached service %s: no tools' runner", name)
			continue
		}
		if a.SandboxID != p.sandbox {
			c.moveAttached(ctx, v, a, p)
			s.wait("attached service %s moves to sandbox %s", name, p.sandbox)
			continue
		}
		sb, err := c.st.GetSandbox(ctx, p.sandbox)
		if err != nil || sb.IsInstance() || sb.Owner != v.env.Owner {
			c.attachedDetail(ctx, v.env.ID, name, fmt.Sprintf("sandbox %s does not exist", p.sandbox))
			s.wait("attached service %s: sandbox %s does not exist", name, p.sandbox)
			continue
		}
		needsStart := a.ProcessID == "" || a.TemplateHash != p.hash ||
			(a.SandboxIncarnation != uuid.Nil && a.SandboxIncarnation != sb.Incarnation)
		if needsStart && !steady(sb) && sb.Op == "" && (a.NextRestartAt.IsZero() || !v.now.Before(a.NextRestartAt)) {
			c.wakeAttached(ctx, v.env.ID, name, sb.ID)
		}
		if a.Phase != model.PhaseReady || a.TemplateHash != p.hash || a.ProcessID == "" {
			s.wait("attached service %s is %s", name, a.Phase)
			if rolling(v) && !v.latest.StartedAt.IsZero() {
				budget := duration("", spec.DefaultRolloutTimeout)
				if v.now.After(v.latest.StartedAt.Add(budget)) {
					reason := fmt.Sprintf("attached service %s did not become ready within %s", name, budget)
					if a.Detail != "" {
						reason += ": " + a.Detail
					}
					if err := c.failRelease(ctx, v, reason, objectName(spec.KindService, name), model.ConditionRolloutFailed,
						evidence{Diff: c.releaseDiff(ctx, v)}); err == nil {
						s.failed = true
						return
					}
				}
			}
		}
	}
}

// recordAttached records an attached service of T.
func (c *Controller) recordAttached(ctx context.Context, s *stepState, p *attachedPlan) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		if _, err := tx.InsertAttached(ctx, store.EnvAttached{EnvID: s.v.env.ID, Service: p.name, SandboxID: p.sandbox,
			Phase: model.PhaseStarting}); err != nil {
			return err
		}
		// The sandbox joins the environment's group.
		return tx.Notify(ctx, db.ChanLeader, HintGroups)
	})
	if err != nil && !errors.Is(err, store.ErrExists) {
		s.wait("attached service %s: %v", p.name, err)
		return
	}
	s.wait("attached service %s is recorded", p.name)
}

// moveAttached moves an attached service to another sandbox: its process
// in the old one stops (when that runs), the row starts over.
func (c *Controller) moveAttached(ctx context.Context, v *view, a store.EnvAttached, p *attachedPlan) {
	c.stopAttached(ctx, v, a)
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockAttached(ctx, v.env.ID, a.Service, store.ForUpdate)
		if err != nil {
			return err
		}
		after := cur
		after.SandboxID, after.SandboxIncarnation, after.ProcessID, after.TemplateHash = p.sandbox, uuid.Nil, "", ""
		after.Phase, after.ReadySince, after.Restarts, after.LastExit = model.PhaseStarting, time.Time{}, 0, nil
		after.ProbeFailures, after.NextRestartAt, after.HostsWritten, after.Detail = 0, time.Time{}, "", ""
		if _, err := tx.UpdateAttached(ctx, cur, after); err != nil {
			return err
		}
		return tx.Notify(ctx, db.ChanLeader, HintGroups)
	})
	if err != nil && ctx.Err() == nil {
		c.rl.warn("attached", "cannot move an attached service", "environment", v.env.ID, "service", a.Service, "err", err)
	}
}

// stopAttached stops an attached service's process and takes its name
// out of the sandbox's hosts block, when the sandbox runs (a paused one is
// not woken: its process does not survive a cold restore, and a resumed
// one is found by the supervisor no more).
func (c *Controller) stopAttached(ctx context.Context, v *view, a store.EnvAttached) {
	sb, err := c.st.GetSandbox(ctx, a.SandboxID)
	if err != nil || !steady(sb) || c.d.Runner == nil || !c.connected(sb.Node) {
		return
	}
	l := lease(sb)
	if a.ProcessID != "" {
		if err := c.d.Runner.StopProcessDetached(ctx, l, a.ProcessID); err != nil && !errors.Is(err, tools.ErrNoProcess) &&
			ctx.Err() == nil {
			c.rl.warn("attached", "cannot stop an attached service's process", "sandbox", sb.ID, "err", err)
		}
	}
	var own []string
	for _, name := range ownAttached(v, sb.ID) {
		if name != a.Service {
			own = append(own, name)
		}
	}
	block := ""
	if len(own) > 0 {
		block = hostsBlock(v, own)
	}
	if err := c.writeHosts(ctx, l, v.env.Name, block); err != nil && ctx.Err() == nil {
		c.rl.warn("hosts", "cannot rewrite a sandbox's hosts block", "sandbox", sb.ID, "err", err)
	}
}

// removeAttached removes an attached service T no longer has: its
// process, its hosts entries, its row.
func (c *Controller) removeAttached(ctx context.Context, v *view, a store.EnvAttached) {
	c.stopAttached(ctx, v, a)
	err := c.fenced(ctx, func(tx *store.Store) error {
		if _, err := tx.DeleteAttached(ctx, v.env.ID, a.Service); err != nil {
			return err
		}
		return tx.Notify(ctx, db.ChanLeader, HintGroups)
	})
	if err != nil && ctx.Err() == nil {
		c.rl.warn("attached", "cannot delete an attached service", "environment", v.env.ID, "service", a.Service, "err", err)
		return
	}
	c.forget("a/" + attachedKey(v.env.ID, a.Service))
}

// ownAttached are the attached services of T that run in the sandbox.
func ownAttached(v *view, sandboxID uuid.UUID) []string {
	var out []string
	for name, svc := range v.target.Services {
		if runtimeOf(svc) == spec.RuntimeAttached && svc.Sandbox == sandboxID.String() {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// attachedDetail writes an attached service's detail.
func (c *Controller) attachedDetail(ctx context.Context, envID uuid.UUID, service, detail string) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockAttached(ctx, envID, service, store.ForUpdate)
		if err != nil {
			return err
		}
		after := cur
		after.Detail = detail
		_, err = tx.UpdateAttached(ctx, cur, after)
		return err
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("attached", "cannot write an attached service's detail", "service", service, "err", err)
	}
}

// wakeAttached wakes a sandbox for the (re)start of an attached service's
// process, off the loop: Acquire with an exec hold, supervise until the
// process is ready (or attachedReady passed), release.
func (c *Controller) wakeAttached(ctx context.Context, envID uuid.UUID, service string, sandboxID uuid.UUID) {
	if c.d.Runner == nil {
		return
	}
	c.pool.run(ctx, "attached/"+attachedKey(envID, service), func(ctx context.Context) {
		l, release, err := c.d.Engine.Acquire(ctx, sandboxID, model.HoldExec)
		if err != nil {
			if ctx.Err() == nil {
				c.attachedDetail(ctx, envID, service, fmt.Sprintf("sandbox %s could not be woken: %v", sandboxID, err))
			}
			return
		}
		defer release()
		deadline := time.Now().Add(c.t.attachedReady)
		for ctx.Err() == nil && time.Now().Before(deadline) {
			v, err := c.load(ctx, envID)
			if err != nil || v == nil {
				return
			}
			svc, ok := v.target.Services[service]
			if !ok || runtimeOf(svc) != spec.RuntimeAttached {
				return
			}
			sb, err := c.st.GetSandbox(ctx, sandboxID)
			if err != nil {
				return
			}
			p := c.planAttached(v, service, svc)
			if !p.ok() || p.sandbox != sandboxID {
				return
			}
			c.superviseAttached(ctx, v, p, sb, l, true)
			a, err := c.st.GetAttached(ctx, envID, service)
			if err != nil || (a.Phase == model.PhaseReady && a.ProcessID != "") || a.Phase == model.PhaseFailed {
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	})
}

// backoff is the wait before the n-th restart: 2^(n-1) units, at most
// restartMax.
func (c *Controller) backoff(n int) time.Duration {
	d := c.t.restartUnit
	for i := 1; i < n && d < c.t.restartMax; i++ {
		d *= 2
	}
	return min(d, c.t.restartMax)
}

// superviseAttached is one look of the supervisor at an attached service
// in its running sandbox, with the sandbox's lease (§4.6): a new
// incarnation of the sandbox resets the service; a changed template stops
// the old process; a process that is gone counts as an exit, restarted
// after the backoff; a working directory that does not exist yet keeps it
// starting; a running process is checked (readiness) and the sandbox's
// hosts block written. wait: wait for another look in flight instead of
// leaving it to that one.
func (c *Controller) superviseAttached(ctx context.Context, v *view, p *attachedPlan, sb store.Sandbox, l model.Lease, wait bool) {
	if c.d.Runner == nil {
		return
	}
	mu := c.attachedLock(v.env.ID, p.name)
	if wait {
		mu.Lock()
	} else if !mu.TryLock() {
		return
	}
	defer mu.Unlock()
	a, err := c.st.GetAttached(ctx, v.env.ID, p.name)
	if err != nil || a.SandboxID != sb.ID {
		return
	}
	after := a
	now := c.dbNow(ctx)
	if a.SandboxIncarnation != uuid.Nil && a.SandboxIncarnation != sb.Incarnation {
		// A new incarnation of the sandbox: its process is gone, and so is
		// what the old one counted.
		after.Restarts, after.NextRestartAt, after.ProbeFailures, after.HostsWritten = 0, time.Time{}, 0, ""
		after.ProcessID, after.Phase, after.ReadySince, after.SandboxIncarnation = "", model.PhaseStarting, time.Time{}, uuid.Nil
	}
	if after.ProcessID != "" && after.TemplateHash != p.hash {
		if err := c.d.Runner.StopProcessDetached(ctx, l, after.ProcessID); err != nil && !errors.Is(err, tools.ErrNoProcess) {
			after.Detail = "stop the old process: " + err.Error()
			c.writeAttached(ctx, a, after)
			return
		}
		after.ProcessID, after.Phase, after.ReadySince, after.ProbeFailures = "", model.PhaseStarting, time.Time{}, 0
		after.NextRestartAt = time.Time{}
	}
	if after.ProcessID != "" {
		ps, err := c.d.Runner.ProcessesDetached(ctx, l)
		if err != nil {
			after.Detail = "list the processes: " + err.Error()
			c.writeAttached(ctx, a, after)
			return
		}
		var st *tools.ProcessStatus
		for i := range ps {
			if ps[i].ID == after.ProcessID {
				st = &ps[i]
			}
		}
		if st == nil || !st.Running {
			after.Restarts++
			after.LastExit, after.Detail = nil, "the process is gone"
			if st != nil && st.ExitCode != nil {
				code := *st.ExitCode
				after.LastExit, after.Detail = &code, fmt.Sprintf("the process exited with code %d", code)
			}
			after.ProcessID, after.Phase, after.ReadySince, after.ProbeFailures = "", model.PhaseExited, time.Time{}, 0
			after.NextRestartAt = now.Add(c.backoff(after.Restarts))
		}
	}
	if after.ProcessID == "" && (after.NextRestartAt.IsZero() || !now.Before(after.NextRestartAt)) {
		c.startAttached(ctx, v, p, sb, l, &after)
	}
	if after.ProcessID != "" {
		c.attachedReadiness(ctx, p, l, a, &after, now)
	}
	block := hostsBlock(v, ownAttached(v, sb.ID))
	if h := blockHash(block); after.HostsWritten != h {
		if err := c.writeHosts(ctx, l, v.env.Name, block); err != nil {
			c.rl.warn("hosts", "cannot write a sandbox's hosts block", "sandbox", sb.ID, "err", err)
		} else {
			after.HostsWritten = h
		}
	}
	c.writeAttached(ctx, a, after)
}

// startAttached starts an attached service's process: its env resolved
// here (secrets too), never written to disk.
func (c *Controller) startAttached(ctx context.Context, v *view, p *attachedPlan, sb store.Sandbox, l model.Lease, after *store.EnvAttached) {
	env, err := c.resolveEnv(ctx, v.env.ID, p.env)
	if err != nil {
		after.Phase, after.Detail = model.PhaseStarting, "its env: "+err.Error()
		return
	}
	id, err := c.d.Runner.StartProcessDetached(ctx, l, tools.ProcessStart{Argv: p.argv, Cwd: p.cwd, Env: env.vars,
		Name: "ferun-env-" + v.env.Name + "-" + p.name})
	switch {
	case errors.Is(err, tools.ErrNoCwd):
		after.Phase, after.Detail = model.PhaseStarting, fmt.Sprintf("cwd %s does not exist yet", p.cwd)
	case err != nil:
		after.Detail = "start the process: " + redact(err.Error(), env.secrets)
	default:
		after.ProcessID, after.SandboxIncarnation, after.TemplateHash = id, sb.Incarnation, p.hash
		after.Phase, after.ReadySince, after.ProbeFailures, after.NextRestartAt, after.Detail =
			model.PhaseStarting, time.Time{}, 0, time.Time{}, ""
		c.log.Info("attached service started", "environment", v.env.ID, "service", p.name, "sandbox", sb.ID, "process", id)
	}
}

// attachedReadiness checks a running process's readiness (§4.4) and moves
// its phase.
func (c *Controller) attachedReadiness(ctx context.Context, p *attachedPlan, l model.Lease, before store.EnvAttached,
	after *store.EnvAttached, now time.Time) {
	class := "up"
	if after.Phase != model.PhaseReady && after.Phase != model.PhaseUnready {
		class = "starting"
	}
	key := "a/" + attachedKey(before.EnvID, before.Service)
	seen, due := c.seenDue(key, after.ProcessID+"/"+class, p.check)
	if !due {
		return
	}
	ok, why := c.runCheck(ctx, l, p.check, seen)
	applyCheck(ok, why, p.check, now, &after.Phase, &after.ReadySince, &after.ProbeFailures, &after.Detail)
	if after.Phase == model.PhaseReady && after.Restarts > 0 && !after.ReadySince.IsZero() &&
		now.Sub(after.ReadySince) >= c.t.crashWindow {
		after.Restarts = 0
	}
}

// writeAttached writes what the supervisor changed of an attached
// service onto the row as it is now.
func (c *Controller) writeAttached(ctx context.Context, before, after store.EnvAttached) {
	if before == after {
		return
	}
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockAttached(ctx, before.EnvID, before.Service, store.ForUpdate)
		if err != nil {
			return err
		}
		if cur.SandboxID != before.SandboxID {
			return nil // moved meanwhile
		}
		next := cur
		next.SandboxIncarnation, next.TemplateHash, next.ProcessID = after.SandboxIncarnation, after.TemplateHash, after.ProcessID
		next.Phase, next.ReadySince, next.Restarts, next.LastExit = after.Phase, after.ReadySince, after.Restarts, after.LastExit
		next.ProbeFailures, next.NextRestartAt, next.HostsWritten, next.Detail =
			after.ProbeFailures, after.NextRestartAt, after.HostsWritten, after.Detail
		if _, err := tx.UpdateAttached(ctx, cur, next); err != nil {
			return err
		}
		if cur.Phase != next.Phase {
			return tx.Notify(ctx, db.ChanLeader, HintReconcile)
		}
		return nil
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("attached", "cannot write an attached service", "service", before.Service, "err", err)
	}
}
