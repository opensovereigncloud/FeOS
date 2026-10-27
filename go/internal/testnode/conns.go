// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	portforwardv1 "github.com/gardenlinux/feos/go/gen/feos/portforward/v1"
	toolv1 "github.com/gardenlinux/feos/go/gen/feos/tool/v1"
	volumev1 "github.com/gardenlinux/feos/go/gen/feos/volume/v1"
	watchv1 "github.com/gardenlinux/feos/go/gen/feos/watch/v1"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// The ALPN protocols of the nodes' port (fleet.proto).
const (
	alpnNode   = "feos-node/1" // the node connection: the node is the HTTP/2 server
	alpnUplink = "h2"          // the uplink and the join: the node is the client
)

// TCP settings of the node's connections (§19.1 of the design): keepalive
// and TCP_USER_TIMEOUT notice a dead instance within seconds.
const (
	tcpKeepAlive     = 15 * time.Second
	tcpUserTimeout   = 20 * time.Second
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 10 * time.Second
)

// session is one stretch of the node's connections: from a start of
// dialling to a restart, a crash, a partition or the end of Run. It owns
// every goroutine and connection of that stretch.
type session struct {
	n *Node
	// ctx is the session's: its connections are served and its uplink
	// runs until it ends.
	ctx    context.Context
	cancel context.CancelFunc
	// dialCtx, within ctx, is the dialling of node connections. A graceful
	// end cancels it first, so that no node connection is dialled while
	// the old ones drain.
	dialCtx     context.Context
	stopDialing context.CancelFunc
	wg          sync.WaitGroup

	mu sync.Mutex
	// draining: a graceful end began, and no connection is dialled or
	// served any more; closed: the end cut every connection.
	draining bool
	closed   bool
	raw      map[*trackedConn]struct{} // every TCP connection dialled, for an abrupt end
	links    map[*nodeLink]struct{}
}

func (n *Node) newSession(parent context.Context) *session {
	ctx, cancel := context.WithCancel(parent)
	dialCtx, stopDialing := context.WithCancel(ctx)
	s := &session{
		n: n, ctx: ctx, cancel: cancel, dialCtx: dialCtx, stopDialing: stopDialing,
		raw: map[*trackedConn]struct{}{}, links: map[*nodeLink]struct{}{},
	}
	s.goOwned(s.manageTargets)
	s.goOwned(func() { n.uplinkLoop(s) })
	return s
}

func (s *session) goOwned(f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

// stop ends the session. Gracefully: the node stops dialling first, then
// every node connection gets a GOAWAY and its calls in flight finish,
// within timeout; the uplink closes after them. Otherwise every connection
// is cut at once.
func (s *session) stop(graceful bool, timeout time.Duration) {
	if graceful {
		// Nothing dialled from here on can become the control plane's
		// current connection to the node and be cut below: track refuses
		// new connections, and dialNode serves none.
		s.mu.Lock()
		s.draining = true
		links := make([]*nodeLink, 0, len(s.links))
		for l := range s.links {
			links = append(links, l)
		}
		s.mu.Unlock()
		s.stopDialing()
		gracefulStop(links, timeout)
		s.cancel()
		s.wg.Wait()
		s.closeRaw()
		return
	}
	s.closeRaw()
	s.cancel()
	s.wg.Wait()
}

// gracefulStop sends every link a GOAWAY and waits until their calls in
// flight finished, or timeout passed and they were cut.
func gracefulStop(links []*nodeLink, timeout time.Duration) {
	var wg sync.WaitGroup
	for _, l := range links {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.gracefulStop(timeout)
		}()
	}
	wg.Wait()
}

// closeRaw closes every TCP connection of the session, without a TLS
// goodbye, and refuses new ones.
func (s *session) closeRaw() {
	s.mu.Lock()
	s.closed = true
	conns := make([]*trackedConn, 0, len(s.raw))
	for c := range s.raw {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// trackedConn is a TCP connection of a session.
type trackedConn struct {
	net.Conn
	s    *session
	once sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.s.mu.Lock()
		delete(c.s.raw, c)
		c.s.mu.Unlock()
	})
	return err
}

