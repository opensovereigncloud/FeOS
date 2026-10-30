// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
	"unicode/utf8"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A tenant's run (ADR 0018, "A tenant's run") is a job's run without an
// environment: its pod is a row of sandboxes under the run's ID
// (tenant_run), whose template the create renders, its secrets and image
// credentials sealed in the run's row (runs.sealed) and resolved by
// RowSecret. env-probe ends the runs that have not ended as it ends a
// job's runs (exitOf, outcome, keptTail, deleteInstance): an exit (its
// result file read first), the deadline, a lost node, a pod that went or
// stopped once it had run. A create, a cancel and a delete run on any
// instance; env-probe also deletes the pods a run's end left (LeftRunPods)
// and env-delete sweeps the runs that ended RunRetention ago.

// RunRetention is how long a run's record stays after its end.
const RunRetention = 7 * 24 * time.Hour

// runTailLines is how many lines of its log a run keeps (within
// storedLog's 64 KiB).
const runTailLines = 1000

// MaxRunResult is how much of its result file a run keeps.
const MaxRunResult = 4 << 10

// The names a run's image credentials have among its sealed secrets: no
// environment variable's name has a dot.
const (
	runCredUser     = "imageCredentials.username"
	runCredPassword = "imageCredentials.password"
)

// ErrRunKeyConflict: the create's Idempotency-Key names a run of the
// tenant made with another body (422).
var ErrRunKeyConflict = errors.New("the Idempotency-Key names a run made with another request")

// ErrRunImage: the run's image does not resolve with its credentials, the
// registry's answer in the text (422).
var ErrRunImage = errors.New("the image does not resolve")

// RunSpec is a run's create, validated by the caller.
type RunSpec struct {
	Image            string
	Command          []string
	Env, Secrets     map[string]string
	ImageCredentials *model.RegistryCredentials
	VCPUs, MemoryMiB int
	Deadline         time.Duration
	ResultPath       string
	Labels           map[string]string
	// Key is the create's Idempotency-Key ("": none), Hash its body's
	// hash, which a repeat with the key must have.
	Key, Hash string
}

// runAAD is the AAD of a run's sealed secrets.
func runAAD(id podid.ID) []byte { return []byte("runs|" + id.String()) }

// CreateRun makes a run of owner: its image pinned with its credentials,
// its pod's row and its record in one transaction; env-probe brings the
// pod up. A create with a key that a run of owner was made with answers
// with that run (false) when its hash is the run's, else
// ErrRunKeyConflict.
func (c *Controller) CreateRun(ctx context.Context, owner string, rs RunSpec) (store.Run, bool, error) {
	if r, ok, err := c.runOfKey(ctx, owner, rs); ok || err != nil {
		return r, false, err
	}
	secrets := maps.Clone(rs.Secrets)
	if rs.ImageCredentials != nil {
		if secrets == nil {
			secrets = map[string]string{}
		}
		secrets[runCredUser], secrets[runCredPassword] = rs.ImageCredentials.Username, rs.ImageCredentials.Password
	}
	if len(secrets) > 0 && c.d.Sealer == nil {
		return store.Run{}, false, errNoSealer
	}
	var auth model.RegistryCredentials
	if rs.ImageCredentials != nil {
		auth = *rs.ImageCredentials
	}
	ref, err := c.d.Engine.ResolveImage(ctx, rs.Image, auth)
	if err != nil {
		if ctx.Err() != nil {
			return store.Run{}, false, ctx.Err()
		}
		return store.Run{}, false, &stateError{msg: fmt.Sprintf("image %s: %v", rs.Image, err), kind: ErrRunImage}
	}
	tmpl := model.PodTemplate{Command: slices.Clone(rs.Command)}
	if len(rs.Env)+len(rs.Secrets) > 0 {
		tmpl.Env = map[string]model.TemplateValue{}
		for k, v := range rs.Env {
			tmpl.Env[k] = model.TemplateValue{Value: v}
		}
		for k := range rs.Secrets {
			tmpl.Env[k] = model.TemplateValue{Secret: k}
		}
	}
	if rs.ImageCredentials != nil {
		tmpl.ImageCredentials = &model.TemplateCredentials{Username: model.TemplateValue{Secret: runCredUser},
			Password: model.TemplateValue{Secret: runCredPassword}}
	}
	idle := 0
	var run store.Run
	err = c.st.InTx(ctx, func(tx *store.Store) error {
		t, err := tx.LockTenant(ctx, owner, store.ForShare)
		if err != nil {
			return err
		}
		if t.Deleting() {
			return errTenantDeleting
		}
		row, err := tx.CreateMintedSandbox(ctx, func(id podid.ID) store.NewSandbox {
			return store.NewSandbox{ID: id, Owner: owner, Image: rs.Image, VCPUs: rs.VCPUs, MemoryMiB: rs.MemoryMiB,
				ImageRef: ref, PodTemplate: &tmpl, IdleAfterS: &idle, TenantRun: true}
		})
		if err != nil {
			return err
		}
		var sealed []byte
		if len(secrets) > 0 {
			plain, err := json.Marshal(secrets)
			if err != nil {
				return err
			}
			if sealed, err = c.d.Sealer.Seal(plain, runAAD(row.ID)); err != nil {
				return fmt.Errorf("seal the run's secrets: %w", err)
			}
		}
		run, err = tx.InsertRun(ctx, store.Run{ID: row.ID, Owner: owner, Key: rs.Key, RequestHash: rs.Hash,
			Image: rs.Image, ImageRef: ref, VCPUs: rs.VCPUs, MemoryMiB: rs.MemoryMiB, ResultPath: rs.ResultPath,
			Labels: rs.Labels}, sealed, rs.Deadline)
		return err
	})
	if errors.Is(err, store.ErrExists) && rs.Key != "" {
		// A create with the same key won the race.
		if r, ok, err := c.runOfKey(ctx, owner, rs); ok || err != nil {
			return r, false, err
		}
	}
	if err != nil {
		return store.Run{}, false, err
	}
	c.log.Info("run created", "tenant", owner, "run", run.ID, "image", ref)
	return run, true, nil
}

