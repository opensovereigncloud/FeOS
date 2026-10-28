// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// identity is what the instance authenticates with: its key, its
// certificate with the chain up to the trust bundle, and the bundle.
type identity struct {
	name       string
	key        crypto.Signer
	cert       tls.Certificate
	leaf       *x509.Certificate
	bundle     *x509.CertPool
	wire       wire.Certificate
	renewAfter time.Time
}

// newIdentity checks an issued certificate (the URI of ingress name in
// the trust domain, the key the instance holds, a trust bundle) and makes
// the identity.
func newIdentity(td, name string, key crypto.Signer, c wire.Certificate) (*identity, error) {
	if key == nil {
		return nil, errors.New("no key")
	}
	if len(c.Chain) == 0 {
		return nil, errors.New("no certificate")
	}
	leaf, err := x509.ParseCertificate(c.Chain[0])
	if err != nil {
		return nil, fmt.Errorf("the certificate: %w", err)
	}
	got, err := pki.ParseIngressID(td, leaf)
	if err != nil {
		return nil, fmt.Errorf("the certificate: %w", err)
	}
	if got != name {
		return nil, fmt.Errorf("the certificate is ingress %q's, not %q's", got, name)
	}
	pub, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(key.Public()) {
		return nil, errors.New("the certificate is not for the key in " + keyFile)
	}
	if len(c.TrustBundle) == 0 {
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
	renewAfter := c.RenewAfter
	if renewAfter.IsZero() {
		renewAfter = leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) / 2)
	}
	return &identity{
		name:       name,
		key:        key,
		cert:       tls.Certificate{Certificate: append([][]byte(nil), c.Chain...), PrivateKey: key, Leaf: leaf},
		leaf:       leaf,
		bundle:     pool,
		wire:       c,
		renewAfter: renewAfter,
	}, nil
}

// serial is the certificate's serial as the control plane keeps it.
func (id *identity) serial() string { return pki.Serial(id.leaf) }

// expired reports whether the certificate is no longer valid at now.
func (id *identity) expired(now time.Time) bool { return !now.Before(id.leaf.NotAfter) }

// untrustedError is a control plane that failed the pin check of the
// join connection's TLS handshake: the token was not sent to it.
type untrustedError struct{ err error }

func (e *untrustedError) Error() string { return e.err.Error() }
func (e *untrustedError) Unwrap() error { return e.err }

// bundlePinError is a Join answered with a trust bundle that lacks the
// root the token pins: the token was spent on that control plane.
type bundlePinError struct{ err error }

func (e *bundlePinError) Error() string { return "the join's answer: " + e.err.Error() }
func (e *bundlePinError) Unwrap() error { return e.err }

// joinToken is a token read from the token file.
type joinToken struct {
	sent, pin, hash string
}

// nextToken reads the token file and returns its token unless it was
// tried; ok false with the reason otherwise.
func (a *Agent) nextToken() (tok joinToken, reason string, ok bool) {
	if a.cfg.JoinTokenFile == "" {
		return joinToken{}, "no join token file is configured", false
	}
	data, err := os.ReadFile(a.cfg.JoinTokenFile)
	if errors.Is(err, fs.ErrNotExist) {
		return joinToken{}, "the join token file does not exist", false
	}
	if err != nil {
		return joinToken{}, fmt.Sprintf("read the join token file: %v", err), false
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return joinToken{}, "the join token file is empty", false
	}
	sent, pin, err := pki.SplitIngressJoinToken(raw)
	if err != nil {
		return joinToken{}, fmt.Sprintf("the join token file: %v", err), false
	}
	hash := pki.HashJoinSecret(sent)
	a.mu.Lock()
	tried := a.tried[hash]
	a.mu.Unlock()
	if tried {
		return joinToken{}, "the join token in the file was tried already; it needs a new one", false
	}
	return joinToken{sent: sent, pin: pin, hash: hash}, "", true
}

// markTried records a token as sent, in memory and in join-tried.
func (a *Agent) markTried(hash string) {
	a.mu.Lock()
	a.tried[hash] = true
	a.mu.Unlock()
	if err := a.dir.addTried(hash); err != nil {
		a.log.Error("ingress agent: record the join token as tried", "err", err)
	}
}

