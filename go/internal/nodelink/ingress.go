// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// ErrIngressRefused is what a definitive refusal of Config.CheckIngress
// wraps: the certificate verifies, but this instance refuses it for good
// (the ingress was removed, the certificate revoked, superseded past its
// grace, or not the ingress's). Any other error of CheckIngress is
// transient.
var ErrIngressRefused = errors.New("ingress refused")

// IngressRefusal returns a definitive refusal for CheckIngress: it wraps
// ErrIngressRefused, and its text, exactly as given, is what every call
// on the refused connection is answered with, e.g. "ingress in-0 was
// removed; join it again". An error that wraps ErrIngressRefused the usual
// way answers with its text too (ingressRefusalText).
func IngressRefusal(format string, args ...any) error {
	return &ingressRefusalError{msg: fmt.Sprintf(format, args...)}
}

type ingressRefusalError struct{ msg string }

func (e *ingressRefusalError) Error() string { return e.msg }
func (e *ingressRefusalError) Unwrap() error { return ErrIngressRefused }

// ingressRefusalText is what a refused ingress connection is answered
// with: the text of an IngressRefusal as given, or the error's text
// without the sentinel's when it wraps it the usual way.
func ingressRefusalText(err error) string {
	var r *ingressRefusalError
	if errors.As(err, &r) {
		return r.msg
	}
	text := err.Error()
	text = strings.TrimPrefix(text, ErrIngressRefused.Error()+": ")
	text = strings.TrimSuffix(text, ": "+ErrIngressRefused.Error())
	return text
}

// IngressPeer is the ferun-ingress instance a call comes from, from its
// verified certificate: never from a message.
type IngressPeer struct {
	Name, Serial, Addr string
	NotAfter           time.Time
	// Conn identifies the connection on this instance (unique while the
	// port runs) and Done is closed when the connection closes: together
	// they key what a handler keeps per connection.
	Conn uint64
	Done <-chan struct{}
}

type ingressKey struct{}

// ContextWithIngress returns ctx carrying p, as the ingress server's
// interceptors do; for tests of ingress handlers.
func ContextWithIngress(ctx context.Context, p IngressPeer) context.Context {
	return context.WithValue(ctx, ingressKey{}, p)
}

// IngressFromContext returns the ingress of a call on the ingress server.
func IngressFromContext(ctx context.Context) (IngressPeer, bool) {
	p, ok := ctx.Value(ingressKey{}).(IngressPeer)
	return p, ok
}

// ingressName is what the registry knows of one ingress name. Its fields
// are guarded by Server.mu; it is dropped once it has no connection and
// no accept in flight.
type ingressName struct {
	name  string
	conns map[*srvConn]*ingressConn
	// accepts counts the accept checks in flight and gen the
	// RecheckIngress calls: an accept whose check ran while a re-check
	// was asked for is re-checked after its registration.
	accepts int
	gen     uint64
	// passes numbers the re-check passes, failed is the number of the
	// latest pass with a transient error. retry re-checks the name with
	// backoff until a pass that began after that one got every answer.
	passes, failed uint64
	retry          *time.Timer
	retryDelay     time.Duration
}

// ingressConn is one registered ingress connection.
type ingressConn struct {
	name, serial, addr string
	notAfter           time.Time
	sc                 *srvConn
	peer               IngressPeer

	// mu: Server.mu
	closed bool
	// timer closes the connection at deadline: NotAfter, or the
	// certificate's revocation time when earlier. The deadline only ever
	// moves earlier, since a revocation is final: an older check's answer
	// that lands late never takes a newer one's revocation back.
	deadline time.Time
	timer    *time.Timer
}

// Ingress is the server of ferun-ingress connections with a certificate;
// the ingress service registers IngressService (wire.RegisterIngressServer)
// on it before Serve. Its calls carry the IngressPeer (IngressFromContext).
func (s *Server) Ingress() *grpc.Server { return s.ingress }

// IngressJoin is the server of ferun-ingress connections without a client
// certificate; the ingress service registers IngressService on it before
// Serve, of which only Join is served there.
func (s *Server) IngressJoin() *grpc.Server { return s.ingressJoin }