// runOfKey is the run of owner a create with rs's key made: ok when there
// is one with rs's hash, ErrRunKeyConflict when its hash differs.
func (c *Controller) runOfKey(ctx context.Context, owner string, rs RunSpec) (store.Run, bool, error) {
	if rs.Key == "" {
		return store.Run{}, false, nil
	}
	r, err := c.st.RunByKey(ctx, owner, rs.Key)
	switch {
	case errors.Is(err, store.ErrNoRow):
		return store.Run{}, false, nil
	case err != nil:
		return store.Run{}, false, err
	case r.RequestHash != rs.Hash:
		return store.Run{}, false, ErrRunKeyConflict
	}
	return r, true, nil
}

// RowSecret is the engine's secret resolver (sandbox.SecretResolver): a
// secret of the row's environment (ResolveSecret), or of its run for a
// tenant's run's pod, whose secrets have no versions.
func (c *Controller) RowSecret(ctx context.Context, row store.Sandbox, name string, version int) (string, error) {
	if !row.TenantRun {
		return c.ResolveSecret(ctx, row.EnvironmentID, name, version)
	}
	secrets, err := c.runSecrets(ctx, row.ID)
	if err != nil {
		return "", err
	}
	v, ok := secrets[name]
	if !ok {
		return "", fmt.Errorf("run %s has no secret %s", row.ID, name)
	}
	return v, nil
}

// runSecrets are a run's secrets and image credentials, unsealed; an
// error once it has ended (they are dropped then).
func (c *Controller) runSecrets(ctx context.Context, id podid.ID) (map[string]string, error) {
	if c.d.Sealer == nil {
		return nil, errNoSealer
	}
	sealed, err := c.st.RunSealed(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", id, err)
	}
	if sealed == nil {
		return nil, fmt.Errorf("run %s has no secrets (any more)", id)
	}
	plain, err := c.d.Sealer.Open(sealed, runAAD(id))
	if err != nil {
		return nil, fmt.Errorf("run %s: open its secrets: %w", id, err)
	}
	var out map[string]string
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, fmt.Errorf("run %s: its secrets: %w", id, err)
	}
	return out, nil
}

// runSecretValues are the values a run's log is redacted of: its secrets
// and image credentials, none when they cannot be read (a run without
// any, or one that has ended).
func (c *Controller) runSecretValues(ctx context.Context, id podid.ID) []string {
	secrets, err := c.runSecrets(ctx, id)
	if err != nil {
		return nil
	}
	return slices.Sorted(maps.Values(secrets))
}

