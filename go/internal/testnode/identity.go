// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	fleetv1 "github.com/ironcore-dev/feos/go/gen/feos/fleet/v1"
	"github.com/ironcore-dev/feos/go/internal/pki"
)

// The errors with which Run ends for good.
var (
	// ErrUntrusted: the control plane did not show the root the join token
	// pins (on the join connection, or in the trust bundle it answered
	// with). The token is never sent to such a server.
	ErrUntrusted = errors.New("the control plane is not the one the join token pins")
	// ErrJoinRefused: the control plane refused the join (a token that is
	// unknown, used or expired, a name that exists, a malformed request).
	ErrJoinRefused = errors.New("the control plane refused the join")
	// ErrRefused: the control plane refused the node's certificate (the
	// node was removed, its certificate revoked, its identity forked): the
	// node must join again, and stops dialling until it has.
	ErrRefused = errors.New("the control plane refused the node; it must join again")
)

// identity is what the node authenticates with after its join: its key,
// its certificate with the chain up to the trust bundle, and the bundle.
type identity struct {
	name       string
	key        crypto.Signer
	cert       tls.Certificate
	leaf       *x509.Certificate
	bundleDER  [][]byte
	bundle     *x509.CertPool
	renewAfter time.Time
	// joinHash is the hash of the token the node joined with: a node given
	// another token joins again.
	joinHash string
}

// newIdentity checks an issued certificate (a node's URI name in the trust
// domain, the key the node holds, a trust bundle) and makes the identity.
func newIdentity(td string, key crypto.Signer, c *fleetv1.Certificate, joinHash string) (*identity, error) {
	if c == nil || len(c.GetChain()) == 0 {
		return nil, errors.New("no certificate")
	}
	leaf, err := x509.ParseCertificate(c.Chain[0])
	if err != nil {
		return nil, fmt.Errorf("the node's certificate: %w", err)
	}
	name, err := pki.ParseNodeID(td, leaf)
	if err != nil {
		return nil, fmt.Errorf("the node's certificate: %w", err)
	}
	pub, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(key.Public()) {
		return nil, errors.New("the node's certificate is not for the node's key")
	}
	if len(c.GetTrustBundle()) == 0 {
		return nil, errors.New("no trust bundle")
	}
	pool := x509.NewCertPool()
	for _, raw := range c.TrustBundle {
		root, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, fmt.Errorf("trust bundle: %w", err)
		}
		pool.AddCert(root)
	}
	renewAfter := leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) / 4 * 3)
	if c.RenewAfter != nil {
		renewAfter = c.RenewAfter.AsTime()
	}
	return &identity{
		name:       name,
		key:        key,
		cert:       tls.Certificate{Certificate: append([][]byte(nil), c.Chain...), PrivateKey: key, Leaf: leaf},
		leaf:       leaf,
		bundleDER:  append([][]byte(nil), c.TrustBundle...),
		bundle:     pool,
		renewAfter: renewAfter,
		joinHash:   joinHash,
	}, nil
}

// serial is the certificate's serial as the control plane keeps it.
func (id *identity) serial() string { return pki.Serial(id.leaf) }

// identityFile holds the identity in the state directory.
const identityFile = "identity.json"

type identityJSON struct {
	Key           []byte    `json:"key"` // PKCS #8, DER
	Chain         [][]byte  `json:"chain"`
	Bundle        [][]byte  `json:"bundle"`
	RenewAfter    time.Time `json:"renewAfter"`
	JoinTokenHash string    `json:"joinTokenHash,omitempty"`
}

// save keeps the identity in dir, replacing the file at once, so that a
// crash leaves the old identity or the new one.
func (id *identity) save(dir string) error {
	key, err := x509.MarshalPKCS8PrivateKey(id.key)
	if err != nil {
		return err
	}
	data, err := json.Marshal(identityJSON{Key: key, Chain: id.cert.Certificate, Bundle: id.bundleDER, RenewAfter: id.renewAfter, JoinTokenHash: id.joinHash})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, identityFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, identityFile)); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// loadIdentity reads the identity kept in dir; an error wrapping
// os.ErrNotExist when there is none.
func loadIdentity(dir, td string) (*identity, error) {
	data, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return nil, err
	}
	var j identityJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("%s: %w", identityFile, err)
	}
	k, err := x509.ParsePKCS8PrivateKey(j.Key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", identityFile, err)
	}
	key, ok := k.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%s: the key is no signer", identityFile)
	}
	c := &fleetv1.Certificate{Chain: j.Chain, TrustBundle: j.Bundle}
	if !j.RenewAfter.IsZero() {
		c.RenewAfter = timestamppb.New(j.RenewAfter)
	}
	return newIdentity(td, key, c, j.JoinTokenHash)
}

// SaveIdentity keeps a node identity in a state directory, as a join
// does: the key and a certificate issued for it with its trust bundle. A
// node given that directory takes it instead of joining (tests issue the
// certificate themselves).
func SaveIdentity(dir, trustDomain string, key crypto.Signer, cert *fleetv1.Certificate) error {
	id, err := newIdentity(trustDomain, key, cert, "")
	if err != nil {
		return err
	}
	return id.save(dir)
}

// untrustedError is a control plane that failed the node's check on the
// join connection.
type untrustedError struct{ err error }

