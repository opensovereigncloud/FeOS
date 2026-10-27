// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// The ALPN protocols of the nodes' port. Anything else, none included, is
// closed.
const (
	ALPNNode   = "feos-node/1" // a node connection: the node is the HTTP/2 server
	ALPNUplink = "h2"          // the uplink and joins: the node is the client
)

// Limits of the accept path (§4 of the design).
const (
	maxPendingHandshakes = 512
	maxConcurrentChecks  = 32
	maxJoinConns         = 64 // join and refusal connections together
	maxUplinksPerNode    = 4
	defaultMaxStreams    = 1024
	defaultDrainTimeout  = 20 * time.Minute
)

// Config is what an instance's nodes' port needs.
type Config struct {
	// Addr is the listen address, --node-listen.
	Addr        string
	TrustDomain string
	InstanceID  string
	// Bundle is the trust bundle that client certificates verify to.
	Bundle *x509.CertPool
	// ServerCert is pki.ServerCertSource.GetCertificate.
	ServerCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// NodeWait is how long Conn waits for a node's connection (--node-wait);
	// ConnDrainTimeout how long a draining connection lives at most
	// (--conn-drain-timeout; 20 minutes when zero).
	NodeWait, ConnDrainTimeout time.Duration
	// MaxStreams is --node-max-streams, the HTTP/2 stream limit nodes
	// advertise on a node connection; the stream and tool budgets derive
	// from it (1024 when zero).
	MaxStreams int
	// CheckNode is asked at accept (atAccept) and at every re-check, never
	// cached. revokeAt is the certificate's revocation time when it lies
	// ahead (zero otherwise). An error wrapping model.ErrNodeRefused or
	// model.ErrCertSuperseded is definitive; any other error is transient.
	// The text of a definitive refusal at accept is what the refusal server
	// answers an uplink with (see Refusal).
	CheckNode func(ctx context.Context, node, serial, peerAddr string, atAccept bool) (revokeAt time.Time, err error)
	// OnPresented is called after a successful accept check, in the
	// check's slot of the 32 (so that its database work is bounded as the
	// checks' is), before the connection takes calls.
	OnPresented func(node, serial, peerAddr string)
	// OnNodeConn is called, in a goroutine of its own, when a node gets a
	// current connection on this instance after having none.
	OnNodeConn func(node string)
	Log        *slog.Logger
}

// Server is an instance's nodes' port. It implements Conns.
type Server struct {
	cfg       Config
	log       *slog.Logger
	rl        *rateLog
	lis       net.Listener
	tlsConfig *tls.Config

	uplink, join, refusal          *grpc.Server
	uplinkLis, joinLis, refusalLis *chanListener

	// The timings of §4, fields so that tests can shorten them.
	handshakeTimeout  time.Duration // a TLS handshake
	readyTimeout      time.Duration // a node connection's gRPC channel to READY
	checkWait         time.Duration // waiting for a CheckNode slot
	checkTimeout      time.Duration // one CheckNode call
	joinLifetime      time.Duration // a join or refusal connection
	joinConnTimeout   time.Duration // grpc.ConnectionTimeout of the join and refusal servers
	joinMaxAge        time.Duration // their MaxConnectionAge
	recheckSpread     time.Duration // RecheckAll's spread
	recheckMinBackoff time.Duration // re-check retries after a transient error
	recheckMaxBackoff time.Duration
	supersededGrace   time.Duration // an uplink superseded at accept, idle
	idleTimeout       time.Duration // grpc.WithIdleTimeout of node connections: 0, never idle
	clientKeepalive   keepalive.ClientParameters
	maxUplinks        int

	longBudget, toolBudget int

	handshakes chan struct{}
	checks     chan struct{}
	joinSlots  chan struct{}

	// ctx owns the port's background work; it ends at Shutdown.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	nodes    map[string]*nodeState
	gens     map[string]uint64 // the number of re-checks per node name seen (checkGen)
	allGen   uint64            // the number of RecheckAll calls
	subs     map[*subscriber]struct{}
	seq      uint64
	serving  bool
	shut     bool
	sweeping bool
	sweepDue bool

	// Counters of connections refused by the limits.
	droppedHandshakes atomic.Int64
	droppedJoins      atomic.Int64
	droppedUplinks    atomic.Int64
}

