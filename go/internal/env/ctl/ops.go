// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/tools"
)

// The answers of the API's calls into the controller: on any instance,
// not fenced; a write wakes the leader's env-reconcile.

// maxLogLines bounds a LogTail.
const maxLogLines = 10000

// LogTail returns the last lines lines of an object's log, oldest first,
// and their source: the main container's log of replica replica of an
// image service, of an addon (replica 0), or of a job's latest run (its
// stored tail once the run's row is gone) (SourceContainer); the process
// log of an attached service (SourceProcess). It never wakes an instance
// (ErrPaused); ErrNoObject, ErrNoContainerLogs.
func (c *Controller) LogTail(ctx context.Context, envID uuid.UUID, kind, name string, replica, lines int) ([]string, string, error) {
	if lines <= 0 {
		lines = 200
	}
	lines = min(lines, maxLogLines)
	v, err := c.load(ctx, envID)
	if errors.Is(err, store.ErrNoRow) || (err == nil && v == nil) {
		return nil, "", ErrNoObject
	}
	if err != nil {
		return nil, "", err
	}
	switch kind {
	case spec.KindService:
		svc, ok := v.target.Services[name]
		if !ok {
			return nil, "", ErrNoObject
		}
		if runtimeOf(svc) == spec.RuntimeAttached {
			return c.processLog(ctx, v, name, lines)
		}
	case spec.KindAddon:
		if _, ok := v.target.Addons[name]; !ok || replica != 0 {
			return nil, "", ErrNoObject
		}
	case spec.KindJob:
		if _, ok := v.target.Jobs[name]; !ok {
			return nil, "", ErrNoObject
		}
		return c.jobLog(ctx, v, name, lines)
	default:
		return nil, "", ErrNoObject
	}
	var row *store.EnvInstanceRow
	for _, r := range v.rowsOf(kind, name) {
		if r.Instance.Replica != replica {
			continue
		}
		if row == nil || (row.Instance.Phase == model.PhaseRetiring && r.Instance.Phase != model.PhaseRetiring) ||
			r.Instance.CreatedAt.After(row.Instance.CreatedAt) {
			r := r
			row = &r
		}
	}
	if row == nil {
		return nil, "", ErrNoObject
	}
	out, err := c.instanceLog(ctx, row.Sandbox, lines)
	return out, SourceContainer, err
}

// instanceLog reads a running instance's container log; a paused or
// hibernated one is ErrPaused (never woken).
func (c *Controller) instanceLog(ctx context.Context, sb store.Sandbox, lines int) ([]string, error) {
	switch {
	case sb.ClientState == model.StatePaused || sb.ClientState == model.StateHibernated ||
		sb.ClientState == model.StatePausing || sb.ClientState == model.StateHibernating:
		return nil, ErrPaused
	case sb.ClientState != model.StateRunning || sb.Node == "":
		return nil, fmt.Errorf("the instance is %s: no log to read", sb.ClientState)
	}
	return c.containerLog(ctx, lease(sb), lines)
}

// jobLog is a job's latest run's log: its container's while its row runs,
// else the tail its run kept.
func (c *Controller) jobLog(ctx context.Context, v *view, name string, lines int) ([]string, string, error) {
	run, err := c.st.LatestJobRun(ctx, v.env.ID, name)
	if errors.Is(err, store.ErrNoRow) {
		return []string{}, SourceContainer, nil
	}
	if err != nil {
		return nil, "", err
	}
	if run.SandboxID != uuid.Nil && run.Phase == model.RunRunning {
		if sb, err := c.st.GetSandbox(ctx, run.SandboxID); err == nil && sb.ClientState == model.StateRunning {
			out, err := c.instanceLog(ctx, sb, lines)
			return out, SourceContainer, err
		}
	}
	tail := run.LogTail
	if len(tail) > lines {
		tail = tail[len(tail)-lines:]
	}
	return slices.Clone(tail), SourceContainer, nil
}

// processLog is an attached service's process log.
func (c *Controller) processLog(ctx context.Context, v *view, name string, lines int) ([]string, string, error) {
	a, ok := v.attached[name]
	if !ok || a.ProcessID == "" {
		return []string{}, SourceProcess, nil
	}
	sb, err := c.st.GetSandbox(ctx, a.SandboxID)
	if err != nil {
		return nil, "", err
	}
	switch {
	case sb.ClientState == model.StatePaused || sb.ClientState == model.StateHibernated ||
		sb.ClientState == model.StatePausing || sb.ClientState == model.StateHibernating:
		return nil, "", ErrPaused
	case !steady(sb):
		return nil, "", fmt.Errorf("sandbox %s is %s: no log to read", sb.ID, sb.ClientState)
	case c.d.Runner == nil:
		return nil, "", errors.New("process logs need the tools' runner, which this instance lacks")
	}
	out, err := c.d.Runner.ProcessLogsDetached(ctx, lease(sb), a.ProcessID, lines)
	if errors.Is(err, tools.ErrNoProcess) {
		return []string{}, SourceProcess, nil
	}
	return out, SourceProcess, err
}

