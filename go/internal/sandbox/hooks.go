// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/store"
)

// What the environments (ADR 0018) hook into the engine. An instance row
// of an environment (sandboxes.environment_id set) is a sandbox to the
// engine; its pod is built from its pod_template, whose secrets the
// SecretResolver unseals, and the PrepareHook puts on the node what the
// pod needs before the pod call (its security groups, its volume).

// SecretResolver returns the value of version version of the secret name
// of the environment envID, for a pod template's env (model.TemplateValue
// and its parts). An error fails the op before any pod call.
type SecretResolver func(ctx context.Context, envID uuid.UUID, name string, version int) (string, error)

// PrepareHook prepares node for the op of kind on the row sb, which has a
// pod template: called in execute after the connection hook and before
// the pod call (CreatePod, StartPod, ResumePod, RestorePod), and inside
// callRepair before its pod calls. An error before the pod call means
// nothing was sent: the op rolls back with the error as its cause; inside
// callRepair it counts as sent (the observation decides).
type PrepareHook func(ctx context.Context, node string, sb store.Sandbox, kind OpKind) error

// errNoRestart is Restart's answer until the engine implements it.
var errNoRestart = errors.New("sandbox: Restart is not implemented yet")

// engineHooks are the hooks the server sets after the engine and the
// environments both exist.
type engineHooks struct {
	mu      sync.Mutex
	secrets SecretResolver
	prepare PrepareHook
}

// SetSecretResolver sets what a pod template's secrets are unsealed with;
// nil: a template that names a secret fails its op.
func (e *Engine) SetSecretResolver(r SecretResolver) {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	e.hooks.secrets = r
}

// SetPrepareHook sets the hook that prepares a node for an op on a row
// with a pod template; nil: none.
func (e *Engine) SetPrepareHook(h PrepareHook) {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	e.hooks.prepare = h
}

// secretResolver is the resolver SetSecretResolver set, or nil.
func (e *Engine) secretResolver() SecretResolver {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	return e.hooks.secrets
}

// prepareHook is the hook SetPrepareHook set, or nil.
func (e *Engine) prepareHook() PrepareHook {
	e.hooks.mu.Lock()
	defer e.hooks.mu.Unlock()
	return e.hooks.prepare
}

// Restart restarts the pod of a running sandbox, for an environment's
// instance whose container exited: a repair op with the trigger "restart",
// which the precheck allows for any running row and which skips the
// repair's recheck (callRepair's StopPod and StartPod; a row with volumes
// never takes the DeletePod and RestorePod branch). reason is the op's
// description. It returns once the op began; the op's outcome arrives as
// the row's state.
func (e *Engine) Restart(ctx context.Context, id uuid.UUID, reason string) error {
	return errNoRestart
}

// ResolveImage pins an image to its digest as a sandbox's create does:
// "<registry>/<repo>@sha256:<linux/amd64 manifest digest>", over the
// anonymous registry protocol, honouring --deny-prefix, a tag cached for
// 60 s. A digest reference is pinned without asking the registry.
func (e *Engine) ResolveImage(ctx context.Context, image string) (string, error) {
	return e.resolveImage(ctx, image)
}