// acceptIngress routes a ferun-ingress/1 connection: without a
// certificate to the ingress join server; with an ingress certificate
// that CheckIngress accepts to the ingress server, registered; with one
// that CheckIngress refuses definitively to the ingress server as a
// refusal connection, every call on which is answered with the refusal.
// Anything else is closed, and so is every new connection once
// StopIngress was called: before any check, so that a stopping instance
// does no database work for it.
func (s *Server) acceptIngress(tc *tls.Conn, state tls.ConnectionState) {
	if s.stopIngress.Load() {
		tc.Close()
		return
	}
	addr := tc.RemoteAddr().String()
	if len(state.PeerCertificates) == 0 {
		s.handLimited(tc, state, s.ingressJoinLis, "")
		return
	}
	leaf := state.PeerCertificates[0]
	name, err := pki.ParseIngressID(s.cfg.TrustDomain, leaf)
	if err != nil {
		tc.Close()
		s.rl.warn("ingress-identity", "nodes' port: the certificate names no ingress; connection closed",
			"peer", addr, "err", err)
		return
	}
	serial := pki.Serial(leaf)
	gen, ok := s.beginIngressAccept(name)
	if !ok {
		tc.Close()
		return
	}
	revokeAt, err := s.checkIngress(s.ctx, name, serial, addr)
	if err != nil {
		s.endIngressAccept(name)
		if errors.Is(err, ErrIngressRefused) {
			s.rl.warn("ingress-refused", "nodes' port: ingress refused; answering with the refusal",
				"ingress", name, "serial", serial, "peer", addr, "err", err)
			text := ingressRefusalText(err)
			if text == "" {
				text = ErrIngressRefused.Error()
			}
			s.handLimited(tc, state, s.ingressLis, text)
			return
		}
		tc.Close()
		s.rl.warn("ingress-check", "nodes' port: ingress connection not checked; closed",
			"ingress", name, "serial", serial, "peer", addr, "err", err)
		return
	}
	ic := &ingressConn{name: name, serial: serial, addr: addr, notAfter: leaf.NotAfter}
	ic.sc = newSrvConn(tc, &authInfo{TLSInfo: tlsInfo(state)}, func() { s.closeIngress(ic, "connection closed") })
	if !s.registerIngress(ic, revokeAt, gen) {
		tc.Close()
		return
	}
	s.ingressLis.hand(ic.sc)
}

// checkIngress runs CheckIngress in one of the 32 check slots that node
// checks use too, waiting at most checkWait for it, for at most
// checkTimeout. Without CheckIngress every check fails transiently.
func (s *Server) checkIngress(ctx context.Context, name, serial, addr string) (time.Time, error) {
	if s.cfg.CheckIngress == nil {
		return time.Time{}, errors.New("this instance checks no ingress (no CheckIngress)")
	}
	release, err := s.checkSlot(ctx, "ingress")
	if err != nil {
		return time.Time{}, err
	}
	defer release()
	cctx, cancel := context.WithTimeout(ctx, s.checkTimeout)
	defer cancel()
	return s.cfg.CheckIngress(cctx, name, serial, addr)
}

func (s *Server) ingressLocked(name string) *ingressName {
	st := s.ingresses[name]
	if st == nil {
		st = &ingressName{name: name, conns: map[*srvConn]*ingressConn{}}
		s.ingresses[name] = st
	}
	return st
}

// dropIngressIdleLocked forgets a name with no connection and no accept
// in flight.
func (s *Server) dropIngressIdleLocked(st *ingressName) {
	if len(st.conns) > 0 || st.accepts > 0 {
		return
	}
	if st.retry != nil {
		st.retry.Stop()
		st.retry = nil
	}
	if s.ingresses[st.name] == st {
		delete(s.ingresses, st.name)
	}
}

// beginIngressAccept counts an accept check of name in flight and returns
// the name's re-check count before it; false once the port is shut down.
func (s *Server) beginIngressAccept(name string) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shut {
		return 0, false
	}
	st := s.ingressLocked(name)
	st.accepts++
	return st.gen, true
}

// endIngressAccept ends an accept that registers nothing.
func (s *Server) endIngressAccept(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.ingresses[name]; st != nil {
		st.accepts--
		s.dropIngressIdleLocked(st)
	}
}

// registerIngress ends an accept by registering its connection, unless
// the port is shut down or stops accepting ingress connections, or the
// name has its maximum of connections. A re-check asked for while the
// accept check ran is repeated for the connection.
func (s *Server) registerIngress(ic *ingressConn, revokeAt time.Time, gen uint64) bool {
	s.mu.Lock()
	st := s.ingressLocked(ic.name)
	st.accepts--
	if s.shut || s.stopIngress.Load() {
		s.dropIngressIdleLocked(st)
		s.mu.Unlock()
		return false
	}
	if len(st.conns) >= s.maxIngress {
		s.dropIngressIdleLocked(st)
		s.mu.Unlock()
		s.droppedIngress.Add(1)
		s.rl.warn("ingresses", "nodes' port: too many connections of an ingress; closed", "ingress", ic.name, "peer", ic.addr)
		return false
	}
	s.ingressSeq++
	ic.peer = IngressPeer{
		Name: ic.name, Serial: ic.serial, Addr: ic.addr, NotAfter: ic.notAfter,
		Conn: s.ingressSeq, Done: ic.sc.done,
	}
	ic.sc.info.ingress = &ic.peer
	st.conns[ic.sc] = ic
	s.armIngressLocked(ic, revokeAt)
	recheck := st.gen != gen
	s.mu.Unlock()
	s.log.Info("ingress connected", "ingress", ic.name, "serial", ic.serial, "peer", ic.addr)
	if recheck {
		s.goOwned(func() { s.RecheckIngress(s.ctx, ic.name) })
	}
	return true
}