// RestartService restarts a service of the environment's target release:
// an attached service's process is stopped and started again (never its
// sandbox's pod); an image service's replicas are restarted one at a
// time. It returns once the restart is under way; ErrNoObject.
func (c *Controller) RestartService(ctx context.Context, envID uuid.UUID, name string) error {
	v, err := c.load(ctx, envID)
	if errors.Is(err, store.ErrNoRow) || (err == nil && v == nil) {
		return ErrNoObject
	}
	if err != nil {
		return err
	}
	svc, ok := v.target.Services[name]
	if !ok {
		return ErrNoObject
	}
	if runtimeOf(svc) == spec.RuntimeAttached {
		return c.restartAttached(ctx, v, name)
	}
	var rows []store.EnvInstanceRow
	for _, r := range v.rowsOf(spec.KindService, name) {
		if r.Instance.Phase != model.PhaseRetiring {
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, byReplica)
	if len(rows) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.Sandbox.ID
	}
	// The first restart begins now; the others follow one at a time, each
	// once the one before is ready again.
	if err := c.restartReplica(ctx, ids[0]); err != nil {
		return err
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for i := 1; i < len(ids); i++ {
			if !c.waitReady(c.bg, ids[i-1]) {
				return
			}
			if err := c.restartReplica(c.bg, ids[i]); err != nil && c.bg.Err() == nil {
				c.rl.warn("restart", "cannot restart a replica", "sandbox", ids[i], "err", err)
				return
			}
		}
	}()
	return nil
}

// restartReplica restarts one replica's pod: its phase is starting again
// (no traffic until it is ready), then Engine.Restart. A row that is not
// running is left to the controller (Ensure brings it up anew).
func (c *Controller) restartReplica(ctx context.Context, id uuid.UUID) error {
	err := c.st.InTx(ctx, func(tx *store.Store) error {
		inst, err := tx.LockEnvInstance(ctx, id, store.ForUpdate)
		if err != nil {
			return err
		}
		after := inst
		after.Phase, after.ReadySince, after.ProbeFailures, after.Detail = model.PhaseStarting, time.Time{}, 0, "restarting"
		_, err = tx.UpdateEnvInstance(ctx, inst, after)
		return err
	})
	if err != nil {
		return err
	}
	err = c.d.Engine.Restart(ctx, id, "restart requested")
	if errors.Is(err, model.ErrPrecondition) {
		return nil
	}
	return err
}

// waitReady waits until an instance is ready again (true), or gone, or
// restartReady passed (false).
func (c *Controller) waitReady(ctx context.Context, id uuid.UUID) bool {
	deadline := time.Now().Add(c.t.restartReady)
	for time.Now().Before(deadline) {
		inst, err := c.st.GetEnvInstance(ctx, id)
		if err != nil {
			return false
		}
		if inst.Phase == model.PhaseReady {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

// restartAttached stops an attached service's process (when its sandbox
// runs) and has the supervisor start it again at once.
func (c *Controller) restartAttached(ctx context.Context, v *view, name string) error {
	a, ok := v.attached[name]
	if !ok {
		return nil
	}
	if sb, err := c.st.GetSandbox(ctx, a.SandboxID); err == nil && steady(sb) && a.ProcessID != "" && c.d.Runner != nil {
		if err := c.d.Runner.StopProcessDetached(ctx, lease(sb), a.ProcessID); err != nil && !errors.Is(err, tools.ErrNoProcess) {
			return err
		}
	}
	return c.st.InTx(ctx, func(tx *store.Store) error {
		cur, err := tx.LockAttached(ctx, v.env.ID, name, store.ForUpdate)
		if err != nil {
			return err
		}
		after := cur
		after.ProcessID, after.Phase, after.ReadySince, after.ProbeFailures = "", model.PhaseStarting, time.Time{}, 0
		after.NextRestartAt, after.Detail = time.Time{}, "restarting"
		if _, err := tx.UpdateAttached(ctx, cur, after); err != nil {
			return err
		}
		return tx.Notify(ctx, db.ChanLeader, HintReconcile)
	})
}