// track keeps a dialled connection in the session; false once the session
// is ending.
func (s *session) track(c net.Conn) (*trackedConn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.draining {
		return nil, false
	}
	tc := &trackedConn{Conn: c, s: s}
	s.raw[tc] = struct{}{}
	return tc, true
}

func (s *session) linkAddrs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var addrs []string
	for l := range s.links {
		addrs = append(addrs, l.addr)
	}
	slices.Sort(addrs)
	return addrs
}

// dialTLS dials addr and runs the TLS handshake of cfg, whose first ALPN
// protocol must be the one negotiated. With a session, the connection
// belongs to it.
func (n *Node) dialTLS(ctx context.Context, s *session, addr string, cfg *tls.Config) (*tls.Conn, error) {
	d := &net.Dialer{
		Timeout:         dialTimeout,
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: tcpKeepAlive, Interval: tcpKeepAlive, Count: 3},
		Control: func(_, _ string, raw syscall.RawConn) error {
			setUserTimeout(raw, tcpUserTimeout)
			return nil
		},
	}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if s != nil {
		tc, ok := s.track(raw)
		if !ok {
			raw.Close()
			return nil, errors.New("the node's connections are ending")
		}
		raw = tc
	}
	conn := tls.Client(raw, cfg)
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		raw.Close()
		return nil, err
	}
	if got := conn.ConnectionState().NegotiatedProtocol; got != cfg.NextProtos[0] {
		raw.Close()
		return nil, fmt.Errorf("the control plane at %s negotiated ALPN %q, not %q", addr, got, cfg.NextProtos[0])
	}
	return conn, nil
}

// clientTLS is the node's TLS configuration toward an instance: TLS 1.3
// without resumption, so that every connection shows the node's current
// certificate; the instance verified for the configured name, whatever
// address is dialled, and for the control plane's URI, which only its
// certificates carry.
func (n *Node) clientTLS(id *identity, alpn string) *tls.Config {
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		ServerName:             n.cfg.ServerName,
		RootCAs:                id.bundle,
		Certificates:           []tls.Certificate{id.cert},
		NextProtos:             []string{alpn},
		SessionTicketsDisabled: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return pki.VerifyControlPlane(id.bundle, n.cfg.ServerName, n.cfg.TrustDomain, rawCerts(cs))
		},
	}
}

// newNodeServer is the gRPC server of one node connection: the node's
// feos.* services with feosd's routing, with the enforcement policy that
// accepts the control plane's pings every 10 s without streams (grpc-go's
// default would answer them with GOAWAY "too_many_pings"), and the
// guest's receive limit: its ToolService and ContainerService decode at
// most 4 MiB a message (tonic's default). A larger one fails here with
// RESOURCE_EXHAUSTED, in the guest with OUT_OF_RANGE; ferund refuses such
// a call before it makes it.
func (n *Node) newNodeServer() *grpc.Server {
	srv := grpc.NewServer(
		grpc.Creds(insecure.NewCredentials()),
		grpc.MaxRecvMsgSize(guestMaxRecvMsgSize),
		grpc.MaxConcurrentStreams(n.cfg.MaxStreams),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.ChainUnaryInterceptor(n.unaryInterceptor),
		grpc.ChainStreamInterceptor(n.streamInterceptor),
	)
	podv1.RegisterPodServiceServer(srv, n.pods)
	containerv1.RegisterContainerServiceServer(srv, n.container)
	toolv1.RegisterToolServiceServer(srv, n.tools)
	watchv1.RegisterWatchServiceServer(srv, n.watch)
	portforwardv1.RegisterPortForwardServiceServer(srv, n.forward)
	networkv1.RegisterMeshServiceServer(srv, n.mesh)
	networkv1.RegisterSecurityGroupServiceServer(srv, n.groups)
	if !n.cfg.NoNameService {
		networkv1.RegisterNameServiceServer(srv, n.names)
	}
	volumev1.RegisterVolumeServiceServer(srv, n.volumes)
	return srv
}

// guestMaxRecvMsgSize is the largest message the guest's services decode.
const guestMaxRecvMsgSize = 4 << 20

// nodeLink is one node connection: the node serves its gRPC server over
// the TLS connection it dialled.
type nodeLink struct {
	addr string
	conn *watchedConn
	srv  *grpc.Server
}