// New makes the nodes' port and binds its listener; Serve accepts.
func New(cfg Config) (*Server, error) {
	if err := pki.ValidateTrustDomain(cfg.TrustDomain); err != nil {
		return nil, err
	}
	if cfg.Bundle == nil || cfg.ServerCert == nil || cfg.CheckNode == nil {
		return nil, errors.New("nodelink: Bundle, ServerCert and CheckNode are required")
	}
	if cfg.Addr == "" {
		return nil, errors.New("nodelink: no listen address")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = defaultMaxStreams
	}
	if cfg.ConnDrainTimeout <= 0 {
		cfg.ConnDrainTimeout = defaultDrainTimeout
	}
	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("nodes' port: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg: cfg,
		log: cfg.Log,
		rl:  newRateLog(cfg.Log),
		lis: lis,
		tlsConfig: &tls.Config{
			MinVersion:             tls.VersionTLS13,
			NextProtos:             []string{ALPNNode, ALPNUplink},
			ClientAuth:             tls.VerifyClientCertIfGiven,
			ClientCAs:              cfg.Bundle,
			SessionTicketsDisabled: true,
			GetCertificate:         cfg.ServerCert,
			GetConfigForClient:     noteClientHello,
		},
		handshakeTimeout:  10 * time.Second,
		readyTimeout:      10 * time.Second,
		checkWait:         5 * time.Second,
		checkTimeout:      10 * time.Second,
		joinLifetime:      60 * time.Second,
		joinConnTimeout:   10 * time.Second,
		joinMaxAge:        30 * time.Second,
		recheckSpread:     5 * time.Second,
		recheckMinBackoff: time.Second,
		recheckMaxBackoff: 30 * time.Second,
		supersededGrace:   5 * time.Second,
		clientKeepalive: keepalive.ClientParameters{
			Time: 10 * time.Second, Timeout: 5 * time.Second, PermitWithoutStream: true,
		},
		maxUplinks: maxUplinksPerNode,
		longBudget: max(1, cfg.MaxStreams/2),
		toolBudget: max(1, cfg.MaxStreams/4),
		handshakes: make(chan struct{}, maxPendingHandshakes),
		checks:     make(chan struct{}, maxConcurrentChecks),
		joinSlots:  make(chan struct{}, maxJoinConns),
		ctx:        ctx,
		cancel:     cancel,
		nodes:      map[string]*nodeState{},
		gens:       map[string]uint64{},
		subs:       map[*subscriber]struct{}{},
	}
	s.uplinkLis = newChanListener(lis.Addr())
	s.joinLis = newChanListener(lis.Addr())
	s.refusalLis = newChanListener(lis.Addr())
	s.uplink = s.newUplinkServer()
	s.join = s.newJoinServer()
	s.refusal = s.newRefusalServer()
	return s, nil
}

// Addr is the address the port listens on.
func (s *Server) Addr() net.Addr { return s.lis.Addr() }

// Uplink is the server of authenticated uplinks; fleet registers its
// FleetService (all RPCs but Join) on it before Serve.
func (s *Server) Uplink() *grpc.Server { return s.uplink }

// Join is the server of connections without a client certificate; fleet
// registers its Join on it before Serve.
func (s *Server) Join() *grpc.Server { return s.join }

