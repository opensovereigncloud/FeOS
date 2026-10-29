// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
)

// connectedWithin is how recently an ingress must have been seen (a
// Hello, a heartbeat on Sync, a Hold) to count as connected.
const connectedWithin = 30 * time.Second

// IssueJoinToken makes a one-time join token of kind ingress
// (model.JoinKindIngress), valid for ttl (an hour when 0), pinned to the
// name of an ingress unless name is "" (a pinned token creates that name
// or joins it again; an unpinned one creates a new name only). token is
// what the operator gets, "feos-ingress-join-<secret>.<pin>", shown once:
// the database keeps only the hash of the part the ingress sends. Its
// prefix is not a node's, so that no node's Join takes it, also not one of
// a ferund that knows no kinds. An invalid name is an *apierr.Error with
// status 400.
func (s *Service) IssueJoinToken(ctx context.Context, createdBy, name string, ttl time.Duration) (id uuid.UUID, token string, expiresAt time.Time, err error) {
	if name != "" && !pki.IsNodeName(name) {
		return uuid.Nil, "", time.Time{}, apierr.New(http.StatusBadRequest,
			"ingress name %q is not a name: 1 to 63 lowercase letters, digits and \"-\", beginning and ending with a letter or a digit", name)
	}
	if ttl <= 0 {
		ttl = DefaultJoinTokenTTL
	}
	token, _, hash, err := pki.NewIngressJoinToken(s.iss.JoinPin())
	if err != nil {
		return uuid.Nil, "", time.Time{}, fmt.Errorf("make a join token: %w", err)
	}
	t, err := s.st.CreateJoinToken(ctx, hash, createdBy, model.JoinKindIngress, name, ttl)
	if err != nil {
		return uuid.Nil, "", time.Time{}, fmt.Errorf("store the join token: %w", err)
	}
	return t.ID, token, t.ExpiresAt, nil
}

// IngressInfo is an ingress instance as the operator sees it (GET
// /api/v1/admin/ingresses).
type IngressInfo struct {
	Name      string
	Status    string // model.IngressJoined, model.IngressRemoved
	JoinedAt  time.Time
	RemovedAt time.Time
	// Its place on the pods' mesh: its /96, its address, its WireGuard
	// endpoint and public key (nil: none since its last join).
	MeshPrefix   string
	MeshAddress  string
	MeshEndpoint string
	PublicKey    []byte
	Version      string // ferun-ingress's, from its last Hello
	LastSeen     time.Time
	LeftAt       time.Time
	// Connected: seen within the last 30 seconds, on the database clock.
	Connected bool
	// AdmittedVersion is the version of model.IngressPeersGroup at which
	// its address entered (or left) the group.
	AdmittedVersion int64
	// CertNotAfter is when its current certificate (the newest presented
	// in a Hello) expires; without one, its newest certificate whose
	// revocation is not in effect; zero without either.
	CertNotAfter time.Time
}

// ListIngresses returns every ingress instance, removed ones included,
// by name. It reads in one transaction, so that every ingress is judged
// connected against the same now() of the database.
func (s *Service) ListIngresses(ctx context.Context) ([]IngressInfo, error) {
	var out []IngressInfo
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		out = nil
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		gs, err := tx.ListIngresses(ctx)
		if err != nil {
			return err
		}
		out = make([]IngressInfo, 0, len(gs))
		for _, g := range gs {
			certs, err := tx.ListIngressCerts(ctx, g.Name)
			if err != nil {
				return err
			}
			out = append(out, IngressInfo{
				Name: g.Name, Status: g.Status, JoinedAt: g.JoinedAt, RemovedAt: g.RemovedAt,
				MeshPrefix: g.MeshPrefix, MeshAddress: g.MeshAddress, MeshEndpoint: g.MeshEndpoint,
				PublicKey: g.MeshPublicKey, Version: g.Version, LastSeen: g.LastSeen, LeftAt: g.LeftAt,
				Connected:       !g.LastSeen.IsZero() && g.LastSeen.After(now.Add(-connectedWithin)),
				AdmittedVersion: g.GroupVersion,
				CertNotAfter:    certNotAfter(g.CurrentSerial, certs, now),
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list the ingresses: %w", err)
	}
	return out, nil
}

// certNotAfter is the NotAfter of the certificate current among certs
// (newest first): the one presented last, or else the newest whose
// revocation is not in effect at now.
func certNotAfter(current string, certs []store.IngressCert, now time.Time) time.Time {
	for _, c := range certs {
		if current != "" && c.Serial == current {
			return c.NotAfter
		}
	}
	if current != "" {
		return time.Time{}
	}
	for _, c := range certs {
		if c.RevokedAt.IsZero() || c.RevokedAt.After(now) {
			return c.NotAfter
		}
	}
	return time.Time{}
}

// RemoveIngress removes the ingress instance name (store.RemoveIngress):
// its certificates are revoked, it leaves the security group
// model.IngressPeersGroup and the mesh's peers, and its connections close,
// on this instance at once and on the others when they follow
// ferun_ingress. Its Sync streams on this instance end first, with
// PERMISSION_DENIED "ingress <name> was removed; join it again", on which
// the ingress declares its identity lost; its connections close once
// they ended (at most streamEndWait) and their status had streamFlush to
// reach the ingress. Its row stays; a token pinned to the name joins it
// again. It reports whether the ingress exists.
func (s *Service) RemoveIngress(ctx context.Context, name string) (bool, error) {
	found, err := s.st.RemoveIngress(ctx, name)
	if err != nil {
		return false, fmt.Errorf("remove ingress %s: %w", name, err)
	}
	if !found {
		return false, nil
	}
	if ended := s.endStreams(name, false, status.Error(codes.PermissionDenied, removedText(name))); len(ended) > 0 {
		awaitStreams(ended, streamEndWait)
		select {
		case <-time.After(streamFlush):
		case <-ctx.Done():
		}
	}
	closed := 0
	if s.links != nil {
		closed = s.links.CloseIngress(name, "ingress removed")
	}
	s.log.Info("ingress removed", "ingress", name, "connections_closed", closed)
	return true, nil
}