// probeRuns is env-probe's pass over the runs that have not ended, each
// in run (false: the pass ends), then over the pods their ends left.
func (c *Controller) probeRuns(ctx context.Context, run func(func()) bool) bool {
	runs, err := c.st.OpenRuns(ctx)
	if err != nil {
		c.rl.warn("runs", "cannot read the runs", "err", err)
		return true
	}
	now := c.dbNow(ctx)
	for _, r := range runs {
		if !run(func() { c.stepRun(ctx, r, now) }) {
			return false
		}
	}
	left, err := c.st.LeftRunPods(ctx)
	if err != nil {
		c.rl.warn("runs", "cannot read the pods of ended runs", "err", err)
		return true
	}
	for _, sb := range left {
		if err := c.deleteInstance(ctx, sb); err != nil && ctx.Err() == nil {
			c.rl.warn("runs", "cannot delete an ended run's pod", "run", sb.ID, "err", err)
		}
	}
	return true
}

// stepRun is one look at a run that has not ended, at now (the
// database's): its end when its deadline passed, its pod went, was lost,
// or stopped after it ran, or its process exited; else its start once
// its container runs, or its pod brought up.
func (c *Controller) stepRun(ctx context.Context, r store.Run, now time.Time) {
	sb, err := c.st.GetSandbox(ctx, r.ID)
	if errors.Is(err, model.ErrNotFound) {
		c.endRun(ctx, r, nil, store.RunEnd{Phase: model.RunFailed, Reason: model.RunReasonNoExitStatus,
			Detail: "its pod is gone"}, true)
		return
	}
	if err != nil {
		c.rl.warn("runs", "cannot read a run's pod", "run", r.ID, "err", err)
		return
	}
	switch {
	case now.After(r.DeadlineAt):
		c.endRun(ctx, r, &sb, store.RunEnd{Phase: model.RunDeadlineExceeded,
			Detail: "its deadline of " + spell(r.DeadlineAt.Sub(r.CreatedAt)) + " passed"}, true)
		return
	case sb.Op == "" && sb.ClientState == model.StateLost:
		c.endRun(ctx, r, &sb, store.RunEnd{Phase: model.RunFailed, Reason: model.RunReasonNodeLost,
			Detail: "its pod was lost with node " + lostNode(sb)}, true)
		return
	case steady(sb) && c.connected(sb.Node):
		info, err := c.getPod(ctx, sb.Node, sb.ID)
		if err != nil || info.GetState() != podv1.PodState_POD_STATE_RUNNING {
			return
		}
		mc := mainContainer(info)
		if exited, code := exitOf(mc); exited {
			c.exitedRun(ctx, r, sb, code)
			return
		}
		if r.Phase == model.RunQueued && mc.GetState() == podv1.ContainerState_CONTAINER_STATE_RUNNING {
			if _, err := c.st.StartRun(ctx, r.ID); err != nil && ctx.Err() == nil {
				c.rl.warn("runs", "cannot mark a run running", "run", r.ID, "err", err)
			}
		}
		return
	case !r.StartedAt.IsZero() && sb.Op == "" &&
		(sb.ClientState == model.StateStopped || sb.ClientState == model.StateFailed):
		// A run is never run again: a pod that stopped once its process
		// ran is its end.
		c.endRun(ctx, r, &sb, store.RunEnd{Phase: model.RunFailed, Reason: model.RunReasonNoExitStatus,
			Detail: "its pod " + sb.ClientState + " before its process exited"}, false)
		return
	}
	c.healThen(ctx, sb, false, func(ctx context.Context, err error) {
		if errors.Is(err, model.ErrImageRefused) {
			c.endRun(ctx, r, &sb, store.RunEnd{Phase: model.RunFailed, Reason: model.RunReasonImage,
				Detail: err.Error()}, false)
		}
	})
}

// exitedRun ends a run whose process exited with code (nil: the node
// reported none): succeeded for 0, failed (exit) for another code, failed
// (no_exit_status) for none; its result file read first.
func (c *Controller) exitedRun(ctx context.Context, r store.Run, sb store.Sandbox, code *int) {
	succeeded, detail := outcome(code, "")
	end := store.RunEnd{Phase: model.RunFailed, Reason: model.RunReasonExit, ExitCode: code, Detail: detail, Exited: true}
	switch {
	case succeeded:
		end.Phase, end.Reason = model.RunSucceeded, ""
	case code == nil:
		end.Reason = model.RunReasonNoExitStatus
	}
	if r.ResultPath != "" {
		result, err := c.readResult(ctx, sb, r.ResultPath)
		if err != nil {
			end.Detail = "its result " + r.ResultPath + " cannot be read: " + err.Error()
		}
		end.Result = redact(result, c.runSecretValues(ctx, r.ID))
	}
	c.endRun(ctx, r, &sb, end, true)
}

