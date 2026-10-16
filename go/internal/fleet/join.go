// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/ironcore-dev/feos/go/gen/feos/fleet/v1"
	"github.com/ironcore-dev/feos/go/internal/activity"
	"github.com/ironcore-dev/feos/go/internal/apierr"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/pki"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// DefaultJoinTokenTTL is a join token's lifetime when none is given.
const DefaultJoinTokenTTL = time.Hour

// IssueJoinToken makes a one-time join token, valid for ttl (an hour when
// 0), pinned to the name node unless node is "" (a pinned token re-joins
// that name; an unpinned one creates a new name only). token is what the
// operator gets, "feos-join-<secret>.<pin>", shown once: the database keeps
// only the hash of the part the node sends. An invalid node name is an
// *apierr.Error with status 400.
func (s *Service) IssueJoinToken(ctx context.Context, createdBy, node string, ttl time.Duration) (id uuid.UUID, token string, expiresAt time.Time, err error) {
	if node != "" && !pki.IsNodeName(node) {
		return uuid.Nil, "", time.Time{}, apierr.New(http.StatusBadRequest,
			"node %q is not a node name: 1 to 63 lowercase letters, digits and \"-\", beginning and ending with a letter or a digit", node)
	}
	if ttl <= 0 {
		ttl = DefaultJoinTokenTTL
	}
	token, _, hash, err := pki.NewJoinToken(s.iss.JoinPin())
	if err != nil {
		return uuid.Nil, "", time.Time{}, fmt.Errorf("make a join token: %w", err)
	}
	t, err := s.st.CreateJoinToken(ctx, hash, createdBy, node, ttl)
	if err != nil {
		return uuid.Nil, "", time.Time{}, fmt.Errorf("store the join token: %w", err)
	}
	return t.ID, token, t.ExpiresAt, nil
}

// join is Join (§8): over a connection without a client certificate, a
// join token and a signing request become the node's name and first
// certificate.
func (s *Service) join(ctx context.Context, req *fleetv1.JoinRequest) (*fleetv1.JoinResponse, error) {
	token, name, csrDER := req.GetToken(), req.GetName(), req.GetCsr()
	if !strings.HasPrefix(token, pki.JoinTokenPrefix) || len(token) != pki.JoinSecretLen {
		return nil, status.Error(codes.InvalidArgument, "the join token is not feos-join-<secret>: send it without the pin")
	}
	if !pki.IsNodeName(name) {
		return nil, status.Errorf(codes.InvalidArgument,
			"node name %q: 1 to 63 lowercase letters, digits and \"-\", beginning and ending with a letter or a digit", name)
	}
	if len(csrDER) == 0 || len(csrDER) > pki.MaxCSRBytes {
		return nil, status.Errorf(codes.InvalidArgument, "the certificate signing request must hold 1 to %d bytes", pki.MaxCSRBytes)
	}
	if !s.joins.allow() {
		s.rl.warn("join-limit", "join refused: too many joins", "peer", peerAddress(ctx))
		return nil, status.Error(codes.ResourceExhausted, "too many joins")
	}
	wait := time.NewTimer(s.joinSlotWait)
	select {
	case s.joinSlots <- struct{}{}:
		wait.Stop()
	case <-wait.C:
		s.rl.warn("join-limit", "join refused: too many joins", "peer", peerAddress(ctx))
		return nil, status.Error(codes.ResourceExhausted, "too many joins")
	case <-ctx.Done():
		wait.Stop()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	defer func() { <-s.joinSlots }()

	peerAddr := peerAddress(ctx)
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	hash := pki.HashJoinSecret(token)
	var resp *fleetv1.JoinResponse
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		resp = nil
		t, err := tx.LockJoinToken(ctx, hash)
		if errors.Is(err, store.ErrNoRow) {
			return status.Error(codes.PermissionDenied, "unknown join token")
		}
		if err != nil {
			return err
		}
		_, keyHash, err := pki.CheckCSR(csrDER, true)
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
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
			cert, _, err := s.issueNodeCert(ctx, tx, t, t.UsedName, first.Kind, csrDER, keyHash, peerAddr)
			if err != nil {
				return err
			}
			resp = &fleetv1.JoinResponse{Name: t.UsedName, Certificate: cert}
			return s.listInstances(ctx, tx, &resp.Instances)
		}
		if !t.ExpiresAt.After(now) {
			return status.Error(codes.PermissionDenied, "join token expired")
		}
		kind := store.CertJoin
		if t.NodeName != "" {
			// A pinned token is the operator's intent to join that name
			// again (a node that lost its key): every earlier
			// certificate of the name is revoked, the node's old
			// connections close everywhere.
			name, kind = t.NodeName, store.CertRejoin
			if err := tx.RejoinNode(ctx, name); err != nil {
				return err
			}
		} else {
			created, err := tx.InsertNode(ctx, name)
			if err != nil {
				return err
			}
			if !created {
				return status.Errorf(codes.AlreadyExists, "node %s exists; join it with a token pinned to its name", name)
			}
		}
		cert, serial, err := s.issueNodeCert(ctx, tx, t, name, kind, csrDER, keyHash, peerAddr)
		if err != nil {
			return err
		}
		if err := tx.MarkJoinTokenUsed(ctx, hash, name, keyHash, serial, peerAddr); err != nil {
			return err
		}
		resp = &fleetv1.JoinResponse{Name: name, Certificate: cert}
		return s.listInstances(ctx, tx, &resp.Instances)
	})
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() != codes.Unavailable {
			s.rl.warn("join", "join refused", "peer", peerAddr, "name", name, "code", st.Code().String(), "err", st.Message())
			return nil, st.Err()
		}
		s.rl.warn("join-failed", "join failed", "peer", peerAddr, "name", name, "err", err)
		return nil, rpcError(err, "join")
	}
	s.log.Info("node joined", "node", resp.Name, "peer", peerAddr)
	return resp, nil
}

