// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/leader"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// The names of fleet's leader loops.
const (
	LoopNodeExpiry      = "node-expiry"
	LoopReconcileSweep  = "reconcile-sweep"
	LoopJoinTokenExpiry = "join-token-expiry"
	LoopRetention       = "fleet-retention"
)

// Retention of fleet's tables (§7).
const (
	// nodeStartRetention keeps a start of feosd after its last event; a
	// node's current start is never deleted.
	nodeStartRetention = 30 * 24 * time.Hour
	// certRetention keeps node and pod certificates after they expired.
	certRetention = 90 * 24 * time.Hour
	// joinTokenRetention keeps used and expired join tokens (the admin
	// view lists them with who used them, from where).
	joinTokenRetention = 30 * 24 * time.Hour
)

// The limits of the leader's fenced bulk transactions (§7): the share lock
// on the lease row never holds the renew up for long.
const (
	fencedLockTimeout      = time.Second
	fencedStatementTimeout = 3 * time.Second
	// fencedBatch is the most rows one fenced transaction touches.
	fencedBatch = 256
	// retentionStatementTimeout is the session's own: retention deletes
	// are unfenced and may take longer.
	retentionStatementTimeout = 30 * time.Second
)

// hintNodesDown is the ferun_leader hint after node expiry: it wakes the
// down sweep, through which the sandboxes of the expired nodes follow.
const hintNodesDown = "down"

// Loops returns fleet's leader loops: node expiry (5 s), the reconcile
// sweep (60 s), the join tokens' expiry (60 s), the retention of
// node_starts, event_gaps, node_certs and issued_certs (1 h), and the
// mesh's peers (15 s, and when a change wakes it).
func (s *Service) Loops() []leader.Loop {
	return []leader.Loop{
		{Name: LoopNodeExpiry, Interval: 5 * time.Second, Run: s.expireNodes},
		{Name: LoopReconcileSweep, Interval: time.Minute, Run: s.reconcileSweep},
		{Name: LoopJoinTokenExpiry, Interval: time.Minute, Run: s.expireJoinTokens},
		{Name: LoopRetention, Interval: time.Hour, Run: s.retention},
		{Name: LoopMeshPeers, Interval: meshPeersInterval, Run: s.sendMeshPeers},
	}
}

// healthy reports whether the leader may mark nodes down: it has led for
// at least the node TTL and the database has not failed within it, so
// that after an outage of the control plane or a failover every node has a
// full TTL to report again.
func (s *Service) healthy() bool {
	since := s.leadingSince()
	if since.IsZero() || time.Since(since) < s.cfg.NodeTTL {
		return false
	}
	if f := s.lastFailure(); !f.IsZero() && time.Since(f) < s.cfg.NodeTTL {
		return false
	}
	return true
}

// expireNodes is node expiry (§7): every joined or ready node whose last
// telemetry (or join, without any) is older than the node TTL becomes down,
// "heartbeat expired". Each fenced transaction marks at most 256 nodes and
// wakes the leader's down sweep, through which the sandboxes of the expired
// nodes follow; the batches go on until one comes back short.
func (s *Service) expireNodes(ctx context.Context) error {
	if s.el == nil || !s.healthy() {
		return nil
	}
	for {
		names, err := s.expireNodesBatch(ctx)
		if errors.Is(err, leader.ErrNotLeader) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("node expiry: %w", err)
		}
		for _, n := range names {
			s.log.Warn("node down: heartbeat expired", "node", n, "ttl", s.cfg.NodeTTL)
		}
		if len(names) == 0 || len(names) < s.expiryBatch {
			return nil
		}
	}
}

// expireNodesBatch marks down at most expiryBatch expired nodes in one
// fenced transaction, which sends the ferun_leader hint when it changed any.
func (s *Service) expireNodesBatch(ctx context.Context) ([]string, error) {
	var names []string
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		names = nil
		if err := tx.SetLocalTimeouts(ctx, fencedLockTimeout, fencedStatementTimeout); err != nil {
			return err
		}
		if err := s.el.Fence(ctx, tx.Q()); err != nil {
			return err
		}
		var err error
		if names, err = tx.ExpireNodesBatch(ctx, s.cfg.NodeTTL, s.expiryBatch); err != nil {
			return err
		}
		if len(names) > 0 {
			return tx.Notify(ctx, db.ChanLeader, hintNodesDown)
		}
		return nil
	})
	return names, err
}

// reconcileSweep requests an inventory reconcile of every ready node this
// instance, the leader, is connected to: the backstop of the event and
// hash triggers.
func (s *Service) reconcileSweep(ctx context.Context) error {
	if s.links == nil {
		return nil
	}
	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("reconcile sweep: %w", err)
	}
	for _, n := range nodes {
		if n.Status == model.NodeReady && s.links.Connected(n.Name) {
			s.requestReconcile(n.Name)
		}
	}
	return nil
}

// expireJoinTokens deletes the join tokens used or expired more than
// joinTokenRetention ago: idempotent, unfenced.
func (s *Service) expireJoinTokens(ctx context.Context) error {
	return s.st.InTx(ctx, func(tx *store.Store) error {
		if err := tx.SetLocalTimeouts(ctx, fencedLockTimeout, retentionStatementTimeout); err != nil {
			return err
		}
		n, err := tx.DeleteStaleJoinTokens(ctx, joinTokenRetention)
		if err != nil {
			return fmt.Errorf("join token expiry: %w", err)
		}
		if n > 0 {
			s.log.Info("deleted used and expired join tokens", "tokens", n)
		}
		return nil
	})
}

// retention deletes old rows of fleet's tables (§7), each in a
// transaction of its own: idempotent, unfenced, waiting at most 1 s for a
// lock. node_starts 30 days after its last event (never a node's current
// start), event_gaps after --activity-retention, node and pod
// certificates 90 days after they expired.
func (s *Service) retention(ctx context.Context) error {
	jobs := []struct {
		what string
		run  func(tx *store.Store) (int64, error)
	}{
		{"node starts", func(tx *store.Store) (int64, error) { return tx.DeleteOldNodeStarts(ctx, nodeStartRetention) }},
		{"event gaps", func(tx *store.Store) (int64, error) {
			return tx.DeleteOldEventGaps(ctx, s.cfg.ActivityRetention)
		}},
		{"node certificates", func(tx *store.Store) (int64, error) { return tx.DeleteOldNodeCerts(ctx, certRetention) }},
		{"pod certificates", func(tx *store.Store) (int64, error) { return tx.DeleteOldIssuedCerts(ctx, certRetention) }},
	}
	var errs []error
	for _, j := range jobs {
		err := s.st.InTx(ctx, func(tx *store.Store) error {
			if err := tx.SetLocalTimeouts(ctx, fencedLockTimeout, retentionStatementTimeout); err != nil {
				return err
			}
			n, err := j.run(tx)
			if err != nil {
				return err
			}
			if n > 0 {
				s.log.Info("retention deleted old rows", "rows", n, "of", j.what)
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("retention of %s: %w", j.what, err))
		}
	}
	return errors.Join(errs...)
}