// readResult reads a run's result file, cut to MaxRunResult at a rune's
// start.
func (c *Controller) readResult(ctx context.Context, sb store.Sandbox, path string) (string, error) {
	if c.d.Runner == nil {
		return "", errors.New("this instance has no tools' runner")
	}
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	s, err := c.d.Runner.ReadDetached(ctx, lease(sb), path)
	if err != nil {
		return "", err
	}
	if len(s) > MaxRunResult {
		n := MaxRunResult
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return s, nil
}

// endRun ends a run as end says, with the tail of its pod's log when
// fromPod and the pod runs (read before the end drops its secrets), and
// deletes its pod (sb; nil: it has none). The end is written once: a run
// another call ended keeps that end, and its pod is deleted all the same.
func (c *Controller) endRun(ctx context.Context, r store.Run, sb *store.Sandbox, end store.RunEnd, fromPod bool) {
	if sb != nil && fromPod && steady(*sb) && c.connected(sb.Node) {
		end.LogTail = c.keptTail(ctx, *sb, runTailLines, func() []string { return c.runSecretValues(ctx, r.ID) })
	}
	ended, err := c.st.EndRun(ctx, r.ID, end)
	if err != nil {
		if ctx.Err() == nil {
			c.rl.warn("runs", "cannot end a run", "run", r.ID, "err", err)
		}
		return
	}
	if ended {
		c.log.Info("run ended", "tenant", r.Owner, "run", r.ID, "phase", end.Phase, "reason", end.Reason,
			"detail", end.Detail)
	}
	if sb == nil {
		return
	}
	// The pod's delete is a mutating call: never on a caller's context.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := c.deleteInstance(dctx, *sb); err != nil {
		c.rl.warn("runs", "cannot delete an ended run's pod (env-probe tries again)", "run", r.ID, "err", err)
	}
}

// CancelRun ends a run of owner that has not ended as cancelled, its
// log's tail kept and its pod deleted, and returns it; a run that has
// ended is returned as it is. ErrNoObject: owner has no such run.
func (c *Controller) CancelRun(ctx context.Context, owner string, id podid.ID) (store.Run, error) {
	r, err := c.st.GetRun(ctx, owner, id)
	if errors.Is(err, store.ErrNoRow) {
		return store.Run{}, ErrNoObject
	}
	if err != nil || r.Ended() {
		return r, err
	}
	var sbp *store.Sandbox
	if sb, err := c.st.GetSandbox(ctx, id); err == nil {
		sbp = &sb
	} else if !errors.Is(err, model.ErrNotFound) {
		return store.Run{}, err
	}
	c.endRun(ctx, r, sbp, store.RunEnd{Phase: model.RunCancelled, Detail: "cancelled"}, true)
	return c.st.GetRun(ctx, owner, id)
}

// DeleteRun cancels a run of owner that has not ended and removes its
// record. ErrNoObject: owner has no such run.
func (c *Controller) DeleteRun(ctx context.Context, owner string, id podid.ID) error {
	r, err := c.CancelRun(ctx, owner, id)
	if err != nil {
		return err
	}
	if !r.Ended() {
		return fmt.Errorf("run %s did not end", id)
	}
	if ok, err := c.st.DeleteRun(ctx, owner, id); err != nil {
		return err
	} else if !ok {
		return ErrNoObject
	}
	return nil
}

// RunLog is the last lines lines of a run's log: read from its pod while
// it runs, redacted of its secrets; the kept tail once it has ended; none
// while its pod does not run yet. ErrNoObject: owner has no such run.
func (c *Controller) RunLog(ctx context.Context, owner string, id podid.ID, lines int) ([]string, error) {
	r, err := c.st.GetRun(ctx, owner, id)
	if errors.Is(err, store.ErrNoRow) {
		return nil, ErrNoObject
	}
	if err != nil {
		return nil, err
	}
	if r.Ended() {
		return r.LogTail[max(len(r.LogTail)-lines, 0):], nil
	}
	sb, err := c.st.GetSandbox(ctx, id)
	if errors.Is(err, model.ErrNotFound) || (err == nil && !(steady(sb) && c.connected(sb.Node))) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out, err := c.containerLog(ctx, lease(sb), lines, answerLog)
	if err != nil {
		return nil, err
	}
	secrets := c.runSecretValues(ctx, id)
	for i := range out {
		out[i] = redact(out[i], secrets)
	}
	return out, nil
}

// sweepRuns removes the runs that ended RunRetention ago (env-delete).
func (c *Controller) sweepRuns(ctx context.Context) error {
	n, err := c.st.SweepRuns(ctx, RunRetention)
	if n > 0 {
		c.log.Info("ended runs swept", "runs", n)
	}
	return err
}
