// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
)

// Peer is the node an uplink call comes from, from its verified
// certificate: never from a message.
type Peer struct {
	Node, Serial, Addr string
	NotAfter           time.Time
}

type peerKey struct{}

// ContextWithPeer returns ctx carrying p, as the uplink server's
// interceptors do; for tests of uplink handlers.
func ContextWithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// NodeFromContext returns the node of an uplink call.
func NodeFromContext(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(Peer)
	return p, ok
}

// authInfo is the AuthInfo of a connection handed to one of the port's
// servers: the TLS state and what nodelink knows of the connection.
type authInfo struct {
	credentials.TLSInfo
	link    *link        // uplinks
	refusal string       // refusal connections, of nodes and ingresses: the refusal's text
	ingress *IngressPeer // registered ingress connections
}

func tlsInfo(state tls.ConnectionState) credentials.TLSInfo {
	return credentials.TLSInfo{
		State:          state,
		CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity},
	}
}

func authInfoFrom(ctx context.Context) *authInfo {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	ai, _ := p.AuthInfo.(*authInfo)
	return ai
}

// serverCreds are the transport credentials of the port's servers: the
// connection arrives handshaken, so the handshake returns it as it is.
type serverCreds struct{}

func (serverCreds) ServerHandshake(c net.Conn) (net.Conn, credentials.AuthInfo, error) {
	sc, ok := c.(*srvConn)
	if !ok {
		return nil, nil, errors.New("nodelink: connection did not come through the nodes' port")
	}
	return sc, sc.info, nil
}

func (serverCreds) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("nodelink: server credentials")
}

func (serverCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls"}
}
func (c serverCreds) Clone() credentials.TransportCredentials { return c }
func (serverCreds) OverrideServerName(string) error           { return nil }

// linkCreds are the transport credentials of one node connection, where
// the instance is the gRPC client over the connection the node dialled.
type linkCreds struct{ state tls.ConnectionState }

func (c *linkCreds) ClientHandshake(_ context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return conn, tlsInfo(c.state), nil
}

func (*linkCreds) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("nodelink: client credentials")
}

func (*linkCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls"}
}
func (c *linkCreds) Clone() credentials.TransportCredentials { return &linkCreds{state: c.state} }
func (*linkCreds) OverrideServerName(string) error           { return nil }

// dial makes the gRPC channel of a node connection over the handshaken
// conn and waits until it is READY, at most readyTimeout. The dialer gives
// out the conn once: the node dials again, never the instance.
func (s *Server) dial(l *link, conn net.Conn, state tls.ConnectionState) error {
	var used atomic.Bool
	cc, err := grpc.NewClient("passthrough:///node-"+l.node,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			if used.Swap(true) {
				return nil, errors.New("the node's connection is gone; the node dials again")
			}
			return conn, nil
		}),
		grpc.WithTransportCredentials(&linkCreds{state: state}),
		grpc.WithIdleTimeout(s.idleTimeout),
		grpc.WithDisableServiceConfig(),
		grpc.WithKeepaliveParams(s.clientKeepalive),
	)
	if err != nil {
		return err
	}
	cc.Connect()
	ctx, cancel := context.WithTimeout(s.ctx, s.readyTimeout)
	defer cancel()
	for {
		st := cc.GetState()
		if st == connectivity.Ready {
			l.cc = cc
			return nil
		}
		if st == connectivity.TransientFailure || st == connectivity.Shutdown {
			cc.Close()
			return fmt.Errorf("channel %s", st)
		}
		if !cc.WaitForStateChange(ctx, st) {
			cc.Close()
			return fmt.Errorf("channel %s after %v", st, s.readyTimeout)
		}
	}
}

// newUplinkServer is the server of authenticated uplinks: FleetService
// without Join.
func (s *Server) newUplinkServer() *grpc.Server {
	return grpc.NewServer(
		grpc.Creds(serverCreds{}),
		grpc.MaxConcurrentStreams(16),
		grpc.MaxRecvMsgSize(2<<20),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.ChainUnaryInterceptor(s.uplinkUnary),
		grpc.ChainStreamInterceptor(s.uplinkStream),
	)
}

// joinOptions are the options of the join and the refusal server:
// MaxConcurrentStreams(2), MaxRecvMsgSize(64 KiB), ConnectionTimeout 10 s
// and MaxConnectionAge 30 s.
func (s *Server) joinOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.Creds(serverCreds{}),
		grpc.MaxConcurrentStreams(2),
		grpc.ConnectionTimeout(s.joinConnTimeout),
		grpc.MaxRecvMsgSize(64 << 10),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: s.joinMaxAge}),
	}
}

// newJoinServer is the server of connections without a client
// certificate: Join only.
func (s *Server) newJoinServer() *grpc.Server {
	opts := append(s.joinOptions(),
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if info.FullMethod != fleetv1.FleetService_Join_FullMethodName {
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

// newRefusalServer is the server of uplinks whose certificate verifies but
// is refused for good: every FleetService method answers
// PERMISSION_DENIED with the refusal, so that the node learns it has to
// join again.
func (s *Server) newRefusalServer() *grpc.Server {
	opts := append(s.joinOptions(), grpc.UnknownServiceHandler(func(_ any, ss grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(ss)
		if !strings.HasPrefix(method, "/"+fleetv1.FleetService_ServiceDesc.ServiceName+"/") {
			return unimplemented(method)
		}
		text := "node refused; join it again"
		if ai := authInfoFrom(ss.Context()); ai != nil && ai.refusal != "" {
			text = ai.refusal
		}
		return status.Error(codes.PermissionDenied, text)
	}))
	return grpc.NewServer(opts...)
}

func unimplemented(method string) error {
	return status.Errorf(codes.Unimplemented, "method %s not implemented", method[strings.LastIndex(method, "/")+1:])
}

// uplinkCall admits a call on an uplink: not on a connection the registry
// closed, not on a superseded one; it counts as active until done.
func (s *Server) uplinkCall(ctx context.Context, method string) (*link, error) {
	if method == fleetv1.FleetService_Join_FullMethodName {
		return nil, unimplemented(method)
	}
	ai := authInfoFrom(ctx)
	if ai == nil || ai.link == nil {
		return nil, status.Error(codes.Unauthenticated, "no node certificate on this connection")
	}
	l := ai.link
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case l.closed:
		return nil, status.Error(codes.Unavailable, "connection closed")
	case l.superseded:
		return nil, status.Error(codes.Unavailable, "certificate superseded; reconnect with the node's newest certificate")
	}
	l.active++
	if l.idleTimer != nil {
		l.idleTimer.Stop()
	}
	return l, nil
}

// uplinkDone ends a call; a superseded uplink whose last call ended is
// closed.
func (s *Server) uplinkDone(l *link) {
	var d deferred
	s.mu.Lock()
	l.active--
	if !l.closed && l.superseded && l.active == 0 {
		s.closeLinkLocked(l, "superseded certificate", &d)
	}
	s.mu.Unlock()
	d.run()
}

func (s *Server) uplinkUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	l, err := s.uplinkCall(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	defer s.uplinkDone(l)
	return h(ContextWithPeer(ctx, l.peer()), req)
}

func (s *Server) uplinkStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
	l, err := s.uplinkCall(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	defer s.uplinkDone(l)
	return h(srv, &peerStream{ServerStream: ss, ctx: ContextWithPeer(ss.Context(), l.peer())})
}

// peerStream is a server stream whose context carries the Peer.
type peerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (p *peerStream) Context() context.Context { return p.ctx }