// armIngressLocked sets the time the connection closes at: its NotAfter,
// or revokeAt when that is earlier. A time later than the one armed
// changes nothing.
func (s *Server) armIngressLocked(ic *ingressConn, revokeAt time.Time) {
	deadline, reason := ic.notAfter, "certificate expired"
	if !revokeAt.IsZero() && revokeAt.Before(deadline) {
		deadline, reason = revokeAt, "superseded certificate revoked"
	}
	if ic.timer != nil {
		if !deadline.Before(ic.deadline) {
			return
		}
		ic.timer.Stop()
		s.log.Info("ingress connection closes when its certificate is revoked",
			"ingress", ic.name, "serial", ic.serial, "peer", ic.addr, "revokeAt", revokeAt)
	}
	ic.deadline = deadline
	ic.timer = time.AfterFunc(time.Until(deadline), func() { s.closeIngress(ic, reason) })
}

// closeIngressLocked takes a connection out of the registry; it is closed
// after Server.mu is released, in a goroutine, since a TLS close may wait
// for the peer.
func (s *Server) closeIngressLocked(ic *ingressConn, reason string, d *deferred) {
	if ic.closed {
		return
	}
	ic.closed = true
	if ic.timer != nil {
		ic.timer.Stop()
	}
	if st := s.ingresses[ic.name]; st != nil {
		delete(st.conns, ic.sc)
		s.dropIngressIdleLocked(st)
	}
	s.log.Info("ingress connection closed", "ingress", ic.name, "serial", ic.serial, "peer", ic.addr, "reason", reason)
	d.add(func() { go ic.sc.Close() })
}

// closeIngress is closeIngressLocked with the lock taken. It is also how a
// connection leaves the registry when it closed, whoever closed it.
func (s *Server) closeIngress(ic *ingressConn, reason string) {
	var d deferred
	s.mu.Lock()
	s.closeIngressLocked(ic, reason, &d)
	s.mu.Unlock()
	d.run()
}

