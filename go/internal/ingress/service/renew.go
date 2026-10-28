// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
)

// RenewCertificate is the ingress's RenewCertificate (§5 of the ingress's
// design): the ingress, named by the certificate it calls with, gets a new
// certificate for the same key. Under the ingress's row lock the
// presenting certificate must still pass CheckIngress's conditions, and
// renewRefusal's: it is the ingress's newest certificate and not revoked,
// not even from a time ahead, the signing request is for its key, and its
// renew time is near. A copy of an older certificate, or of the newest one
// with another key, thus never yields a certificate that outlives the
// instance's own. The renewal is recorded in ingress_certs (kind renew,
// with the presenting certificate as its previous one). It revokes
// nothing: the new certificate supersedes the older ones when the ingress
// presents it in a Hello.
func (s *Service) RenewCertificate(ctx context.Context, req *wire.RenewRequest) (*wire.RenewResponse, error) {
	p, err := ingressPeer(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.CSR) == 0 || len(req.CSR) > pki.MaxCSRBytes {
		return nil, status.Errorf(codes.InvalidArgument, "the certificate signing request must hold 1 to %d bytes", pki.MaxCSRBytes)
	}
	_, keyHash, err := pki.CheckCSR(req.CSR, true)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	var cert wire.Certificate
	var serial string
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		cert, serial = wire.Certificate{}, ""
		g, err := tx.LockIngress(ctx, p.Name, store.ForUpdate)
		if err != nil {
			if errors.Is(err, store.ErrNoRow) {
				return status.Errorf(codes.PermissionDenied, "certificate %s is not ingress %s's", p.Serial, p.Name)
			}
			return err
		}
		if err := checkInTx(ctx, tx, p.Name, p.Serial); err != nil {
			return err
		}
		presented, err := tx.GetIngressCert(ctx, p.Serial)
		if errors.Is(err, store.ErrNoRow) {
			return status.Errorf(codes.PermissionDenied, "certificate %s is not ingress %s's", p.Serial, p.Name)
		}
		if err != nil {
			return err
		}
		newest := g.CurrentSerial
		if newest == "" {
			if newest, err = tx.NewestIngressCert(ctx, p.Name); err != nil {
				return err
			}
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		if err := renewRefusal(p.Name, presented, newest, keyHash, now, s.renewEarly); err != nil {
			return err
		}
		c, leaf, err := s.iss.IssueIngress(req.CSR, p.Name, s.cfg.CertTTL)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "issue the ingress's certificate: %v", err)
		}
		if _, err := tx.InsertIngressCert(ctx, store.IngressCert{
			Serial: pki.Serial(leaf), Ingress: p.Name, Kind: store.CertRenew, KeyHash: keyHash,
			PreviousSerial: p.Serial, Peer: p.Addr, NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter,
		}); err != nil {
			return err
		}
		cert, serial = wire.CertificateFromProto(c), pki.Serial(leaf)
		return nil
	})
	if err != nil {
		if st, ok := refused(err); ok {
			s.rl.warn("renew", "ingress certificate renewal refused", "ingress", p.Name, "serial", p.Serial,
				"code", st.Code().String(), "err", st.Message())
			return nil, st.Err()
		}
		return nil, rpcError(err, "renew the certificate")
	}
	s.log.Info("ingress renewed its certificate", "ingress", p.Name, "serial", serial, "previous_serial", p.Serial)
	return &wire.RenewResponse{Certificate: cert}, nil
}

// renewRefusal is why the certificate c of ingress name may not be renewed
// for the key with hash keyHash at now, the database's clock, or nil:
//   - c must not be revoked, not even from a time ahead (a superseded
//     certificate in its grace);
//   - c must be the ingress's newest certificate, newest: its current one
//     (the newest presented in a Hello), or, while it has none, its newest
//     one that is not revoked;
//   - the signing request must be for c's key: a new key comes only with
//     a join;
//   - its renew time (half its lifetime, as it told the ingress) less
//     early must have come, so that one certificate is renewed once per
//     lifetime, give or take a lost answer.
func renewRefusal(name string, c store.IngressCert, newest, keyHash string, now time.Time, early time.Duration) error {
	switch {
	case !c.RevokedAt.IsZero():
		return status.Errorf(codes.PermissionDenied, "certificate %s of ingress %s was superseded; renew the newest certificate", c.Serial,
			name)
	case c.Serial != newest:
		return status.Errorf(codes.PermissionDenied, "certificate %s is not the newest certificate of ingress %s; renew the newest one",
			c.Serial, name)
	case c.KeyHash != keyHash:
		return status.Errorf(codes.InvalidArgument,
			"the signing request is not for the key of certificate %s; a certificate is renewed for its key, a new key needs a join",
			c.Serial)
	}
	if from := c.NotBefore.Add(c.NotAfter.Sub(c.NotBefore) / 2).Add(-early); now.Before(from) {
		return status.Errorf(codes.FailedPrecondition, "certificate %s of ingress %s is renewed from %s on", c.Serial, name,
			from.UTC().Format(time.RFC3339))
	}
	return nil
}
