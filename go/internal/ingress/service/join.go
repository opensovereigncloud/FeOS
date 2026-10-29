// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Join is the ingress's Join (§5 of the ingress's design): over a
// connection without a client certificate, a one-time join token of kind
// ingress, "feos-ingress-join-<secret>" (a node's token is refused by its
// form), and a signing request become the instance's name and its first
// certificate. A token pinned to a name joins that name, created or again
// (store.RejoinIngress: every older certificate revoked, the WireGuard key
// cleared and retired, the /96 kept); an unpinned one creates the
// proposed name, or fails ALREADY_EXISTS. A used token answers again, with
// a new certificate for the name it gave, a Join with the same key within
// 10 minutes whose first answer may have been lost, as long as nothing
// overtook that answer. Joins are limited to 20 a second and 4 at once in
// the database.
func (s *Service) Join(ctx context.Context, req *wire.JoinRequest) (*wire.JoinResponse, error) {
	token, name, csrDER := req.Token, req.Name, req.CSR
	if strings.HasPrefix(token, pki.JoinTokenPrefix) {
		return nil, status.Error(codes.InvalidArgument, "this join token is for a node")
	}
	if !pki.IsIngressJoinSecret(token) {
		return nil, status.Error(codes.InvalidArgument, "the join token is not feos-ingress-join-<secret>: send it without the pin")
	}
	if !pki.IsNodeName(name) {
		return nil, status.Errorf(codes.InvalidArgument,
			"ingress name %q: 1 to 63 lowercase letters, digits and \"-\", beginning and ending with a letter or a digit", name)
	}
	if len(csrDER) == 0 || len(csrDER) > pki.MaxCSRBytes {
		return nil, status.Errorf(codes.InvalidArgument, "the certificate signing request must hold 1 to %d bytes", pki.MaxCSRBytes)
	}
	peerAddr := peerAddress(ctx)
	if !s.joins.allow() {
		s.rl.warn("join-limit", "ingress join refused: too many joins", "peer", peerAddr)
		return nil, status.Error(codes.ResourceExhausted, "too many joins")
	}
	wait := time.NewTimer(s.joinSlotWait)
	select {
	case s.joinSlots <- struct{}{}:
		wait.Stop()
	case <-wait.C:
		s.rl.warn("join-limit", "ingress join refused: too many joins", "peer", peerAddr)
		return nil, status.Error(codes.ResourceExhausted, "too many joins")
	case <-ctx.Done():
		wait.Stop()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	defer func() { <-s.joinSlots }()
	_, keyHash, err := pki.CheckCSR(csrDER, true)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	hash := pki.HashJoinSecret(token)
	var resp *wire.JoinResponse
	var kind string
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		resp, kind = nil, ""
		t, err := tx.LockJoinToken(ctx, hash)
		if errors.Is(err, store.ErrNoRow) {
			return status.Error(codes.PermissionDenied, "unknown join token")
		}
		if err != nil {
			return err
		}
		if t.Kind != model.JoinKindIngress {
			return status.Error(codes.PermissionDenied, "this join token is for a node")
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		if t.Used() {
			if t.UsedKeyHash != keyHash || !t.UsedAt.After(now.Add(-joinRetryWindow)) {
				return status.Error(codes.PermissionDenied, "join token already used")
			}
			// The first answer was lost: answer again, for the name it
			// gave, with a certificate of the same kind; revoke nothing.
			first, ok, err := firstAnswer(ctx, tx, t)
			if err != nil {
				return err
			}
			if !ok {
				return status.Error(codes.PermissionDenied, "join token already used")
			}
			cert, _, err := s.issueJoinCert(ctx, tx, t, t.UsedName, first.Kind, csrDER, keyHash, peerAddr)
			if err != nil {
				return err
			}
			resp, kind = &wire.JoinResponse{Name: t.UsedName, Certificate: cert}, first.Kind
			return nil
		}
		if !t.ExpiresAt.After(now) {
			return status.Error(codes.PermissionDenied, "join token expired")
		}
		joined, k := name, store.CertJoin
		if t.NodeName != "" {
			// A pinned token is the operator's intent to join that name
			// (again, when it exists: an instance that lost its state):
			// every earlier certificate of the name is revoked, its
			// connections close everywhere, and its next Hello must bring
			// a new WireGuard key: the copy of a stolen state directory
			// does not stay on the mesh.
			joined, k = t.NodeName, store.CertRejoin
			if err := tx.RejoinIngress(ctx, joined); err != nil {
				return err
			}
		} else {
			created, err := tx.InsertIngress(ctx, joined)
			if err != nil {
				return err
			}
			if !created {
				return status.Errorf(codes.AlreadyExists, "ingress %s exists; join it with a token pinned to its name", joined)
			}
		}
		cert, serial, err := s.issueJoinCert(ctx, tx, t, joined, k, csrDER, keyHash, peerAddr)
		if err != nil {
			return err
		}
		if err := tx.MarkJoinTokenUsed(ctx, hash, joined, keyHash, serial, peerAddr); err != nil {
			return err
		}
		resp, kind = &wire.JoinResponse{Name: joined, Certificate: cert}, k
		return nil
	})
	if err != nil {
		if st, ok := refused(err); ok {
			s.rl.warn("join", "ingress join refused", "peer", peerAddr, "name", name, "code", st.Code().String(), "err", st.Message())
			return nil, st.Err()
		}
		s.rl.warn("join-failed", "ingress join failed", "peer", peerAddr, "name", name, "err", err)
		return nil, rpcError(err, "join")
	}
	s.log.Info("ingress joined", "ingress", resp.Name, "kind", kind, "peer", peerAddr)
	return resp, nil
}

