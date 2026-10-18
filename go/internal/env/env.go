// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/env/addons"
	"github.com/ironcore-dev/feos/go/internal/env/ctl"
	"github.com/ironcore-dev/feos/go/internal/env/envdeps"
	"github.com/ironcore-dev/feos/go/internal/leader"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
	"github.com/ironcore-dev/feos/go/internal/sandbox"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// The interfaces the service and its controller reach the instance
// through (internal/env/envdeps, where ctl names them too).
type (
	// Engine is what the environments need of the sandbox engine
	// (*sandbox.Engine).
	Engine = envdeps.Engine
	// Runner are the tools' runner's detached calls (*tools.Runner).
	Runner = envdeps.Runner
	// Ports is what the environments need of the port door
	// (*ports.Handler).
	Ports = envdeps.Ports
	// Sealer seals the secrets at rest (internal/seal).
	Sealer = envdeps.Sealer
)

// Deps are the parts of the instance the service works with. DB, Engine
// and Elector are required; Store is made from DB when nil; Blueprint is
// addons.Get when nil. The others may be nil while the instance lacks
// them, each with what then does not work: without Runner the attached
// services and the exec checks, without Ports the http and tcp checks and
// the route door (503), without Sealer the secrets (503 "secrets need
// --secret-key-file") and thereby every spec that references one or has
// an addon.
type Deps struct {
	// InstanceID is the instance's ID.
	InstanceID string
	DB         *db.DB
	Store      *store.Store
	Elector    *leader.Elector
	Conns      nodelink.Conns
	Engine     Engine
	Runner     Runner
	Ports      Ports
	Sealer     Sealer
	// Blueprint looks up an addon type.
	Blueprint func(typ string) (addons.Blueprint, bool)
	Log       *slog.Logger
}

// Service is the environments service of one instance.
type Service struct {
	d   Deps
	st  *store.Store
	ctl *ctl.Controller
	log *slog.Logger
}

// New returns the environments service of an instance, with its
// controller.
func New(deps Deps) (*Service, error) {
	if deps.DB == nil || deps.Engine == nil {
		return nil, errors.New("env: DB and Engine are required")
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Blueprint == nil {
		deps.Blueprint = addons.Get
	}
	st := deps.Store
	if st == nil {
		st = store.New(deps.DB)
	}
	log := deps.Log.With("component", "env")
	s := &Service{d: deps, st: st, log: log}
	c, err := ctl.New(ctl.Deps{
		InstanceID: deps.InstanceID, DB: deps.DB, Store: st, Elector: deps.Elector, Conns: deps.Conns,
		Engine: deps.Engine, Runner: deps.Runner, Ports: deps.Ports, Sealer: deps.Sealer,
		ResolveSecret: s.ResolveSecret, Blueprint: deps.Blueprint, Log: deps.Log,
	})
	if err != nil {
		return nil, err
	}
	s.ctl = c
	var missing []any
	if deps.Runner == nil {
		missing = append(missing, "runner", "no detached calls: attached services and exec checks fail")
	}
	if deps.Ports == nil {
		missing = append(missing, "ports", "no port door exports: http and tcp checks and the route door fail")
	}
	if deps.Sealer == nil {
		missing = append(missing, "sealer", "no --secret-key-file: secrets and addons are refused")
	}
	if len(missing) > 0 {
		log.Info("environments run without some parts", missing...)
	}
	return s, nil
}

// Run runs the service's background work (the controller's) until ctx
// ends, and waits for it.
func (s *Service) Run(ctx context.Context) error {
	return s.ctl.Run(ctx)
}

// Loops returns the leader loops of the environments: the controller's.
func (s *Service) Loops() []model.Loop {
	return s.ctl.Loops()
}

// Controller is the service's controller.
func (s *Service) Controller() *ctl.Controller { return s.ctl }

// SecretsEnabled reports whether the instance can seal secrets
// (--secret-key-file): without, the secrets routes answer 503.
func (s *Service) SecretsEnabled() bool { return s.d.Sealer != nil }

// ResolveSecret is the engine's secret resolver (sandbox.SecretResolver)
// and the controller's: the value of version version of the secret name
// of environment envID, a tenant's or a system secret, unsealed with its
// AAD (envdeps.SecretAAD). A deleted version still resolves (a release
// pins it until a later one is realised).
func (s *Service) ResolveSecret(ctx context.Context, envID uuid.UUID, name string, version int) (string, error) {
	return "", errNotImplemented("resolving a secret")
}

// PrepareHook is the engine's prepare hook (sandbox.PrepareHook): the
// controller's.
func (s *Service) PrepareHook(ctx context.Context, node string, sb store.Sandbox, kind sandbox.OpKind) error {
	return s.ctl.PrepareHook(ctx, node, sb, kind)
}

// The hooks are the engine's.
var (
	_ sandbox.SecretResolver = (*Service)(nil).ResolveSecret
	_ sandbox.PrepareHook    = (*Service)(nil).PrepareHook
)