func (e *untrustedError) Error() string { return e.err.Error() }
func (e *untrustedError) Unwrap() error { return e.err }

// rawCerts are the DER certificates a peer presented.
func rawCerts(cs tls.ConnectionState) [][]byte {
	raw := make([][]byte, len(cs.PeerCertificates))
	for i, c := range cs.PeerCertificates {
		raw[i] = c.Raw
	}
	return raw
}

// loadOrJoin takes the identity kept in the state directory, unless the
// node was given another join token than the one it joined with, or joins.
func (n *Node) loadOrJoin(ctx context.Context) (*identity, error) {
	var joinHash string
	if n.cfg.JoinToken != "" {
		sent, _, err := pki.SplitJoinToken(n.cfg.JoinToken)
		if err != nil {
			return nil, err
		}
		joinHash = pki.HashJoinSecret(sent)
	}
	if n.cfg.StateDir != "" {
		id, err := loadIdentity(n.cfg.StateDir, n.cfg.TrustDomain)
		switch {
		case err == nil && (joinHash == "" || id.joinHash == joinHash):
			return id, nil
		case err == nil:
			n.log.Info("test node: given another join token than the one it joined with; joining again")
		case !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("the identity in %s: %w", n.cfg.StateDir, err)
		}
	}
	if joinHash == "" {
		return nil, errors.New("testnode: no join token, and no identity in the state directory")
	}
	id, err := n.join(ctx)
	if err != nil {
		return nil, err
	}
	if n.cfg.StateDir != "" {
		if err := id.save(n.cfg.StateDir); err != nil {
			return nil, fmt.Errorf("keep the identity: %w", err)
		}
	}
	return id, nil
}

// join joins the node with its token, over a connection that
// authenticates the control plane by the token's pin, trying the
// configured addresses in turn. A server that fails the pin check never
// sees the token. A Join whose answer is lost is retried with the same
// key, which the control plane answers again.
func (n *Node) join(ctx context.Context) (*identity, error) {
	sent, pin, err := pki.SplitJoinToken(n.cfg.JoinToken)
	if err != nil {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := pki.CreateCSR(key, n.cfg.Name)
	if err != nil {
		return nil, err
	}
	req := &fleetv1.JoinRequest{Token: sent, Name: n.cfg.Name, Csr: csr}
	b := newBackoff(n.backoffMin, n.backoffMax)
	for i := 0; ; i++ {
		addr := n.cfg.Addrs[i%len(n.cfg.Addrs)]
		resp, err := n.callJoin(ctx, addr, pin, req)
		if err == nil {
			if err := pki.CheckBundlePin(resp.GetCertificate().GetTrustBundle(), pin); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrUntrusted, err)
			}
			id, err := newIdentity(n.cfg.TrustDomain, key, resp.GetCertificate(), pki.HashJoinSecret(sent))
			if err != nil {
				return nil, fmt.Errorf("the join's answer: %w", err)
			}
			if id.name != resp.GetName() {
				return nil, fmt.Errorf("the join's answer names node %q, its certificate %q", resp.GetName(), id.name)
			}
			n.log.Info("test node: joined", "name", id.name, "serial", id.serial())
			n.setInstances(resp.GetInstances())
			return id, nil
		}
		var ue *untrustedError
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("%w: %s: %v", ErrUntrusted, addr, ue.err)
		}
		switch status.Code(err) {
		case codes.PermissionDenied, codes.AlreadyExists, codes.InvalidArgument:
			return nil, fmt.Errorf("%w: %s", ErrJoinRefused, status.Convert(err).Message())
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		n.rl.warn("join", "test node: join failed; retrying", "addr", addr, "err", err)
		if !b.wait(ctx) {
			return nil, ctx.Err()
		}
	}
}

// callJoin sends one Join to addr, over a connection whose server passed
// pki.VerifyJoinServer for the pin before anything is sent.
func (n *Node) callJoin(ctx context.Context, addr, pin string, req *fleetv1.JoinRequest) (*fleetv1.JoinResponse, error) {
	cfg := &tls.Config{
		MinVersion:             tls.VersionTLS13,
		ServerName:             n.cfg.ServerName,
		NextProtos:             []string{alpnUplink},
		SessionTicketsDisabled: true,
		// Before the join the node has no trust bundle: the pin is its
		// trust anchor, and VerifyConnection checks the chain against it.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if err := pki.VerifyJoinServer(pin, n.cfg.ServerName, n.cfg.TrustDomain, rawCerts(cs)); err != nil {
				return &untrustedError{err}
			}
			return nil
		},
	}
	conn, err := n.dialTLS(ctx, nil, addr, cfg)
	if err != nil {
		return nil, err
	}
	cc, err := onceClient(n.cfg.ServerName, conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// gRPC closes the connection only if it took it from the dialer, which
	// a Join that ends before it connects never makes it do.
	defer conn.Close()
	defer cc.Close()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return fleetv1.NewFleetServiceClient(cc).Join(cctx, req)
}

// onceClient makes a gRPC client over one connection that is already
// handshaken: its dialer gives the connection out once.
func onceClient(serverName string, conn net.Conn) (*grpc.ClientConn, error) {
	var used atomic.Bool
	return grpc.NewClient("passthrough:///"+serverName,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			if used.Swap(true) {
				return nil, errors.New("the join connection is gone")
			}
			return conn, nil
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDisableServiceConfig(),
	)
}