// joinLoop joins with a token from the token file: it returns nil once
// the instance holds a certificate, or ctx's error. A token is sent only
// to a server that passed the pin check, and never again once a Join with
// it got an answer or a refusal (it is then recorded in join-tried); a
// Join without an answer, or whose certificate or new WireGuard key could
// not be kept, is sent again with the same token and key, which the
// control plane answers again. A join makes a new WireGuard key before it
// keeps the certificate, so that the first Hello after any join brings
// one: a join again retires the key before it, and a copy of the state
// directory, wg.key with it, does not stay on the mesh.
func (a *Agent) joinLoop(ctx context.Context) error {
	a.setJoining(true)
	defer a.setJoining(false)
	b := newBackoff(a.tune.backoffMin, a.tune.backoffMax)
	var noted string
	for {
		tok, reason, ok := a.nextToken()
		if !ok {
			if reason != noted {
				a.log.Warn("ingress agent: no valid certificate; waiting for a join token", "reason", reason, "file", a.cfg.JoinTokenFile)
				noted = reason
			}
			a.setLastError(reason)
			if !sleep(ctx, a.tune.tokenPoll) {
				return ctx.Err()
			}
			continue
		}
		noted = ""
		id, err := a.joinOnce(ctx, tok)
		if err == nil {
			if err := a.newMeshKey("joined"); err != nil {
				a.log.Error("ingress agent: make a new WireGuard key after the join", "err", err)
				if !b.wait(ctx) {
					return ctx.Err()
				}
				continue
			}
			if err := a.dir.saveCert(id.wire); err != nil {
				// The control plane answers the same key again for a
				// while; until the certificate is kept, try again.
				a.log.Error("ingress agent: keep the certificate", "err", err)
				if !b.wait(ctx) {
					return ctx.Err()
				}
				continue
			}
			a.markTried(tok.hash)
			a.joined(id)
			a.log.Info("ingress agent: joined", "name", id.name, "serial", id.serial(), "notAfter", id.leaf.NotAfter)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var (
			ue *untrustedError
			be *bundlePinError
		)
		switch {
		case errors.As(err, &ue):
			a.setLastError(err.Error())
			a.rl.error("join-untrusted", "ingress agent: the control plane is not the one the join token pins; the token was not sent", "err", err)
		case errors.As(err, &be):
			a.markTried(tok.hash)
			a.setLastError(err.Error())
			a.log.Error("ingress agent: the control plane answered the join with a trust bundle that lacks the root the token pins; the token was spent on it and will not be sent again, the instance needs a new one",
				"err", err)
			continue
		case isDefinitiveJoinError(err):
			a.markTried(tok.hash)
			a.setLastError(err.Error())
			a.log.Error("ingress agent: the join was refused; the token will not be sent again, the instance needs a new one",
				"code", status.Code(err).String(), "err", status.Convert(err).Message())
			continue
		default:
			a.setLastError(err.Error())
			a.rl.warn("join", "ingress agent: join failed; retrying", "err", err)
		}
		if !b.wait(ctx) {
			return ctx.Err()
		}
	}
}

// isDefinitiveJoinError reports whether the control plane answered a Join
// with a refusal, after which the token is spent; an answer the instance
// cannot take counts as one.
func isDefinitiveJoinError(err error) bool {
	var (
		me *joinAnswerError
		be *bundlePinError
	)
	if errors.As(err, &me) || errors.As(err, &be) {
		return true
	}
	switch status.Code(err) {
	case codes.PermissionDenied, codes.AlreadyExists, codes.InvalidArgument:
		return true
	}
	return false
}

// joinAnswerError is an answer to a Join that the instance cannot take:
// the token was spent on it.
type joinAnswerError struct{ err error }

func (e *joinAnswerError) Error() string { return "the join's answer: " + e.err.Error() }
func (e *joinAnswerError) Unwrap() error { return e.err }

// joinOnce sends the Join to the addresses in random order until one
// answers. An untrusted server, a refusal or an answer ends the round.
func (a *Agent) joinOnce(ctx context.Context, tok joinToken) (*identity, error) {
	a.mu.Lock()
	key := a.key
	a.mu.Unlock()
	csr, err := pki.CreateCSR(key, a.cfg.Name)
	if err != nil {
		return nil, err
	}
	req := &wire.JoinRequest{Token: tok.sent, Name: a.cfg.Name, CSR: csr}
	var errs []error
	for _, addr := range a.order(a.cfg.ControlPlane) {
		resp, err := a.callJoin(ctx, addr, tok.pin, req)
		if err == nil {
			if err := pki.CheckBundlePin(resp.Certificate.TrustBundle, tok.pin); err != nil {
				return nil, &bundlePinError{fmt.Errorf("%s: %w", addr, err)}
			}
			if resp.Name != a.cfg.Name {
				return nil, &joinAnswerError{fmt.Errorf("the token is pinned to ingress %q, this instance is %q", resp.Name, a.cfg.Name)}
			}
			id, err := newIdentity(a.cfg.TrustDomain, a.cfg.Name, key, resp.Certificate)
			if err != nil {
				return nil, &joinAnswerError{err}
			}
			return id, nil
		}
		var ue *untrustedError
		if errors.As(err, &ue) {
			return nil, &untrustedError{fmt.Errorf("%s: %w", addr, ue.err)}
		}
		if isDefinitiveJoinError(err) || ctx.Err() != nil {
			return nil, err
		}
		errs = append(errs, fmt.Errorf("%s: %w", addr, err))
	}
	return nil, errors.Join(errs...)
}

// callJoin sends one Join to addr, over a connection whose server passed
// pki.VerifyJoinServer for the pin before anything is sent.
func (a *Agent) callJoin(ctx context.Context, addr, pin string, req *wire.JoinRequest) (*wire.JoinResponse, error) {
	cfg := &tls.Config{
		MinVersion:             tls.VersionTLS13,
		ServerName:             a.cfg.ControlPlaneName,
		NextProtos:             []string{wire.ALPN},
		SessionTicketsDisabled: true,
		// Before the join the instance has no trust bundle: the pin is
		// its trust anchor, and VerifyConnection checks the chain against
		// it.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if err := pki.VerifyJoinServer(pin, a.cfg.ControlPlaneName, a.cfg.TrustDomain, rawCerts(cs)); err != nil {
				return &untrustedError{err}
			}
			return nil
		},
	}
	conn, err := a.dialTLS(ctx, addr, cfg)
	if err != nil {
		return nil, err
	}
	// gRPC closes the connection only if it took it from the dialer,
	// which a Join that ends before it connects never makes it do.
	defer conn.Close()
	cc, err := onceClient(a.cfg.ControlPlaneName, conn)
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	cctx, cancel := context.WithTimeout(ctx, a.tune.joinTimeout)
	defer cancel()
	return wire.NewClient(cc).Join(cctx, req)
}

