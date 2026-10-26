// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package fleet is the control plane's side of the uplink, feos.fleet.v1
// (ADR 0015): the FleetService that nodes call on the nodes' port, and
// what an instance knows and decides about nodes from it.
//
//   - Join, on the join server, turns a one-time join token and a signing
//     request into a node name and its first certificate.
//   - RenewCertificate, ReportTelemetry, PublishEvents and
//     IssuePodCredentials, on the uplink server, take the node's name from
//     its verified certificate (nodelink.NodeFromContext), never from a
//     message.
//   - CheckNode decides, for nodelink, whether a node's certificate may
//     connect, on the database clock and never from a cache; OnPresented
//     records the certificate lineage (a newer certificate supersedes the
//     older ones after a grace; a superseded one presented after it means
//     the identity forked).
//   - Run follows the other instances' writes: ferun_node re-checks a
//     node's connections and reloads its rollout deadline, ferun_reconcile
//     hands a node's inventory reconcile to an instance connected to it.
//   - Loops are the leader's jobs: node expiry, the reconcile sweep, the
//     join tokens' expiry and the retention of the fleet's tables.
package fleet

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/ironcore-dev/feos/go/gen/feos/fleet/v1"
	"github.com/ironcore-dev/feos/go/internal/activity"
	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/leader"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
	"github.com/ironcore-dev/feos/go/internal/pki"
	"github.com/ironcore-dev/feos/go/internal/podid"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// BindingsSource gives a pod's bindings and their version, which
// IssuePodCredentials seals to the run's key (*bindings.Service). No
// bindings, or bindings switched off: nil.
type BindingsSource interface {
	PodBindings(ctx context.Context, podID podid.ID) (*fleetv1.PodBindings, string, error)
}

// SandboxHooks is what fleet hands to the sandbox engine (*sandbox.Engine):
// pod events, inventory reconciles and the registry's inventory hash.
type SandboxHooks interface {
	PodChanged(node string, podID podid.ID)
	ReconcileNode(ctx context.Context, node string) error
	RegistryHash(ctx context.Context, node string) (string, error)
}

// Links is the instance's nodes' port as fleet uses it (*nodelink.Server).
type Links interface {
	nodelink.Conns
	Recheck(ctx context.Context, node string)
	RecheckAll(ctx context.Context)
	SetStoppingUntil(node string, until time.Time)
}

var _ Links = (*nodelink.Server)(nil)

// Config is fleet's part of the instance's configuration.
type Config struct {
	InstanceID, TrustDomain string
	// NodeCertTTL and PodCertTTL are the lifetimes of node and pod
	// certificates (--node-cert-ttl, --pod-cert-ttl).
	NodeCertTTL, PodCertTTL time.Duration
	// RolloutGrace is how long calls wait for a node that announced
	// "stopping" (--rollout-grace).
	RolloutGrace time.Duration
	// NodeTTL is the telemetry age after which the leader marks a node
	// down (--node-ttl).
	NodeTTL time.Duration
	// ConnDrainTimeout is --conn-drain-timeout; a superseded certificate's
	// grace is this plus 10 minutes (§8).
	ConnDrainTimeout time.Duration
	// ActivityRetention is how long event_gaps rows are kept
	// (--activity-retention).
	ActivityRetention time.Duration
	// PodNetwork is the cluster's ULA when its pods are on a mesh (ADR
	// 0017); the zero prefix when its network is routed.
	PodNetwork netip.Prefix
	Log        *slog.Logger
}

// Deps are the parts of the instance fleet works with.
type Deps struct {
	DB       *db.DB
	Store    *store.Store
	Issuer   *pki.Issuer
	Links    Links
	Hooks    SandboxHooks
	Bindings BindingsSource // nil: no bindings
	Activity *activity.Recorder
	Elector  *leader.Elector
}

// Defaults of Config's durations when zero (§3).
const (
	defaultNodeCertTTL       = 168 * time.Hour
	defaultPodCertTTL        = time.Hour
	defaultRolloutGrace      = 30 * time.Second
	defaultNodeTTL           = 15 * time.Second
	defaultConnDrainTimeout  = 20 * time.Minute
	defaultActivityRetention = 720 * time.Hour
)

// Fixed timings of §8.
const (
	// supersededExtra is what a superseded certificate's grace adds to the
	// drain timeout, so that a renewal's draining connections finish
	// before they are cut.
	supersededExtra = 10 * time.Minute
	// telemetryInterval is what ReportTelemetry tells nodes to wait.
	telemetryInterval = 5
	// reconcileClaimEvery spaces the inventory reconciles of a node across
	// instances.
	reconcileClaimEvery = 30 * time.Second
	// reconcileNotifyEvery spaces this instance's ferun_reconcile requests
	// for one node.
	reconcileNotifyEvery = 5 * time.Second
	// reconcileTimeout bounds one inventory reconcile.
	reconcileTimeout = 5 * time.Minute
	// mismatchSettle is how long a node's inventory hash must differ from
	// the registry's before a reconcile is requested (two reports, so that
	// ops in flight settle).
	mismatchSettle = 10 * time.Second
	// joinRetryWindow is how long a used join token answers again a Join
	// with the same key, whose answer was lost.
	joinRetryWindow = 10 * time.Minute
	// presentTimeout bounds the recording of a certificate presentation.
	presentTimeout = 10 * time.Second
	// maxPresentations is how many presentations one instance records
	// under a node's row lock at once.
	maxPresentations = 4
	// rpcTimeout bounds the database work of one unary call.
	rpcTimeout = 15 * time.Second
)