// firstAnswer returns the certificate a used join token's first answer
// issued, when a repeat of that join may be answered again: the node's row,
// locked FOR UPDATE, is neither removed nor forked, and the certificate is
// the node's and not revoked, not even from a time ahead. Otherwise ok is
// false: a pinned re-join, a removal, a fork or the presentation of a newer
// certificate has overtaken the first answer, and answering again would
// hand the name back to whoever holds the token and the key.
func firstAnswer(ctx context.Context, tx *store.Store, t store.JoinToken) (store.NodeCert, bool, error) {
	n, err := tx.LockNode(ctx, t.UsedName, store.ForUpdate)
	if errors.Is(err, store.ErrNoRow) {
		return store.NodeCert{}, false, nil
	}
	if err != nil {
		return store.NodeCert{}, false, err
	}
	if n.Status == model.NodeRemoved || n.Forked() {
		return store.NodeCert{}, false, nil
	}
	c, err := tx.GetNodeCert(ctx, t.UsedSerial)
	if errors.Is(err, store.ErrNoRow) {
		return store.NodeCert{}, false, nil
	}
	if err != nil {
		return store.NodeCert{}, false, err
	}
	if c.Node != t.UsedName || !c.RevokedAt.IsZero() {
		return store.NodeCert{}, false, nil
	}
	return c, true, nil
}

// issueNodeCert issues the certificate of a join or re-join and records
// it with an activity row of the node.
func (s *Service) issueNodeCert(ctx context.Context, tx *store.Store, t store.JoinToken, name, kind string,
	csrDER []byte, keyHash, peerAddr string) (*fleetv1.Certificate, string, error) {
	cert, leaf, err := s.iss.IssueNode(csrDER, name, s.cfg.NodeCertTTL)
	if err != nil {
		return nil, "", status.Errorf(codes.InvalidArgument, "issue the node's certificate: %v", err)
	}
	serial := pki.Serial(leaf)
	if _, err := tx.InsertNodeCert(ctx, store.NodeCert{
		Serial: serial, Node: name, Kind: kind, KeyHash: keyHash, TokenID: t.ID, Peer: peerAddr,
		NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter,
	}); err != nil {
		return nil, "", err
	}
	if err := s.recordNodeCert(ctx, tx, name, kind, leaf, keyHash, peerAddr, ""); err != nil {
		return nil, "", err
	}
	return cert, serial, nil
}

// recordNodeCert writes the node-scoped activity row of an issued node
// certificate, in tx.
func (s *Service) recordNodeCert(ctx context.Context, tx *store.Store, node, kind string, leaf *x509.Certificate,
	keyHash, peerAddr, previous string) error {
	detail := map[string]any{
		"serial": pki.Serial(leaf), "kind": kind, "key_hash": keyHash, "peer": peerAddr,
		"not_after": leaf.NotAfter.UTC().Format(time.RFC3339),
	}
	if previous != "" {
		detail["previous_serial"] = previous
	}
	return s.act.RecordTx(ctx, tx.Q(), activity.Event{
		Node: node, Kind: activity.KindNodeCert,
		Name:   fmt.Sprintf("node certificate issued (%s)", kind),
		Detail: detail,
	})
}

// listInstances fills the instances nodes keep a node connection to: those
// with a node address, seen within the instance TTL, stopping ones
// included (§8 "Instances for nodes").
func (s *Service) listInstances(ctx context.Context, tx *store.Store, out *[]*fleetv1.ControlPlaneInstance) error {
	ins, err := tx.InstancesForNodes(ctx)
	if err != nil {
		return err
	}
	list := make([]*fleetv1.ControlPlaneInstance, 0, len(ins))
	for _, i := range ins {
		list = append(list, &fleetv1.ControlPlaneInstance{Id: i.ID, NodeAddress: i.NodeAddress})
	}
	*out = list
	return nil
}