// Serve starts the port: it accepts connections and serves node
// connections, uplinks and joins until Shutdown. It returns nil when ctx
// ends or Shutdown is called, whichever comes first.
//
// The end of ctx stops nothing. A stopping instance stays listed to the
// nodes until it deletes its row in step 5 of its shutdown (§7, §8): its
// in-flight requests and ops need their node connections until then, and
// nodes re-dial a connection to it that broke. Shutdown, in step 5, is
// the only way to close the port.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	if s.serving || s.shut {
		s.mu.Unlock()
		return errors.New("nodelink: Serve called twice or after Shutdown")
	}
	s.serving = true
	s.wg.Add(4)
	s.mu.Unlock()
	go func() { defer s.wg.Done(); _ = s.uplink.Serve(s.uplinkLis) }()
	go func() { defer s.wg.Done(); _ = s.join.Serve(s.joinLis) }()
	go func() { defer s.wg.Done(); _ = s.refusal.Serve(s.refusalLis) }()
	accepting := make(chan struct{})
	go func() {
		defer s.wg.Done()
		defer close(accepting)
		s.acceptLoop()
	}()
	select {
	case <-ctx.Done():
	case <-accepting:
	}
	return nil
}

// acceptLoop accepts until the listener is closed. Any other error, such as
// EMFILE or ENOBUFS while descriptors or buffers run out under load, is
// retried with backoff (5 ms doubling to 1 s): the port never stops
// accepting while the listener is open.
func (s *Server) acceptLoop() {
	var backoff time.Duration
	for {
		c, err := s.lis.Accept()
		if err != nil {
			if s.isShut() {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				s.log.Error("nodes' port: listener closed before Shutdown; accepting no more", "err", err)
				return
			}
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			s.rl.warn("accept", "nodes' port: accept failed; retrying", "err", err, "backoff", backoff)
			t := time.NewTimer(backoff)
			select {
			case <-t.C:
			case <-s.ctx.Done():
				t.Stop()
				return
			}
			continue
		}
		backoff = 0
		s.accept(c)
	}
}

func (s *Server) isShut() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shut
}

// goOwned runs f in a goroutine the port owns, unless it is shut down.
func (s *Server) goOwned(f func()) bool {
	s.mu.Lock()
	if s.shut {
		s.mu.Unlock()
		return false
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		f()
	}()
	return true
}

// accept admits a TCP connection to a handshake, or closes it when 512
// handshakes are pending.
func (s *Server) accept(c net.Conn) {
	tuneTCP(c)
	select {
	case s.handshakes <- struct{}{}:
	default:
		s.droppedHandshakes.Add(1)
		c.Close()
		s.rl.warn("handshakes", "nodes' port: too many pending handshakes; connection closed", "peer", c.RemoteAddr().String())
		return
	}
	if !s.goOwned(func() { s.handshake(c) }) {
		<-s.handshakes
		c.Close()
	}
}

// handshake runs the TLS handshake under its deadline and routes the
// connection by its ALPN protocol.
func (s *Server) handshake(c net.Conn) {
	hc := &helloConn{Conn: c}
	tc := tls.Server(hc, s.tlsConfig)
	ctx, cancel := context.WithTimeout(s.ctx, s.handshakeTimeout)
	err := tc.HandshakeContext(ctx)
	cancel()
	<-s.handshakes
	if err != nil {
		c.Close()
		if !isProbe(err, hc.hello.Load()) {
			s.rl.warn("handshake", "nodes' port: TLS handshake failed", "peer", c.RemoteAddr().String(), "err", err)
		}
		return
	}
	state := tc.ConnectionState()
	switch state.NegotiatedProtocol {
	case ALPNNode:
		s.acceptNode(tc, state)
	case ALPNUplink:
		s.acceptH2(tc, state)
	default:
		tc.Close()
		s.rl.warn("alpn", "nodes' port: no known ALPN protocol; connection closed",
			"peer", c.RemoteAddr().String(), "alpn", state.NegotiatedProtocol)
	}
}

// identify returns the node of a verified client certificate.
func (s *Server) identify(tc *tls.Conn, state tls.ConnectionState) (node, serial string, leaf *x509.Certificate, ok bool) {
	leaf = state.PeerCertificates[0]
	node, err := pki.ParseNodeID(s.cfg.TrustDomain, leaf)
	if err != nil {
		tc.Close()
		s.rl.warn("identity", "nodes' port: the certificate names no node; connection closed",
			"peer", tc.RemoteAddr().String(), "err", err)
		return "", "", nil, false
	}
	return node, pki.Serial(leaf), leaf, true
}