// Service implements feos.fleet.v1 and fleet's part of the nodes' port.
type Service struct {
	cfg      Config
	d        *db.DB
	st       *store.Store
	iss      *pki.Issuer
	links    Links
	hooks    SandboxHooks
	bindings BindingsSource
	act      *activity.Recorder
	el       *leader.Elector
	log      *slog.Logger
	rl       *rateLog

	// Join's own limits (§8): a token bucket of 10 joins/s with a burst of
	// 20, and 4 database slots waited for at most joinSlotWait.
	joins        *tokenBucket
	joinSlots    chan struct{}
	joinSlotWait time.Duration
	// presents are the slots of OnPresented's locked passes (4).
	presents chan struct{}

	// The inputs of node expiry's health gate; tests replace them.
	leadingSince func() time.Time
	lastFailure  func() time.Time
	// now is this instance's clock, which turns a node's rollout grace
	// into a deadline for nodelink (time.Now); tests skew it.
	now func() time.Time
	// expiryBatch is the most nodes one transaction of node expiry marks
	// down (fencedBatch; tests make it smaller).
	expiryBatch int

	// Background work (reconciles, re-checks) belongs to Run: it stops
	// with Run's context.
	bg       context.Context
	bgCancel context.CancelFunc
	bgMu     sync.Mutex
	bgClosed bool
	wg       sync.WaitGroup

	reconciles coalescer // per node: one reconcile on this instance at a time
	refreshes  coalescer // per node ("" for all): reload and re-check

	mu       sync.Mutex
	notified map[string]time.Time // node → when this instance last sent ferun_reconcile for it

	mesh meshSender // the leader's state of the peers it sent

	unknownEvents atomic.Int64
}

// New returns the fleet service. DB and Issuer are required; Store and
// Activity are made from DB when nil.
func New(cfg Config, deps Deps) *Service {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	defaults := []struct {
		v *time.Duration
		d time.Duration
	}{
		{&cfg.NodeCertTTL, defaultNodeCertTTL}, {&cfg.PodCertTTL, defaultPodCertTTL},
		{&cfg.RolloutGrace, defaultRolloutGrace}, {&cfg.NodeTTL, defaultNodeTTL},
		{&cfg.ConnDrainTimeout, defaultConnDrainTimeout}, {&cfg.ActivityRetention, defaultActivityRetention},
	}
	for _, x := range defaults {
		if *x.v <= 0 {
			*x.v = x.d
		}
	}
	log := cfg.Log.With("component", "fleet")
	st := deps.Store
	if st == nil {
		st = store.New(deps.DB)
	}
	act := deps.Activity
	if act == nil {
		act = activity.NewRecorder(st, cfg.InstanceID, cfg.Log)
	}
	s := &Service{
		cfg: cfg, d: deps.DB, st: st, iss: deps.Issuer, links: deps.Links, hooks: deps.Hooks,
		bindings: deps.Bindings, act: act, el: deps.Elector, log: log, rl: newRateLog(log),
		joins:        newTokenBucket(10, 20),
		joinSlots:    make(chan struct{}, 4),
		joinSlotWait: 2 * time.Second,
		presents:     make(chan struct{}, maxPresentations),
		leadingSince: func() time.Time { return time.Time{} },
		lastFailure:  deps.DB.LastFailure,
		now:          time.Now,
		expiryBatch:  fencedBatch,
		notified:     map[string]time.Time{},
	}
	if s.el != nil {
		s.leadingSince = s.el.LeadingSince
	}
	s.bg, s.bgCancel = context.WithCancel(context.Background())
	return s
}

// supersededGrace is how long a superseded certificate stays valid.
func (s *Service) supersededGrace() time.Duration {
	return s.cfg.ConnDrainTimeout + supersededExtra
}

// UplinkServer returns the FleetService of the uplink server: every RPC but
// Join, which answers UNIMPLEMENTED.
func (s *Service) UplinkServer() fleetv1.FleetServiceServer { return &uplinkServer{s: s} }

// JoinServer returns the FleetService of the join server: Join only.
func (s *Service) JoinServer() fleetv1.FleetServiceServer { return &joinServer{s: s} }

type uplinkServer struct {
	fleetv1.UnimplementedFleetServiceServer
	s *Service
}

func (u *uplinkServer) RenewCertificate(ctx context.Context, req *fleetv1.RenewCertificateRequest) (*fleetv1.RenewCertificateResponse, error) {
	return u.s.renewCertificate(ctx, req)
}

func (u *uplinkServer) ReportTelemetry(ctx context.Context, req *fleetv1.Telemetry) (*fleetv1.TelemetryResponse, error) {
	return u.s.reportTelemetry(ctx, req)
}

