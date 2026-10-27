// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// The ALPN protocols of the nodes' port (fleet.proto).
const (
	alpnNode   = "feos-node/1"
	alpnUplink = "h2"
)

// identity is a node's key and certificate, as a join or a renewal gave
// them, with the trust bundle.
type identity struct {
	name   string
	cert   tls.Certificate
	leaf   *x509.Certificate
	bundle *x509.CertPool
}

// serial is the certificate's serial as the control plane keeps it.
func (id identity) serial() string { return pki.Serial(id.leaf) }

// rawCerts are the DER certificates of a TLS peer.
func rawCerts(cs tls.ConnectionState) [][]byte {
	out := make([][]byte, len(cs.PeerCertificates))
	for i, c := range cs.PeerCertificates {
		out[i] = c.Raw
	}
	return out
}

// onceConn gives gRPC one connection that is already handshaken.
func onceConn(serverName string, conn net.Conn) (*grpc.ClientConn, error) {
	var used atomic.Bool
	return grpc.NewClient("passthrough:///"+serverName,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			if used.Swap(true) {
				return nil, errors.New("the connection is gone")
			}
			return conn, nil
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDisableServiceConfig(),
	)
}

// rawJoin sends one Join to the nodes' port at addr, as a node does: on a
// new join connection whose server passed pki.VerifyJoinServer for the
// token's pin before anything is sent.
func (c *cluster) rawJoin(addr, token, name string, key ed25519.PrivateKey) (*fleetv1.JoinResponse, error) {
	sent, pin, err := pki.SplitJoinToken(token)
	if err != nil {
		return nil, err
	}
	csr, err := pki.CreateCSR(key, name)
	if err != nil {
		return nil, err
	}
	serverName := c.issuer.ControlPlaneDomain()
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13, ServerName: serverName, NextProtos: []string{alpnUplink},
		SessionTicketsDisabled: true,
		// The pin is the trust anchor before the join.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return pki.VerifyJoinServer(pin, serverName, trustDomain, rawCerts(cs))
		},
	}
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	conn, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	cc, err := onceConn(serverName, conn)
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	return fleetv1.NewFleetServiceClient(cc).Join(ctx, &fleetv1.JoinRequest{Token: sent, Name: name, Csr: csr})
}

// joinIdentity joins name with token through addr with a new key and
// returns the identity the answer gives.
func (c *cluster) joinIdentity(addr, token, name string) identity {
	c.t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		c.t.Fatal(err)
	}
	resp, err := c.rawJoin(addr, token, name, key)
	if err != nil {
		c.t.Fatalf("join %s: %v", name, err)
	}
	return identityOf(c.t, name, key, resp.GetCertificate())
}

// identityOf makes an identity of a key and the certificate issued for
// it.
func identityOf(t *testing.T, name string, key ed25519.PrivateKey, cert *fleetv1.Certificate) identity {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.GetChain()[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	for _, raw := range cert.GetTrustBundle() {
		root, err := x509.ParseCertificate(raw)
		if err != nil {
			t.Fatal(err)
		}
		pool.AddCert(root)
	}
	return identity{name: name, cert: tls.Certificate{Certificate: cert.GetChain(), PrivateKey: key, Leaf: leaf},
		leaf: leaf, bundle: pool}
}

// clientTLS is a node's TLS configuration toward an instance: TLS 1.3
// without resumption, the control plane verified for its name and URI.
func (c *cluster) clientTLS(id identity, alpn string) *tls.Config {
	serverName := c.issuer.ControlPlaneDomain()
	return &tls.Config{
		MinVersion: tls.VersionTLS13, ServerName: serverName, RootCAs: id.bundle,
		Certificates: []tls.Certificate{id.cert}, NextProtos: []string{alpn}, SessionTicketsDisabled: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return pki.VerifyControlPlane(id.bundle, serverName, trustDomain, rawCerts(cs))
		},
	}
}

// rawUplink is a gRPC client of the uplink server at addr with the
// identity id: every dial a new TLS connection with ALPN h2.
func (c *cluster) rawUplink(addr string, id identity) fleetv1.FleetServiceClient {
	c.t.Helper()
	cfg := c.clientTLS(id, alpnUplink)
	cc, err := grpc.NewClient("passthrough:///"+cfg.ServerName,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDisableServiceConfig(),
	)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = cc.Close() })
	return fleetv1.NewFleetServiceClient(cc)
}

// nodeConn is a node connection the test holds itself: a TLS connection
// with ALPN feos-node/1 on which it serves an empty gRPC server, as a node
// serves its feos.* APIs.
type nodeConn struct {
	conn   *watched
	srv    *grpc.Server
	served chan struct{}
}

// dialNodeConn opens a node connection to addr with the identity id and
// serves it until it closes. The TLS handshake succeeds or fails here; a
// refusal of the certificate closes the connection right after it.
func (c *cluster) dialNodeConn(addr string, id identity) (*nodeConn, error) {
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	conn, err := (&tls.Dialer{Config: c.clientTLS(id, alpnNode)}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	w := &watched{Conn: conn, closed: make(chan struct{})}
	srv := grpc.NewServer(
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
	)
	nc := &nodeConn{conn: w, srv: srv, served: make(chan struct{})}
	go func() {
		defer close(nc.served)
		_ = srv.Serve(&oneConnListener{conn: w, done: make(chan struct{})})
	}()
	c.t.Cleanup(nc.close)
	return nc, nil
}

// closed is closed when the connection is closed, by either side.
func (nc *nodeConn) closed() <-chan struct{} { return nc.conn.closed }

func (nc *nodeConn) close() {
	nc.srv.Stop()
	_ = nc.conn.Close()
	<-nc.served
}

// watched is a connection that tells when it is closed: by a read that
// fails or by Close.
type watched struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (w *watched) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if err != nil {
		w.once.Do(func() { close(w.closed) })
	}
	return n, err
}

func (w *watched) Close() error {
	err := w.Conn.Close()
	w.once.Do(func() { close(w.closed) })
	return err
}

// oneConnListener hands out one connection, then blocks until closed.
type oneConnListener struct {
	mu   sync.Mutex
	conn net.Conn
	done chan struct{}
	once sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	conn := l.conn
	l.conn = nil
	l.mu.Unlock()
	if conn != nil {
		return conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return &net.TCPAddr{} }
