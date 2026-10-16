// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// refusalText is why a certificate check refuses a node for good, in the
// words the node reads (the refusal server's PERMISSION_DENIED, §4), or ""
// when nothing does. A superseded certificate is not refused here: within
// its grace it is valid, past it the caller decides (a fork at accept, a
// closed connection on a re-check).
func refusalText(node string, c store.NodeCertCheck) string {
	switch {
	case !c.Found:
		return fmt.Sprintf("certificate of node %s is unknown; join it again", node)
	case c.NodeStatus == model.NodeRemoved:
		return fmt.Sprintf("node %s was removed; join it again", node)
	case !c.ForkedAt.IsZero():
		return forkedText(node)
	case c.Revoked() && c.RevokedReason != store.RevokedSuperseded:
		return fmt.Sprintf("certificate of node %s was revoked (%s); join it again", node, c.RevokedReason)
	}
	return ""
}

func forkedText(node string) string {
	return fmt.Sprintf("identity of node %s forked; join it again with a token pinned to its name", node)
}

// supersededPast reports a superseded certificate past its grace.
func supersededPast(c store.NodeCertCheck) bool {
	return c.Revoked() && c.RevokedReason == store.RevokedSuperseded
}

// supersededText is what calls on a superseded certificate past its grace
// are answered with, as nodelink answers the calls on a superseded uplink.
const supersededText = "certificate superseded; reconnect with the node's newest certificate"

// checkInTx checks, in a transaction, that the certificate serial of node
// still passes CheckNode's conditions (§8 RenewCertificate,
// IssuePodCredentials): known, its revocation not in effect, the node
// neither removed nor forked. A superseded certificate within its grace
// passes, as it does at CheckNode: once the re-check that its supersession
// causes has marked the node's older uplinks superseded, nodelink refuses
// new calls on them before they get here (§4). It returns the gRPC error a
// call on it is refused with: PERMISSION_DENIED for a definitive refusal
// (the node must join again), UNAVAILABLE for a superseded certificate
// past its grace (the node must use its newest certificate).
func checkInTx(ctx context.Context, tx *store.Store, node, serial string) error {
	c, err := tx.CheckNodeCert(ctx, node, serial)
	if err != nil {
		return err
	}
	if text := refusalText(node, c); text != "" {
		return status.Error(codes.PermissionDenied, text)
	}
	if supersededPast(c) {
		return status.Error(codes.Unavailable, supersededText)
	}
	return nil
}

// CheckNode is nodelink.Config.CheckNode: whether the certificate serial of
// node may connect from peerAddr, decided in one statement on the database
// clock, never from a cache. It returns the certificate's revocation time
// when one lies ahead (a superseded certificate in its grace). A definitive
// refusal wraps model.ErrNodeRefused, with the text the refusal server
// answers; a re-check of a superseded certificate past its grace returns
// model.ErrCertSuperseded. Presented at accept (atAccept) past its grace,
// a superseded certificate means that two holders use the node's identity:
// the node is declared forked (§8 "Certificate lineage"). Any other error
// is transient.
func (s *Service) CheckNode(ctx context.Context, node, serial, peerAddr string, atAccept bool) (time.Time, error) {
	c, err := s.st.CheckNodeCert(ctx, node, serial)
	if err != nil {
		return time.Time{}, fmt.Errorf("check certificate %s of node %s: %w", serial, node, err)
	}
	if text := refusalText(node, c); text != "" {
		return time.Time{}, nodelink.Refusal("%s", text)
	}
	if supersededPast(c) {
		if !atAccept {
			return time.Time{}, fmt.Errorf("certificate %s of node %s: %w", serial, node, model.ErrCertSuperseded)
		}
		return s.fork(ctx, node, serial, peerAddr)
	}
	return c.RevokeAhead(), nil
}

// fork declares node's identity forked after its superseded certificate
// serial was presented past its grace in a new handshake: identity
// forked, every certificate revoked ("forked"), ferun_node sent; the
// connection is refused. The conditions are checked again under the node's
// row lock.
func (s *Service) fork(ctx context.Context, node, serial, peerAddr string) (time.Time, error) {
	var revokeAt time.Time
	var refusal string
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		revokeAt, refusal = time.Time{}, ""
		if _, err := tx.LockNode(ctx, node, store.ForUpdate); err != nil {
			return err
		}
		c, err := tx.CheckNodeCert(ctx, node, serial)
		if err != nil {
			return err
		}
		if refusal = refusalText(node, c); refusal != "" {
			return nil
		}
		if !supersededPast(c) {
			revokeAt = c.RevokeAhead()
			return nil
		}
		description := fmt.Sprintf("identity forked: a superseded certificate was presented at %s from %s; "+
			"join the node again with a token pinned to its name", c.Now.UTC().Format(time.RFC3339), peerAddr)
		if err := tx.ForkNode(ctx, node, description); err != nil {
			return err
		}
		refusal = forkedText(node)
		s.log.Warn("identity of a node forked: its superseded certificate was presented past its grace",
			"node", node, "serial", serial, "peer", peerAddr)
		return nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("record the fork of node %s: %w", node, err)
	}
	if refusal != "" {
		return time.Time{}, nodelink.Refusal("%s", refusal)
	}
	return revokeAt, nil
}

// OnPresented is nodelink.Config.OnPresented: node presented the
// certificate serial from peerAddr in a handshake that passed CheckNode.
// It records the certificate lineage: the certificate's first sight, and,
// when it was issued after the node's current one, that it is current now
// and every older certificate is revoked one superseded grace ahead
// (--conn-drain-timeout + 10 min), with ferun_node, so that every instance
// re-checks the node's connections.
func (s *Service) OnPresented(node, serial, peerAddr string) {
	ctx, cancel := context.WithTimeout(s.bg, presentTimeout)
	defer cancel()
	var p store.Presentation
	err := s.st.InTx(ctx, func(tx *store.Store) (err error) {
		p, err = tx.RecordPresentation(ctx, node, serial, s.supersededGrace())
		return err
	})
	if err != nil {
		s.rl.warn("present", "cannot record a node's certificate presentation", "node", node, "serial", serial, "peer", peerAddr, "err", err)
		return
	}
	if p.Superseded > 0 {
		s.log.Info("node presented a newer certificate; the older ones are superseded",
			"node", node, "serial", serial, "superseded", p.Superseded, "grace", s.supersededGrace())
	}
}

// OnNodeConn is nodelink.Config.OnNodeConn: a node got a current
// connection on this instance after having none; its inventory is
// reconciled (§8 "Reconcile requests").
func (s *Service) OnNodeConn(node string) {
	s.requestReconcile(node)
}
