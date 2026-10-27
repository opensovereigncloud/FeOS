// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
)

// renewCertificate is RenewCertificate (§8): the node, named by the
// certificate it calls with, gets a new certificate for the key of its
// signing request (it may be a new key). The presenting certificate must
// still pass CheckNode's conditions, checked again under the node's row
// lock; a removed node is told to join again. The renewal is recorded in
// node_certs, the node's renewal history, and in an activity row.
func (s *Service) renewCertificate(ctx context.Context, req *fleetv1.RenewCertificateRequest) (*fleetv1.RenewCertificateResponse, error) {
	p, err := nodePeer(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	var cert *fleetv1.Certificate
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		cert = nil
		if _, err := tx.LockNode(ctx, p.Node, store.ForUpdate); err != nil {
			if errors.Is(err, store.ErrNoRow) {
				return status.Errorf(codes.PermissionDenied, "node %s is unknown; join it again", p.Node)
			}
			return err
		}
		if err := checkInTx(ctx, tx, p.Node, p.Serial); err != nil {
			return err
		}
		_, keyHash, err := pki.CheckCSR(req.GetCsr(), true)
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		c, leaf, err := s.iss.IssueNode(req.GetCsr(), p.Node, s.cfg.NodeCertTTL)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "issue the node's certificate: %v", err)
		}
		if _, err := tx.InsertNodeCert(ctx, store.NodeCert{
			Serial: pki.Serial(leaf), Node: p.Node, Kind: store.CertRenew, KeyHash: keyHash,
			PreviousSerial: p.Serial, Peer: p.Addr, NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter,
		}); err != nil {
			return err
		}
		if err := s.recordNodeCert(ctx, tx, p.Node, store.CertRenew, leaf, keyHash, p.Addr, p.Serial); err != nil {
			return err
		}
		cert = c
		return nil
	})
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() != codes.Unavailable {
			s.rl.warn("renew", "certificate renewal refused", "node", p.Node, "serial", p.Serial, "code", st.Code().String(), "err", st.Message())
			return nil, st.Err()
		}
		return nil, rpcError(err, "renew the certificate")
	}
	s.log.Info("node renewed its certificate", "node", p.Node, "previous_serial", p.Serial)
	return &fleetv1.RenewCertificateResponse{Certificate: cert}, nil
}
