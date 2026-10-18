// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/env/addons"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// stepAddons brings the addons of T up (§4.3 step 2): credentials,
// volume, instance, recreate, provisioning.
func (c *Controller) stepAddons(ctx context.Context, s *stepState) {
	s.addons = map[string]*addonPlan{}
	for _, name := range slices.Sorted(maps.Keys(s.v.target.Addons)) {
		if !c.stepAddon(ctx, s, name, s.v.target.Addons[name]) {
			s.addonsReady = false
		}
	}
}

// stepAddon brings one addon up and reports whether it is ready and
// provisioned.
func (c *Controller) stepAddon(ctx context.Context, s *stepState, name string, a spec.Addon) bool {
	v := s.v
	obj := objectName(spec.KindAddon, name)
	bp, ok := c.d.Blueprint(a.Type)
	if !ok {
		s.wait("addon %s: no addon type %q", name, a.Type)
		return false
	}
	addon := specAddon(bp, name, a)
	if err := c.ensureCredentials(ctx, v.env.ID, name, bp.Credentials(addon)); err != nil {
		if errors.Is(err, errNoSealer) {
			s.reason = errNoSealer.Error()
		}
		s.wait("addon %s: its credentials: %v", name, err)
		return false
	}
	inst, err := bp.Render(addon, addons.Context{Env: v.env.Name, Name: name})
	if err != nil {
		s.wait("addon %s: %v", name, err)
		return false
	}
	var vol *store.EnvVolume
	if inst.Mount != "" {
		got, err := c.addonVolume(ctx, v, bp, addon)
		if err != nil {
			s.wait("addon %s: its volume: %v", name, err)
			return false
		}
		if got.Deleting {
			s.wait("addon %s: its old volume is being deleted", name)
			return false
		}
		vol = &got
	}
	cur, hasCur := v.addonRow(name)
	p := c.planAddon(v, name, a, vol, cur.Sandbox.Address)
	s.addons[name] = p
	if !p.ok() {
		s.wait("addon %s: %s", name, p.why())
		return false
	}
	if !hasCur {
		c.newAddon(ctx, s, p, vol)
		return false
	}
	if cur.Instance.TemplateHash != p.hash {
		c.recreateAddon(ctx, s, p, cur, vol)
		return false
	}
	c.keep(ctx, v, cur, 0)
	sb := cur.Sandbox
	if c.heal(ctx, cur, false) {
		// Never replaced: its volume is on the lost node. Ensure syncs it
		// when the node returns.
		c.ensure(ctx, sb.ID)
		if !v.isOpen(obj, model.ConditionVolumeUnavailable) {
			c.incident(ctx, v.env.ID, obj, v.targetNum, model.ConditionVolumeUnavailable,
				fmt.Sprintf("addon %s is lost with node %s, which has its volume", name, sb.LostFrom), evidence{})
		}
		s.wait("addon %s is lost", name)
		return false
	}
	if vol != nil && vol.Provisioned && steady(sb) && (vol.Node != sb.Node || sb.PinNode != sb.Node) {
		// Restored elsewhere: its volume came along.
		c.moveVolume(ctx, v, *vol, sb)
	}
	if cur.Instance.Phase != model.PhaseReady {
		s.wait("addon %s is %s", name, cur.Instance.Phase)
		return false
	}
	c.closeIncidents(ctx, v, obj, model.ConditionVolumeUnavailable)
	if cur.Instance.ProvisionedHash != p.stepsHash {
		c.provision(ctx, v, p, cur)
		s.wait("addon %s is being provisioned", name)
		return false
	}
	return true
}

// addonVolume reads the addon's volume row, recording it with the plan's
// size at its first creation (never resized).
func (c *Controller) addonVolume(ctx context.Context, v *view, bp addons.Blueprint, a addons.Addon) (store.EnvVolume, error) {
	object := volumeObject(a.Name)
	if vol, ok := v.volumes[object]; ok {
		return vol, nil
	}
	plan, ok := bp.Plans()[a.Plan]
	if !ok {
		return store.EnvVolume{}, fmt.Errorf("no plan %q", a.Plan)
	}
	var out store.EnvVolume
	err := c.fenced(ctx, func(tx *store.Store) error {
		vol, err := tx.InsertEnvVolume(ctx, store.EnvVolume{EnvID: v.env.ID, Object: object, VolumeID: uuid.New(),
			SizeMiB: plan.VolumeMib})
		if errors.Is(err, store.ErrExists) {
			vol, err = tx.GetEnvVolume(ctx, v.env.ID, object)
		}
		out = vol
		return err
	})
	if err != nil {
		return store.EnvVolume{}, err
	}
	v.volumes[object] = out
	return out, nil
}

