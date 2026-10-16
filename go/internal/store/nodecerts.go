// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The kinds of node certificates.
const (
	CertJoin   = "join"
	CertRejoin = "rejoin"
	CertRenew  = "renew"
)

// The reasons a node certificate is revoked for.
const (
	RevokedRejoined   = "rejoined"   // a join with a token pinned to the node's name
	RevokedSuperseded = "superseded" // a newer certificate was presented
	RevokedRemoved    = "removed"    // the node was removed
	RevokedForked     = "forked"     // a superseded certificate was presented past its grace
)

// NodeCert is a row of node_certs: one node certificate issued, an entry
// of the node's renewal history.
type NodeCert struct {
	Serial         string // lowercase hex
	Node           string
	Kind           string // CertJoin, CertRejoin, CertRenew
	KeyHash        string // sha256 hex of the key's SPKI DER
	PreviousSerial string // renew: the certificate that asked
	TokenID        uuid.UUID
	Peer           string // remote address of the issuing call
	NotBefore      time.Time
	NotAfter       time.Time
	IssuedAt       time.Time
	FirstSeenAt    time.Time // first presented in a handshake
	RevokedAt      time.Time // may lie ahead: superseded, in its grace
	RevokedReason  string
}

const nodeCertCols = `serial, node, kind, key_hash, previous_serial, token_id, peer, not_before, not_after, issued_at,
	first_seen_at, revoked_at, revoked_reason`

func scanNodeCert(row pgx.Row) (NodeCert, error) {
	var c NodeCert
	var tokenID *uuid.UUID
	var firstSeen, revoked *time.Time
	if err := row.Scan(&c.Serial, &c.Node, &c.Kind, &c.KeyHash, &c.PreviousSerial, &tokenID, &c.Peer,
		&c.NotBefore, &c.NotAfter, &c.IssuedAt, &firstSeen, &revoked, &c.RevokedReason); err != nil {
		return NodeCert{}, err
	}
	c.TokenID, c.FirstSeenAt, c.RevokedAt = uuidOf(tokenID), timeOf(firstSeen), timeOf(revoked)
	return c, nil
}

