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
//
// The target T of an environment is its latest release R, unless R
// failed: then the realised release (nothing when none was realised), so
// that a failed release is never retried until a new one arrives. An
// instance's template hash is model.PodTemplate.Hash over its rendered
// template; an instance whose hash differs from T's is replaced (rolled
// for image services, recreated for addons). An addon's hash takes its
// image as the blueprint writes it (its tag): the row pins the digest at
// its insert, so that a new digest under the tag never recreates it.
package ctl

import (
	"context"
	"errors"
	"log/slog"
	"sync"
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

// errNoSealer is the answer of everything that needs a secret's value
// while the instance has no --secret-key-file.
var errNoSealer = errors.New("secrets need --secret-key-file")

// errTenantDeleting refuses an instance insert of a tenant being deleted.
var errTenantDeleting = errors.New("the tenant is being deleted")

// errEnvDeleting refuses an instance insert of an environment being
// deleted.
var errEnvDeleting = errors.New("the environment is being deleted")

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
	// ResolveSecret is not used: the controller resolves secrets itself
	// (Controller.ResolveSecret), which the environment service's resolver
	// delegates to. It stays so that callers that set it keep compiling.
	ResolveSecret sandbox.SecretResolver
	// Blueprint looks up an addon type.
	Blueprint func(typ string) (addons.Blueprint, bool)
	Log       *slog.Logger
}

// timings are the controller's durations, fields so that the package's
// tests shorten them.
type timings struct {
	// retireGrace is how long a retiring instance stays (routes stop
	// sending to it, hosts blocks drop it) before it is deleted.
	retireGrace time.Duration
	// crashWindow: so many exits within it open crash_loop; restarts
	// reset after an instance was ready for as long.
	crashWindow time.Duration
	crashExits  int
	// restartMax bounds the restart backoff (2^(n-1) s).
	restartMax time.Duration
	// restartUnit is the backoff's first step.
	restartUnit time.Duration
	// steadyAfter: without a readiness check, an instance is ready when
	// its container still runs so long after it was first seen running.
	steadyAfter time.Duration
	// noCapacityAfter: an instance pending without capacity for so long
	// opens no_capacity.
	noCapacityAfter time.Duration
	// provisionRetry is the wait after a failed provisioning step.
	provisionRetry time.Duration
	// stepTimeout bounds one provisioning step; probeTimeout an exec
	// check.
	stepTimeout, probeTimeout time.Duration
	// nodeCall bounds a node call of a loop.
	nodeCall time.Duration
	// attachedReady bounds how long a woken sandbox is held for its
	// process's first readiness.
	attachedReady time.Duration
	// restartReady bounds how long RestartService waits for a restarted
	// replica to be ready before it restarts the next.
	restartReady time.Duration
}

func defaultTimings() timings {
	return timings{
		retireGrace:     5 * time.Second,
		crashWindow:     10 * time.Minute,
		crashExits:      3,
		restartMax:      5 * time.Minute,
		restartUnit:     time.Second,
		steadyAfter:     2 * time.Second,
		noCapacityAfter: 2 * time.Minute,
		provisionRetry:  10 * time.Second,
		stepTimeout:     2 * time.Minute,
		probeTimeout:    10 * time.Second,
		nodeCall:        10 * time.Second,
		attachedReady:   time.Minute,
		restartReady:    5 * time.Minute,
	}
}

// The fenced transactions' timeouts (as the engine's loops).
const (
	fencedLockTimeout = time.Second
	fencedStmtTimeout = 3 * time.Second
)

// Controller is the environment controller of one instance.
type Controller struct {
	d   Deps
	st  *store.Store
	log *slog.Logger
	rl  *rateLog
	t   timings

	pool  *pool
	specs *specCache

	// bg is the parent of the work that outlives a call (RestartService's
	// replica restarts); Run's end cancels it and waits for wg.
	bg       context.Context
	cancelBg context.CancelFunc
	wg       sync.WaitGroup

	mu sync.Mutex
	// seen is what the probe loop remembers of instances and attached
	// services: when their container (process) was first seen running in
	// the phase it observed last, and when they were last checked.
	seen map[string]*seenState
	// locks serialise the supervision of each attached service.
	locks map[string]*sync.Mutex
	// provisionFailed holds when an addon instance's provisioning failed
	// last, for provisionRetry.
	provisionFailed map[uuid.UUID]time.Time
	// lastPending is what each rolling release waited for at its last
	// step, for logPending.
	lastPending map[string]string
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
	log := deps.Log.With("component", "env-ctl")
	c := &Controller{d: deps, st: st, log: log, rl: newRateLog(log), t: defaultTimings(),
		specs: newSpecCache(), seen: map[string]*seenState{}, locks: map[string]*sync.Mutex{},
		provisionFailed: map[uuid.UUID]time.Time{}, lastPending: map[string]string{}}
	c.pool = newPool(poolWorkers, log)
	c.bg, c.cancelBg = context.WithCancel(context.Background())
	return c, nil
}

