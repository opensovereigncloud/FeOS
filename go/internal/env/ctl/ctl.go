// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package ctl is the environment controller (ADR 0018): the leader loops
// that bring every environment to its target release and keep it there,
// and the engine's prepare hook.
//
// Loops (Loops), run by the leader, fenced, never with a node call inside
// a transaction:
//
//	env-reconcile  2 s, hint "env-reconcile": one step per environment whose latest
//	               release is not realised, whose instances do not match their
//	               target, or that is deleting: groups, addons (credentials,
//	               volume, instance, provisioning), release jobs, image services
//	               (rolling, surge 1), attached services, removed objects, realised
//	env-probe      1 s: containers' exits and restarts with backoff, readiness
//	               (http, tcp, exec), attached services' processes, hosts blocks
//	env-groups     10 s, hint "env-groups": the security groups each connected
//	               node should have, re-put where they differ
//	env-volumes    30 s: the volumes marked deleting, deleted on their nodes
//	env-delete     5 s: environments deleting (and those of deleting tenants)
//
// The controller inserts and deletes instance rows (sandboxes with
// environment_id) and runs Engine.Ensure for the rows that should run in
// a bounded pool of its own (8 workers per instance, single flight per
// row); reconcile steps enqueue, never call Ensure themselves.
package ctl

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/env/addons"
	"github.com/ironcore-dev/feos/go/internal/env/envdeps"
	"github.com/ironcore-dev/feos/go/internal/leader"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
	"github.com/ironcore-dev/feos/go/internal/sandbox"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// The controller's loops, and the hints that wake them: a writer that
// wants the controller to act sends db.Notify(ctx, q, db.ChanLeader,
// HintReconcile) (or HintGroups) inside its transaction.
const (
	LoopReconcile = "env-reconcile"
	LoopProbe     = "env-probe"
	LoopGroups    = "env-groups"
	LoopVolumes   = "env-volumes"
	LoopDelete    = "env-delete"

	HintReconcile = LoopReconcile
	HintGroups    = LoopGroups
)

// The loops' intervals (§4.2).
const (
	reconcileEvery = 2 * time.Second
	probeEvery     = time.Second
	groupsEvery    = 10 * time.Second
	volumesEvery   = 30 * time.Second
	deleteEvery    = 5 * time.Second
)

// The answers of LogTail, RestartService and RunJob that the API maps to
// statuses of its own.
var (
	// ErrNoObject: the environment's target release has no such object
	// (or replica).
	ErrNoObject = errors.New("no such object")
	// ErrPaused: the instance is paused or hibernated; reading its logs
	// would wake it (409 "paused").
	ErrPaused = errors.New("paused")
	// ErrNoContainerLogs: the instance's node does not serve container
	// logs (an older feosd answers Unimplemented): 501 "this node does not
	// serve container logs yet".
	ErrNoContainerLogs = errors.New("this node does not serve container logs yet")
)

// The sources of LogTail's lines.
const (
	// SourceContainer: the main container's log of an image instance.
	SourceContainer = "container"
	// SourceProcess: the process log of an attached service.
	SourceProcess = "process"
)

// errNotImplemented is the answer of what a later lane implements.
var errNotImplemented = errors.New("environment controller: not implemented yet")

// Deps are the parts of the instance the controller works with. DB,
// Engine and Elector are required; Store is made from DB when nil;
// Blueprint is addons.Get when nil. Without a Runner the attached services
// and exec checks fail (with a detail saying so), without Ports the http
// and tcp checks, without a Sealer the addons' credentials.
type Deps struct {
	// InstanceID is the instance's ID.
	InstanceID string
	DB         *db.DB
	Store      *store.Store
	Elector    *leader.Elector
	Conns      nodelink.Conns
	Engine     envdeps.Engine
	Runner     envdeps.Runner
	Ports      envdeps.Ports
	Sealer     envdeps.Sealer
	// ResolveSecret unseals a secret of an environment: the attached
	// services' env is resolved with it (and never written to disk).
	ResolveSecret sandbox.SecretResolver
	// Blueprint looks up an addon type.
	Blueprint func(typ string) (addons.Blueprint, bool)
	Log       *slog.Logger
}

// Controller is the environment controller of one instance.
type Controller struct {
	d   Deps
	st  *store.Store
	log *slog.Logger
}

// New returns the controller of an instance.
func New(deps Deps) (*Controller, error) {
	if deps.DB == nil || deps.Engine == nil {
		return nil, errors.New("ctl: DB and Engine are required")
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
	return &Controller{d: deps, st: st, log: deps.Log.With("component", "env-ctl")}, nil
}

// Run runs the controller's background work until ctx ends and waits for
// it: what outlives a loop's run (the Ensure pool ends with the leader's
// term, inside the loops).
func (c *Controller) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Loops returns the controller's leader loops (§4.2).
func (c *Controller) Loops() []model.Loop {
	idle := func(context.Context) error { return nil }
	return []model.Loop{
		{Name: LoopReconcile, Interval: reconcileEvery, Run: idle},
		{Name: LoopProbe, Interval: probeEvery, Run: idle},
		{Name: LoopGroups, Interval: groupsEvery, Run: idle},
		{Name: LoopVolumes, Interval: volumesEvery, Run: idle},
		{Name: LoopDelete, Interval: deleteEvery, Run: idle},
	}
}

// PrepareHook is the engine's prepare hook (sandbox.PrepareHook) for a
// row with a pod template: it puts the row's security groups, and those
// their rules reference, on node (whole groups); before a create it also
// makes sure the row's volume is there: created on node when it never was
// (then provisioned, its node and the row's pin_node set in a transaction
// of its own), else present there and free (AVAILABLE, CHECKPOINTED
// without a pod, or held by this pod); otherwise it fails and opens
// volume_unavailable.
func (c *Controller) PrepareHook(ctx context.Context, node string, sb store.Sandbox, kind sandbox.OpKind) error {
	return nil
}

// LogTail returns the last lines lines of an object's log, oldest first,
// and their source: the main container's log of replica replica of an
// image service, of an addon (replica 0), or of a job's latest run (its
// stored tail once the run's row is gone) (SourceContainer); the process
// log of an attached service (SourceProcess). It never wakes an instance
// (ErrPaused); ErrNoObject, ErrNoContainerLogs.
func (c *Controller) LogTail(ctx context.Context, envID uuid.UUID, kind, name string, replica, lines int) ([]string, string, error) {
	return nil, "", errNotImplemented
}

// RestartService restarts a service of the environment's target release:
// an attached service's process is stopped and started again (never its
// sandbox's pod); an image service's replicas are restarted one at a
// time. It returns once the restart is under way; ErrNoObject.
func (c *Controller) RestartService(ctx context.Context, envID uuid.UUID, name string) error {
	return errNotImplemented
}

// RunJob starts a run of a job of the environment's target release (what
// jobs on "manual" are for): a new attempt 1. It returns once the run's
// row is inserted; ErrNoObject.
func (c *Controller) RunJob(ctx context.Context, envID uuid.UUID, name string) error {
	return errNotImplemented
}