// InsertNodeCert records an issued node certificate (issued_at = now()).
func (s *Store) InsertNodeCert(ctx context.Context, c NodeCert) (NodeCert, error) {
	return scanNodeCert(s.q.QueryRow(ctx, `INSERT INTO node_certs (serial, node, kind, key_hash, previous_serial,
			token_id, peer, not_before, not_after)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+nodeCertCols,
		c.Serial, c.Node, c.Kind, c.KeyHash, c.PreviousSerial, nullUUID(c.TokenID), c.Peer, c.NotBefore, c.NotAfter))
}

// GetNodeCert reads a node certificate by serial; ErrNoRow if none.
func (s *Store) GetNodeCert(ctx context.Context, serial string) (NodeCert, error) {
	c, err := scanNodeCert(s.q.QueryRow(ctx, "SELECT "+nodeCertCols+" FROM node_certs WHERE serial = $1", serial))
	return c, noRow(err, ErrNoRow)
}

// ListNodeCerts returns a node's certificates, newest first: its renewal
// history.
func (s *Store) ListNodeCerts(ctx context.Context, node string) ([]NodeCert, error) {
	rows, err := s.q.Query(ctx, "SELECT "+nodeCertCols+" FROM node_certs WHERE node = $1 ORDER BY issued_at DESC, serial", node)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (NodeCert, error) { return scanNodeCert(r) })
}

// RevokeNodeCerts revokes, from now on, every certificate of node that is
// not revoked yet or whose revocation lies ahead, except the serial except
// ("" for none). It returns how many it revoked. It sends nothing; the
// callers that need ferun_node send it (RejoinNode, RemoveNode, ForkNode
// do).
func (s *Store) RevokeNodeCerts(ctx context.Context, node, reason, except string) (int64, error) {
	tag, err := s.q.Exec(ctx, `UPDATE node_certs SET revoked_at = now(), revoked_reason = $2
		WHERE node = $1 AND serial <> $3 AND (revoked_at IS NULL OR revoked_at > now())`, node, reason, except)
	return tag.RowsAffected(), err
}

// NodeCertCheck is what checkNode decides on, read in one statement with
// the database clock.
type NodeCertCheck struct {
	Found         bool // the serial exists and is the node's
	RevokedAt     time.Time
	RevokedReason string
	NodeStatus    string
	ForkedAt      time.Time
	Now           time.Time // the database clock of the read
}

// Revoked reports a revocation in effect.
func (c NodeCertCheck) Revoked() bool {
	return !c.RevokedAt.IsZero() && !c.RevokedAt.After(c.Now)
}

// RevokeAhead returns a revocation time that lies ahead (a superseded
// certificate in its grace), or zero.
func (c NodeCertCheck) RevokeAhead() time.Time {
	if c.RevokedAt.After(c.Now) {
		return c.RevokedAt
	}
	return time.Time{}
}

// CheckNodeCert reads what decides whether a connection presenting serial
// for node is allowed: the certificate's revocation, the node's status and
// fork, and now(). Found is false for an unknown serial or one of another
// node.
func (s *Store) CheckNodeCert(ctx context.Context, node, serial string) (NodeCertCheck, error) {
	var c NodeCertCheck
	var revoked, forked *time.Time
	err := s.q.QueryRow(ctx, `SELECT c.revoked_at, c.revoked_reason, n.status, n.identity_forked_at, now()
		FROM node_certs c JOIN nodes n ON n.name = c.node WHERE c.serial = $1 AND c.node = $2`, serial, node).
		Scan(&revoked, &c.RevokedReason, &c.NodeStatus, &forked, &c.Now)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeCertCheck{}, nil
	}
	if err != nil {
		return NodeCertCheck{}, err
	}
	c.Found = true
	c.RevokedAt, c.ForkedAt = timeOf(revoked), timeOf(forked)
	return c, nil
}

// Presentation is what RecordPresentation changed.
type Presentation struct {
	// FirstSeen: the certificate was presented for the first time.
	FirstSeen bool
	// Current: the certificate became the node's current one.
	Current bool
	// Superseded is how many older certificates got a revocation at the
	// end of the grace.
	Superseded int64
}

// RecordPresentation records that node presented the certificate serial in
// a handshake that passed checkNode (the certificate lineage): with the
// node row locked, the certificate's first_seen_at is set if unset; if it
// was issued after the node's current certificate (or there is none), it
// becomes current and every older unrevoked certificate of the node is
// revoked one grace ahead ("superseded"), and ferun_node is sent so that
// every instance re-checks the node's connections. Run it in a
// transaction; ErrNoRow when the node or certificate does not exist.
func (s *Store) RecordPresentation(ctx context.Context, node, serial string, grace time.Duration) (Presentation, error) {
	var p Presentation
	if _, err := s.LockNode(ctx, node, ForUpdate); err != nil {
		return p, err
	}
	tag, err := s.q.Exec(ctx, "UPDATE node_certs SET first_seen_at = now() WHERE serial = $1 AND node = $2 AND first_seen_at IS NULL",
		serial, node)
	if err != nil {
		return p, err
	}
	p.FirstSeen = tag.RowsAffected() > 0
	var newer bool
	var issuedAt time.Time
	err = s.q.QueryRow(ctx, `SELECT c.issued_at, n.current_serial <> c.serial AND coalesce(
			c.issued_at > (SELECT cur.issued_at FROM node_certs cur WHERE cur.serial = n.current_serial AND cur.node = n.name),
			true)
		FROM node_certs c JOIN nodes n ON n.name = c.node WHERE c.serial = $1 AND c.node = $2`, serial, node).Scan(&issuedAt, &newer)
	if err != nil {
		return p, noRow(err, ErrNoRow)
	}
	if !newer {
		return p, nil
	}
	if _, err := s.q.Exec(ctx, "UPDATE nodes SET current_serial = $2 WHERE name = $1", node, serial); err != nil {
		return p, err
	}
	p.Current = true
	tag, err = s.q.Exec(ctx, `UPDATE node_certs SET revoked_at = now() + $3::interval, revoked_reason = 'superseded'
		WHERE node = $1 AND serial <> $2 AND revoked_at IS NULL AND issued_at < $4`, node, serial, interval(grace), issuedAt)
	if err != nil {
		return p, err
	}
	p.Superseded = tag.RowsAffected()
	if p.Superseded > 0 {
		if err := s.Notify(ctx, chanNode, node); err != nil {
			return p, err
		}
	}
	return p, nil
}

// DeleteOldNodeCerts removes node certificates that expired more than keep
// ago. It returns how many.
func (s *Store) DeleteOldNodeCerts(ctx context.Context, keep time.Duration) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM node_certs WHERE not_after < now() - $1::interval", interval(keep))
	return tag.RowsAffected(), err
}