// firstAnswer returns the certificate a used join token's first answer
// issued, when a repeat of that join may be answered again: the
// ingress's row, locked FOR UPDATE, is not removed, and the certificate is
// the ingress's and not revoked, not even from a time ahead. Otherwise ok
// is false: a join again, a removal or the presentation of a newer
// certificate has overtaken the first answer, and answering again would
// hand the name back to whoever holds the token and the key.
func firstAnswer(ctx context.Context, tx *store.Store, t store.JoinToken) (store.IngressCert, bool, error) {
	g, err := tx.LockIngress(ctx, t.UsedName, store.ForUpdate)
	if errors.Is(err, store.ErrNoRow) {
		return store.IngressCert{}, false, nil
	}
	if err != nil {
		return store.IngressCert{}, false, err
	}
	if g.Status == model.IngressRemoved {
		return store.IngressCert{}, false, nil
	}
	c, err := tx.GetIngressCert(ctx, t.UsedSerial)
	if errors.Is(err, store.ErrNoRow) {
		return store.IngressCert{}, false, nil
	}
	if err != nil {
		return store.IngressCert{}, false, err
	}
	if c.Ingress != t.UsedName || !c.RevokedAt.IsZero() {
		return store.IngressCert{}, false, nil
	}
	return c, true, nil
}

// issueJoinCert issues the certificate of a join or a join again and
// records it in ingress_certs with the token and the peer.
func (s *Service) issueJoinCert(ctx context.Context, tx *store.Store, t store.JoinToken, name, kind string,
	csrDER []byte, keyHash, peerAddr string) (wire.Certificate, string, error) {
	cert, leaf, err := s.iss.IssueIngress(csrDER, name, s.cfg.CertTTL)
	if err != nil {
		return wire.Certificate{}, "", status.Errorf(codes.InvalidArgument, "issue the ingress's certificate: %v", err)
	}
	serial := pki.Serial(leaf)
	if _, err := tx.InsertIngressCert(ctx, store.IngressCert{
		Serial: serial, Ingress: name, Kind: kind, KeyHash: keyHash, TokenID: t.ID, Peer: peerAddr,
		NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter,
	}); err != nil {
		return wire.Certificate{}, "", err
	}
	return wire.CertificateFromProto(cert), serial, nil
}