// acceptNode takes a node connection: a client certificate is required
// and must pass CheckNode; then the gRPC channel over it must reach READY
// before the connection is registered.
func (s *Server) acceptNode(tc *tls.Conn, state tls.ConnectionState) {
	addr := tc.RemoteAddr().String()
	if len(state.PeerCertificates) == 0 {
		tc.Close()
		s.rl.warn("nocert", "nodes' port: node connection without a certificate; closed", "peer", addr)
		return
	}
	node, serial, leaf, ok := s.identify(tc, state)
	if !ok {
		return
	}
	gen := s.genOf(node)
	revokeAt, err := s.check(s.ctx, node, serial, addr, true)
	if err != nil {
		tc.Close()
		s.rl.warn("refused", "nodes' port: node connection refused", "node", node, "serial", serial, "peer", addr, "err", err)
		return
	}
	l := &link{
		kind: nodeLink, node: node, serial: serial, addr: addr,
		notBefore: leaf.NotBefore, notAfter: leaf.NotAfter, raw: tc,
	}
	if err := s.dial(l, tc, state); err != nil {
		tc.Close()
		s.rl.warn("ready", "nodes' port: node connection did not get ready; closed", "node", node, "peer", addr, "err", err)
		return
	}
	s.registerNode(l, revokeAt, gen)
}

// acceptH2 routes an "h2" connection: with a certificate to the uplink
// server, or to the refusal server when CheckNode refuses it definitively;
// without one to the join server.
func (s *Server) acceptH2(tc *tls.Conn, state tls.ConnectionState) {
	addr := tc.RemoteAddr().String()
	if len(state.PeerCertificates) == 0 {
		s.handLimited(tc, state, s.joinLis, "")
		return
	}
	node, serial, leaf, ok := s.identify(tc, state)
	if !ok {
		return
	}
	gen := s.genOf(node)
	revokeAt, err := s.check(s.ctx, node, serial, addr, true)
	if err != nil {
		if definitive(err) {
			s.rl.warn("refused", "nodes' port: uplink refused; answering from the refusal server", "node", node, "serial", serial, "peer", addr, "err", err)
			s.handLimited(tc, state, s.refusalLis, refusalText(err))
			return
		}
		tc.Close()
		s.rl.warn("check", "nodes' port: uplink not checked; closed", "node", node, "peer", addr, "err", err)
		return
	}
	l := &link{
		kind: uplinkLink, node: node, serial: serial, addr: addr,
		notBefore: leaf.NotBefore, notAfter: leaf.NotAfter,
	}
	sc := newSrvConn(tc, &authInfo{TLSInfo: tlsInfo(state), link: l}, func() { s.uplinkClosed(l) })
	l.raw = sc
	if !s.registerUplink(l, revokeAt, gen) {
		s.droppedUplinks.Add(1)
		tc.Close()
		s.rl.warn("uplinks", "nodes' port: too many uplinks of a node; closed", "node", node, "peer", addr)
		return
	}
	s.uplinkLis.hand(sc)
}

// handLimited hands a join or refusal connection to its server, within
// the shared limit of 64 such connections, each closed after 60 s.
func (s *Server) handLimited(tc *tls.Conn, state tls.ConnectionState, lis *chanListener, refusal string) {
	select {
	case s.joinSlots <- struct{}{}:
	default:
		s.droppedJoins.Add(1)
		tc.Close()
		s.rl.warn("joins", "nodes' port: too many join connections; closed", "peer", tc.RemoteAddr().String())
		return
	}
	sc := newSrvConn(tc, &authInfo{TLSInfo: tlsInfo(state), refusal: refusal}, func() { <-s.joinSlots })
	if !s.goOwned(func() {
		t := time.NewTimer(s.joinLifetime)
		defer t.Stop()
		select {
		case <-t.C:
			sc.Close()
		case <-sc.done:
		case <-s.ctx.Done():
			sc.Close()
		}
	}) {
		sc.Close()
		return
	}
	lis.hand(sc)
}