// renew renews the certificate on the session's connection, for the same
// key, and keeps the new one.
func (a *Agent) renew(ctx context.Context, s *session) (*identity, error) {
	old := s.id
	csr, err := pki.CreateCSR(old.key, old.name)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, a.tune.callTimeout)
	defer cancel()
	resp, err := s.client.RenewCertificate(cctx, &wire.RenewRequest{CSR: csr})
	if err != nil {
		return nil, err
	}
	id, err := newIdentity(a.cfg.TrustDomain, old.name, old.key, resp.Certificate)
	if err != nil {
		return nil, fmt.Errorf("the renewal's answer: %w", err)
	}
	// The renewal runs inside a session, and a loss is declared only
	// after the session ended: the identity is still old here.
	a.mu.Lock()
	current := a.id == old
	a.mu.Unlock()
	if !current {
		return nil, errors.New("the identity changed during the renewal")
	}
	if err := a.dir.saveCert(id.wire); err != nil {
		return nil, fmt.Errorf("keep the renewed certificate: %w", err)
	}
	a.mu.Lock()
	a.id = id
	a.lastRenew = time.Now()
	a.mu.Unlock()
	a.log.Info("ingress agent: renewed the certificate", "serial", id.serial(), "notAfter", id.leaf.NotAfter, "renewAfter", id.renewAfter)
	return id, nil
}

// renewDelay is how long until the identity is renewed: at its renew
// time, but never sooner than renewMinGap after the last renewal.
func (a *Agent) renewDelay(id *identity) time.Duration {
	a.mu.Lock()
	last := a.lastRenew
	a.mu.Unlock()
	at := id.renewAfter
	if !last.IsZero() {
		if gap := last.Add(a.tune.renewMinGap); gap.After(at) {
			at = gap
		}
	}
	return time.Until(at)
}