// RecheckIngress runs CheckIngress again for every connection of the
// ingress name on this instance and acts on the answers: a definitive
// refusal closes a connection at once; a revocation time makes it close
// then, when that is earlier than its certificate's NotAfter. Nothing
// else closes a connection: a transient error keeps it, and the name is
// re-checked again with backoff (1 s doubling to 30 s) until a pass that
// began after the failed one gets every answer. It returns when its pass
// has ended or ctx ends.
func (s *Server) RecheckIngress(ctx context.Context, name string) {
	s.mu.Lock()
	if s.shut {
		s.mu.Unlock()
		return
	}
	st := s.ingresses[name]
	if st == nil {
		s.mu.Unlock()
		return
	}
	st.gen++
	st.passes++
	pass := st.passes
	conns := make([]*ingressConn, 0, len(st.conns))
	for _, ic := range st.conns {
		conns = append(conns, ic)
	}
	s.mu.Unlock()

	transient := false
	for _, ic := range conns {
		revokeAt, err := s.checkIngress(ctx, name, ic.serial, ic.addr)
		if s.applyIngress(ic, revokeAt, err) {
			transient = true
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if transient {
		st.failed = max(st.failed, pass)
	}
	if s.ingresses[name] != st {
		return // no connection left: nothing to re-check
	}
	s.ingressRetryLocked(st, transient, pass)
}

// ingressRetryLocked arms the re-check of a name after a pass with a
// transient error, with backoff, and clears it after a pass that got every
// answer and began after the latest failed one.
func (s *Server) ingressRetryLocked(st *ingressName, transient bool, pass uint64) {
	if !transient {
		if pass > st.failed {
			st.retryDelay = 0
			if st.retry != nil {
				st.retry.Stop()
				st.retry = nil
			}
		}
		return
	}
	if s.shut || len(st.conns) == 0 {
		return
	}
	st.retryDelay = min(max(2*st.retryDelay, s.recheckMinBackoff), s.recheckMaxBackoff)
	if st.retry != nil {
		st.retry.Stop()
	}
	name := st.name
	st.retry = time.AfterFunc(st.retryDelay, func() { s.goOwned(func() { s.RecheckIngress(s.ctx, name) }) })
}

// applyIngress acts on one re-check's answer; it reports a transient
// error.
func (s *Server) applyIngress(ic *ingressConn, revokeAt time.Time, err error) (transient bool) {
	var d deferred
	defer func() { d.run() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if ic.closed {
		return false
	}
	switch {
	case err == nil:
		s.armIngressLocked(ic, revokeAt)
		return false
	case errors.Is(err, ErrIngressRefused):
		s.log.Info("ingress refused on re-check", "ingress", ic.name, "serial", ic.serial, "err", err)
		s.closeIngressLocked(ic, "refused: "+err.Error(), &d)
		return false
	default:
		s.rl.warn("ingress-recheck", "nodes' port: ingress re-check failed; connection kept, retrying",
			"ingress", ic.name, "serial", ic.serial, "err", err)
		return true
	}
}

// CloseIngress closes every connection of the ingress name on this
// instance, as when it was removed, and returns how many it closed.
func (s *Server) CloseIngress(name, reason string) int {
	var d deferred
	s.mu.Lock()
	n := 0
	if st := s.ingresses[name]; st != nil {
		for _, ic := range st.conns {
			s.closeIngressLocked(ic, reason, &d)
			n++
		}
	}
	s.mu.Unlock()
	d.run()
	return n
}

// StopIngress makes the port close every new ferun-ingress/1 connection
// at once, before any check, from now on (an instance that stops, D18).
// Established connections stay until they end or Shutdown.
func (s *Server) StopIngress() {
	if !s.stopIngress.Swap(true) {
		s.log.Info("nodes' port: accepting no new ingress connections")
	}
}

// IngressNames lists, sorted, the ingress names with a connection on this
// instance, registered or being accepted: what a re-check of every
// ingress re-checks.
func (s *Server) IngressNames() []string {
	s.mu.Lock()
	names := make([]string, 0, len(s.ingresses))
	for name := range s.ingresses {
		names = append(names, name)
	}
	s.mu.Unlock()
	sort.Strings(names)
	return names
}

// IngressConnections counts the registered connections of the ingress
// name on this instance.
func (s *Server) IngressConnections(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.ingresses[name]; st != nil {
		return len(st.conns)
	}
	return 0
}

// newIngressServer is the server of ingress connections with a
// certificate: IngressService without Join, in JSON.
func (s *Server) newIngressServer() *grpc.Server {
	return grpc.NewServer(
		grpc.Creds(serverCreds{}),
		grpc.ForceServerCodec(wire.Codec{}),
		grpc.MaxConcurrentStreams(16),
		grpc.MaxRecvMsgSize(4<<20),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.ChainUnaryInterceptor(s.ingressUnary),
		grpc.ChainStreamInterceptor(s.ingressStream),
	)
}

// newIngressJoinServer is the server of ingress connections without a
// client certificate: Join only, in JSON, with the join server's limits.
func (s *Server) newIngressJoinServer() *grpc.Server {
	opts := append(s.joinOptions(),
		grpc.ForceServerCodec(wire.Codec{}),
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if info.FullMethod != wire.MethodJoin {
				return nil, unimplemented(info.FullMethod)
			}
			return h(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			return unimplemented(info.FullMethod)
		}),
	)
	return grpc.NewServer(opts...)
}

// ingressCall admits a call on the ingress server: a refusal connection
// answers with its refusal, a connection without an ingress is not
// authenticated, and Join belongs to the join server.
func ingressCall(ctx context.Context, method string) (IngressPeer, error) {
	ai := authInfoFrom(ctx)
	if ai != nil && ai.refusal != "" {
		return IngressPeer{}, status.Error(codes.PermissionDenied, ai.refusal)
	}
	if ai == nil || ai.ingress == nil {
		return IngressPeer{}, status.Error(codes.Unauthenticated, "no ingress certificate on this connection")
	}
	if method == wire.MethodJoin {
		return IngressPeer{}, unimplemented(method)
	}
	return *ai.ingress, nil
}

func (s *Server) ingressUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	p, err := ingressCall(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	return h(ContextWithIngress(ctx, p), req)
}

func (s *Server) ingressStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
	p, err := ingressCall(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	return h(srv, &peerStream{ServerStream: ss, ctx: ContextWithIngress(ss.Context(), p)})
}