// check runs CheckNode within the port's limit of 32 concurrent checks,
// waiting at most checkWait for a slot. At accept, a certificate that
// passes is reported to OnPresented in the same slot: what an accept does
// in the database stays within the limit, however fast a node redials.
func (s *Server) check(ctx context.Context, node, serial, addr string, atAccept bool) (time.Time, error) {
	t := time.NewTimer(s.checkWait)
	select {
	case s.checks <- struct{}{}:
		t.Stop()
	case <-t.C:
		return time.Time{}, errors.New("too many node checks in flight")
	case <-ctx.Done():
		t.Stop()
		return time.Time{}, ctx.Err()
	}
	defer func() { <-s.checks }()
	cctx, cancel := context.WithTimeout(ctx, s.checkTimeout)
	revokeAt, err := s.cfg.CheckNode(cctx, node, serial, addr, atAccept)
	cancel()
	if err == nil && atAccept && s.cfg.OnPresented != nil {
		s.cfg.OnPresented(node, serial, addr)
	}
	return revokeAt, err
}

// definitive reports whether a CheckNode error refuses for good.
func definitive(err error) bool {
	return errors.Is(err, model.ErrNodeRefused) || errors.Is(err, model.ErrCertSuperseded)
}

// Refusal returns a definitive refusal for CheckNode: it wraps
// model.ErrNodeRefused, and its text, exactly as given, is what the
// refusal server answers the node's uplink calls with, e.g. "node n1 was
// removed; join it again". An error that wraps model.ErrNodeRefused the
// usual way answers with its text too (refusalText).
func Refusal(format string, args ...any) error {
	return &refusalError{msg: fmt.Sprintf(format, args...)}
}

type refusalError struct{ msg string }

func (e *refusalError) Error() string { return e.msg }
func (e *refusalError) Unwrap() error { return model.ErrNodeRefused }

// refusalText is what the refusal server answers for a definitive refusal
// at accept: the text of a Refusal as given, or the error's text without
// the sentinel's when it wraps one the usual way ("node refused: <text>"
// or "<text>: node refused"), so that the node reads exactly "node n1 was
// removed; join it again" (§4).
func refusalText(err error) string {
	var r *refusalError
	if errors.As(err, &r) {
		return r.msg
	}
	text := err.Error()
	for _, sentinel := range []error{model.ErrNodeRefused, model.ErrCertSuperseded} {
		text = strings.TrimPrefix(text, sentinel.Error()+": ")
		text = strings.TrimSuffix(text, ": "+sentinel.Error())
	}
	return text
}

// Shutdown closes the node connections at once, stops the uplink, join and
// refusal servers (gracefully, then forcibly after 5 s or when ctx ends)
// and closes the listener; it waits for the port's goroutines until ctx
// ends.
func (s *Server) Shutdown(ctx context.Context) {
	s.mu.Lock()
	if s.shut {
		s.mu.Unlock()
		return
	}
	s.shut = true
	var d deferred
	for _, st := range s.nodes {
		if st.retry != nil {
			st.retry.Stop()
		}
		for _, l := range append([]*link(nil), st.links...) {
			s.closeLinkLocked(l, "instance shutting down", &d)
		}
	}
	s.mu.Unlock()
	d.run()
	s.lis.Close()
	s.cancel()

	var wg sync.WaitGroup
	for _, g := range []*grpc.Server{s.uplink, s.join, s.refusal} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := make(chan struct{})
			go func() { g.GracefulStop(); close(done) }()
			t := time.NewTimer(5 * time.Second)
			defer t.Stop()
			select {
			case <-done:
			case <-t.C:
				g.Stop()
				<-done
			case <-ctx.Done():
				g.Stop()
				<-done
			}
		}()
	}
	wg.Wait()
	s.uplinkLis.Close()
	s.joinLis.Close()
	s.refusalLis.Close()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// deferred collects what must run after Server.mu is released: closing
// connections (a TLS close may wait for the peer) and callbacks.
type deferred []func()

func (d *deferred) add(f func()) { *d = append(*d, f) }

func (d deferred) run() {
	for _, f := range d {
		f()
	}
}