// isClosed reports whether the link's connection is closed.
func (l *nodeLink) isClosed() bool {
	select {
	case <-l.conn.closed:
		return true
	default:
		return false
	}
}

func (l *nodeLink) gracefulStop(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		l.srv.GracefulStop()
		close(done)
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		l.srv.Stop()
		<-done
	}
}

// dialNode opens a node connection to addr with the identity id and
// serves it until it closes or the session ends. A connection whose
// handshake ends after a graceful end of the session began is closed
// unserved: the control plane registers a node connection only once its
// HTTP/2 server answered, so it never sends a call there.
func (s *session) dialNode(ctx context.Context, addr string, id *identity) (*nodeLink, error) {
	conn, err := s.n.dialTLS(ctx, s, addr, s.n.clientTLS(id, alpnNode))
	if err != nil {
		return nil, err
	}
	l := &nodeLink{addr: addr, conn: newWatchedConn(conn), srv: s.n.newNodeServer()}
	s.mu.Lock()
	if s.draining || s.closed {
		s.mu.Unlock()
		l.conn.Close()
		return nil, errors.New("the node's connections are ending")
	}
	s.links[l] = struct{}{}
	s.mu.Unlock()
	s.goOwned(func() { s.serveLink(l) })
	return l, nil
}

func (s *session) serveLink(l *nodeLink) {
	lis := newOneConnListener(l.conn)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = l.srv.Serve(lis)
	}()
	select {
	case <-l.conn.closed:
	case <-s.ctx.Done():
	}
	lis.Close()
	l.srv.Stop()
	<-done
	s.mu.Lock()
	delete(s.links, l)
	s.mu.Unlock()
}

// manageTargets keeps one dialler per target address: the listed
// instances, or the bootstrap addresses. An instance no longer listed
// loses its connections gracefully. It ends when the session stops
// dialling.
func (s *session) manageTargets() {
	targets := map[string]context.CancelFunc{}
	for {
		addrs, changed := s.n.targets()
		want := map[string]bool{}
		for _, a := range addrs {
			want[a] = true
		}
		for a, cancel := range targets {
			if !want[a] {
				cancel()
				delete(targets, a)
			}
		}
		for _, a := range addrs {
			if _, ok := targets[a]; ok {
				continue
			}
			ctx, cancel := context.WithCancel(s.dialCtx)
			targets[a] = cancel
			s.goOwned(func() { s.runTarget(ctx, a) })
		}
		select {
		case <-changed:
		case <-s.dialCtx.Done():
			for _, cancel := range targets {
				cancel()
			}
			return
		}
	}
}

// runTarget keeps a node connection to addr until ctx ends: it redials
// with backoff when the connection ends, and dials a new one with the new
// certificate after a renewal, leaving the old one to drain on the control
// plane's side, which closes it. When the target is dropped (ctx ends while
// the session still dials), its connections get a GOAWAY, so that the
// control plane moves its new calls to another connection while the calls
// in flight finish; when the session ends, its end takes care of them.
func (s *session) runTarget(ctx context.Context, addr string) {
	n := s.n
	var mine []*nodeLink
	defer func() {
		if s.dialCtx.Err() != nil {
			return
		}
		gracefulStop(slices.DeleteFunc(mine, (*nodeLink).isClosed), n.gracefulTimeout)
	}()
	b := newBackoff(n.backoffMin, n.backoffMax)
	for ctx.Err() == nil {
		id, renewed := n.identityNow()
		l, err := s.dialNode(ctx, addr, id)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			n.rl.warn("dial "+addr, "test node: node connection failed; redialling", "addr", addr, "err", err)
			b.wait(ctx)
			continue
		}
		mine = append(slices.DeleteFunc(mine, (*nodeLink).isClosed), l)
		up := time.Now()
		select {
		case <-l.conn.closed:
			if time.Since(up) > n.backoffMax {
				b.reset()
			}
			n.rl.warn("closed "+addr, "test node: node connection closed; redialling", "addr", addr)
			b.wait(ctx)
		case <-renewed:
			b.reset()
		case <-ctx.Done():
			return
		}
	}
}