func (u *uplinkServer) PublishEvents(stream grpc.BidiStreamingServer[fleetv1.EventBatch, fleetv1.EventAck]) error {
	return u.s.publishEvents(stream)
}

func (u *uplinkServer) IssuePodCredentials(ctx context.Context, req *fleetv1.IssuePodCredentialsRequest) (*fleetv1.IssuePodCredentialsResponse, error) {
	return u.s.issuePodCredentials(ctx, req)
}

func (u *uplinkServer) JoinMesh(ctx context.Context, req *fleetv1.JoinMeshRequest) (*fleetv1.JoinMeshResponse, error) {
	return u.s.joinMesh(ctx, req)
}

type joinServer struct {
	fleetv1.UnimplementedFleetServiceServer
	s *Service
}

func (j *joinServer) Join(ctx context.Context, req *fleetv1.JoinRequest) (*fleetv1.JoinResponse, error) {
	return j.s.join(ctx, req)
}

// nodePeer returns the node of an uplink call, from its certificate.
func nodePeer(ctx context.Context) (nodelink.Peer, error) {
	p, ok := nodelink.NodeFromContext(ctx)
	if !ok || p.Node == "" {
		return nodelink.Peer{}, status.Error(codes.Unauthenticated, "no node certificate on this connection")
	}
	return p, nil
}

// peerAddress is the remote address of the call's connection.
func peerAddress(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

// rpcError is what a call answers for err: a gRPC status as it is, the
// caller's cancellation as such, anything else (the database) as
// UNAVAILABLE, so that the node tries again, on this or another instance.
func rpcError(err error, what string) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok {
		return st.Err()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return status.Errorf(codes.Canceled, "%s: %v", what, err)
	case errors.Is(err, context.DeadlineExceeded):
		return status.Errorf(codes.DeadlineExceeded, "%s: %v", what, err)
	}
	return status.Errorf(codes.Unavailable, "%s: %v", what, err)
}

// goBackground runs f in a goroutine owned by Run; false once Run ended.
func (s *Service) goBackground(f func(ctx context.Context)) bool {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgClosed {
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f(s.bg)
	}()
	return true
}

// stop ends the background work and waits for it.
func (s *Service) stop() {
	s.bgMu.Lock()
	s.bgClosed = true
	s.bgMu.Unlock()
	s.bgCancel()
	s.wg.Wait()
}

// coalescer runs one job per key at a time; a request while one runs
// makes exactly one more run after it, which covers every request that
// arrived meanwhile.
type coalescer struct {
	mu      sync.Mutex
	running map[string]bool
	again   map[string]bool
}

// request reports whether the caller starts the key's job; otherwise the
// running job runs once more.
func (c *coalescer) request(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running == nil {
		c.running, c.again = map[string]bool{}, map[string]bool{}
	}
	if c.running[key] {
		c.again[key] = true
		return false
	}
	c.running[key] = true
	return true
}

// next reports whether the key's job must run once more; when not, the
// job is over.
func (c *coalescer) next(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.again[key] {
		delete(c.again, key)
		return true
	}
	delete(c.running, key)
	return false
}

// abort ends the key's job without another run.
func (c *coalescer) abort(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.running, key)
	delete(c.again, key)
}

// coalesce starts job for key in the background unless it runs already,
// and runs it again as long as requests arrive while it runs.
func (s *Service) coalesce(c *coalescer, key string, job func(ctx context.Context)) {
	if !c.request(key) {
		return
	}
	if !s.goBackground(func(ctx context.Context) {
		for {
			job(ctx)
			if ctx.Err() != nil {
				c.abort(key)
				return
			}
			if !c.next(key) {
				return
			}
		}
	}) {
		c.abort(key)
	}
}

// tokenBucket admits rate events per second with a burst.
type tokenBucket struct {
	mu            sync.Mutex
	rate, burst   float64
	tokens        float64
	last          time.Time
	nowForTesting func() time.Time
}

func newTokenBucket(rate, burst float64) *tokenBucket {
	return &tokenBucket{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.nowForTesting != nil {
		now = b.nowForTesting()
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed.Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rateLog logs a kind of warning at most once a second, with the number
// of those suppressed since: log lines that a remote party can trigger.
type rateLog struct {
	log   *slog.Logger
	mu    sync.Mutex
	kinds map[string]*rateKind
}

type rateKind struct {
	last       time.Time
	suppressed int64
}

func newRateLog(log *slog.Logger) *rateLog {
	return &rateLog{log: log, kinds: map[string]*rateKind{}}
}

func (r *rateLog) warn(kind, msg string, args ...any) {
	r.mu.Lock()
	k := r.kinds[kind]
	if k == nil {
		k = &rateKind{}
		r.kinds[kind] = k
	}
	now := time.Now()
	if now.Sub(k.last) < time.Second {
		k.suppressed++
		r.mu.Unlock()
		return
	}
	suppressed := k.suppressed
	k.last, k.suppressed = now, 0
	r.mu.Unlock()
	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	r.log.Warn(msg, args...)
}