// newAddon inserts an addon's instance: pinned to its volume's node once
// the volume was created there, and only once the pod that held the
// volume last is gone from its node (no row, no tombstone): a volume is
// attached to one pod at a time.
func (c *Controller) newAddon(ctx context.Context, s *stepState, p *addonPlan, vol *store.EnvVolume) {
	v := s.v
	for _, r := range v.rowsOf(spec.KindAddon, p.name) {
		if r.Instance.Phase == model.PhaseRetiring {
			s.wait("addon %s: its old instance is being deleted", p.name)
			return
		}
	}
	pin := ""
	if vol != nil {
		if vol.Holder != uuid.Nil {
			if held, err := c.stillHolds(ctx, vol.Holder); err != nil || held {
				s.wait("addon %s: its old pod still holds its volume", p.name)
				return
			}
		}
		if vol.Provisioned {
			pin = vol.Node
		}
	}
	if _, err := c.insertInstance(ctx, v, insert{plan: &p.podPlan, release: v.targetNum, pin: pin}); err != nil {
		s.wait("addon %s: %v", p.name, err)
		return
	}
	s.wait("addon %s is starting", p.name)
}

// stillHolds reports whether a row, or a tombstone of its pod, is left:
// its pod may still hold a volume.
func (c *Controller) stillHolds(ctx context.Context, id uuid.UUID) (bool, error) {
	_, err := c.st.GetSandbox(ctx, id)
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, model.ErrNotFound):
		return false, err
	}
	ts, err := c.st.TombstonesOfPod(ctx, id)
	return len(ts) > 0, err
}

// recreateAddon replaces an addon whose template changed (a version, a
// plan, a rendered option): only while its instance runs on a ready node
// (else that instance is brought up first, a restore); the volume
// records the node and the old row as its holder, the old row goes, and
// newAddon inserts the new one on that node once the old pod is gone.
func (c *Controller) recreateAddon(ctx context.Context, s *stepState, p *addonPlan, cur store.EnvInstanceRow, vol *store.EnvVolume) {
	v := s.v
	sb := cur.Sandbox
	if c.heal(ctx, cur, false) {
		c.ensure(ctx, sb.ID)
		s.wait("addon %s is lost; it is recreated once it runs again", p.name)
		return
	}
	if !steady(sb) || !c.nodeReady(ctx, sb.Node) {
		if sb.Op == "" && sb.ClientState != model.StateRunning {
			c.ensure(ctx, sb.ID)
		}
		s.wait("addon %s: waits for its instance to run before it is recreated", p.name)
		return
	}
	err := c.fenced(ctx, func(tx *store.Store) error {
		inst, err := tx.LockEnvInstance(ctx, sb.ID, store.ForUpdate)
		if err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		after := inst
		after.Phase, after.RetireAt = model.PhaseRetiring, now
		if _, err := tx.UpdateEnvInstance(ctx, inst, after); err != nil {
			return err
		}
		if vol == nil {
			return nil
		}
		vv, err := tx.LockEnvVolume(ctx, v.env.ID, vol.Object, store.ForUpdate)
		if err != nil {
			return err
		}
		va := vv
		va.Holder = sb.ID
		if vv.Provisioned {
			va.Node = sb.Node
		}
		_, err = tx.UpdateEnvVolume(ctx, vv, va)
		return err
	})
	if err != nil {
		s.wait("addon %s: %v", p.name, err)
		return
	}
	c.log.Info("addon recreated", "environment", v.env.ID, "addon", p.name, "old", sb.ID, "node", sb.Node)
	if err := c.deleteInstance(ctx, sb); err != nil && ctx.Err() == nil {
		c.rl.warn("recreate", "cannot delete an addon's old instance", "sandbox", sb.ID, "err", err)
	}
	s.wait("addon %s is being recreated", p.name)
}

// moveVolume records that an addon's volume is on the node its instance
// runs on now (a restore elsewhere took it along), and pins the row there.
func (c *Controller) moveVolume(ctx context.Context, v *view, vol store.EnvVolume, sb store.Sandbox) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockEnvVolume(ctx, v.env.ID, vol.Object, store.ForUpdate)
		if err != nil {
			return err
		}
		after := cur
		after.Node = sb.Node
		if _, err := tx.UpdateEnvVolume(ctx, cur, after); err != nil {
			return err
		}
		_, err = tx.EnvCtlSetPin(ctx, sb.ID, sb.Node)
		return err
	})
	if err != nil && ctx.Err() == nil {
		c.rl.warn("volume", "cannot record a volume's node", "environment", v.env.ID, "object", vol.Object, "err", err)
		return
	}
	c.log.Info("addon volume moved", "environment", v.env.ID, "object", vol.Object, "node", sb.Node)
}