// Run runs the controller's background work until ctx ends and waits for
// it: what outlives a loop's run (the Ensure pool ends with the leader's
// term, inside the loops; a restart of a service's replicas with ctx).
func (c *Controller) Run(ctx context.Context) error {
	<-ctx.Done()
	c.cancelBg()
	c.wg.Wait()
	c.pool.wait()
	return nil
}

// Loops returns the controller's leader loops (§4.2).
func (c *Controller) Loops() []model.Loop {
	return []model.Loop{
		{Name: LoopReconcile, Interval: reconcileEvery, Run: c.reconcile},
		{Name: LoopProbe, Interval: probeEvery, Run: c.probe},
		{Name: LoopGroups, Interval: groupsEvery, Run: c.groupsLoop},
		{Name: LoopVolumes, Interval: volumesEvery, Run: c.volumesLoop},
		{Name: LoopDelete, Interval: deleteEvery, Run: c.deleteLoop},
	}
}

// fenced runs fn in a transaction of the leader: short lock and statement
// timeouts, the lease checked first (ErrNotLeader without one).
func (c *Controller) fenced(ctx context.Context, fn func(tx *store.Store) error) error {
	if c.d.Elector == nil {
		return leader.ErrNotLeader
	}
	return c.st.InTx(ctx, func(tx *store.Store) error {
		if err := tx.SetLocalTimeouts(ctx, fencedLockTimeout, fencedStmtTimeout); err != nil {
			return err
		}
		if err := c.d.Elector.Fence(ctx, tx.Q()); err != nil {
			return err
		}
		return fn(tx)
	})
}

// notify sends a hint to the leader's loops outside a transaction.
func (c *Controller) notify(ctx context.Context, hint string) {
	if err := c.st.Notify(ctx, db.ChanLeader, hint); err != nil {
		c.rl.warn("notify", "cannot wake the leader's loops", "hint", hint, "err", err)
	}
}

// lease is the lease of a row as the row says: no Acquire.
func lease(sb store.Sandbox) model.Lease {
	return model.Lease{SandboxID: sb.ID, Incarnation: sb.Incarnation, Node: sb.Node}
}

// steady reports whether a row runs without an op: what the probes and
// the detached calls need.
func steady(sb store.Sandbox) bool {
	return sb.ClientState == model.StateRunning && sb.Op == "" && sb.Node != ""
}

// attachedKey names an attached service in the controller's maps.
func attachedKey(envID uuid.UUID, service string) string { return envID.String() + "/" + service }

// attachedLock is the lock of an attached service's supervision.
func (c *Controller) attachedLock(envID uuid.UUID, service string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := attachedKey(envID, service)
	m := c.locks[k]
	if m == nil {
		m = &sync.Mutex{}
		c.locks[k] = m
	}
	return m
}

// rateLog logs a warning of each kind at most once a minute.
type rateLog struct {
	log  *slog.Logger
	mu   sync.Mutex
	last map[string]time.Time
}

func newRateLog(log *slog.Logger) *rateLog { return &rateLog{log: log, last: map[string]time.Time{}} }

func (r *rateLog) warn(kind, msg string, args ...any) {
	r.mu.Lock()
	now := time.Now()
	if t, ok := r.last[kind]; ok && now.Sub(t) < time.Minute {
		r.mu.Unlock()
		r.log.Debug(msg, args...)
		return
	}
	r.last[kind] = now
	r.mu.Unlock()
	r.log.Warn(msg, args...)
}

// objectName is an object's name as incidents and labels write it.
func objectName(kind, name string) string { return kind + "/" + name }