// provisionKey is the pool's key of an instance's provisioning.
func provisionKey(id uuid.UUID) string { return "provision/" + id.String() }

// provision runs an addon's provisioning steps in its container, off the
// loop: hold-free, the env carrying its credentials; on success the steps'
// hash is stored, on a failure the step's output (credentials redacted)
// is the instance's detail and the steps run again after provisionRetry.
func (c *Controller) provision(ctx context.Context, v *view, p *addonPlan, r store.EnvInstanceRow) {
	if c.d.Runner == nil {
		c.setDetail(ctx, r.Sandbox.ID, "provisioning needs the tools' runner, which this instance lacks")
		return
	}
	c.mu.Lock()
	failed, wait := c.provisionFailed[r.Sandbox.ID]
	c.mu.Unlock()
	if wait && v.now.Sub(failed) < c.t.provisionRetry && v.now.After(failed) {
		return
	}
	envID, l := v.env.ID, lease(r.Sandbox)
	steps, hash, name, keys := p.steps, p.stepsHash, p.name, p.credentials
	c.pool.run(ctx, provisionKey(r.Sandbox.ID), func(ctx context.Context) {
		secrets := c.credentialValues(ctx, envID, name, keys)
		for _, st := range steps {
			env, err := c.resolveEnv(ctx, envID, st.Env)
			if err != nil {
				c.provisionDone(ctx, r.Sandbox.ID, "", fmt.Sprintf("provisioning step %s: %v", st.Name, err))
				return
			}
			code, stdout, stderr, err := c.d.Runner.ExecDetached(ctx, l, st.Argv, env.vars, "/", c.t.stepTimeout)
			out := redact(strings.TrimSpace(stderr+"\n"+stdout), append(secrets, env.secrets...))
			switch {
			case err != nil:
				c.provisionDone(ctx, r.Sandbox.ID, "", fmt.Sprintf("provisioning step %s: %s", st.Name,
					redact(err.Error(), append(secrets, env.secrets...))))
				return
			case code != 0:
				c.provisionDone(ctx, r.Sandbox.ID, "", fmt.Sprintf("provisioning step %s exited %d: %s", st.Name, code,
					lastLines(out, 5)))
				return
			}
		}
		c.provisionDone(ctx, r.Sandbox.ID, hash, "")
		c.log.Info("addon provisioned", "environment", envID, "addon", name, "sandbox", r.Sandbox.ID)
	})
}

// provisionDone stores a provisioning's outcome: the steps' hash, or why
// it failed.
func (c *Controller) provisionDone(ctx context.Context, id uuid.UUID, hash, detail string) {
	c.mu.Lock()
	if hash == "" {
		c.provisionFailed[id] = c.dbNow(ctx)
	} else {
		delete(c.provisionFailed, id)
	}
	c.mu.Unlock()
	err := c.fenced(ctx, func(tx *store.Store) error {
		inst, err := tx.LockEnvInstance(ctx, id, store.ForUpdate)
		if err != nil {
			return err
		}
		after := inst
		after.Detail = detail
		if hash != "" {
			after.ProvisionedHash = hash
		}
		if _, err := tx.UpdateEnvInstance(ctx, inst, after); err != nil {
			return err
		}
		return tx.Notify(ctx, db.ChanLeader, HintReconcile)
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("provision", "cannot store a provisioning's outcome", "sandbox", id, "err", err)
	}
}

// setDetail writes an instance's detail.
func (c *Controller) setDetail(ctx context.Context, id uuid.UUID, detail string) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		inst, err := tx.LockEnvInstance(ctx, id, store.ForUpdate)
		if err != nil {
			return err
		}
		after := inst
		after.Detail = detail
		_, err = tx.UpdateEnvInstance(ctx, inst, after)
		return err
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("detail", "cannot write an instance's detail", "sandbox", id, "err", err)
	}
}

// dbNow is the database's time, or the instance's when it cannot be read.
func (c *Controller) dbNow(ctx context.Context) time.Time {
	if t, err := c.st.Now(ctx); err == nil {
		return t
	}
	return time.Now()
}

// lastLines is the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
