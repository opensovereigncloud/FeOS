// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// allKey is the coalescer key of the refresh of every node.
const allKey = ""

// Run follows the writes of the other instances until ctx ends, and stops
// fleet's background work then (§8 "ferun_node"):
//   - ferun_node with a node name: the node's rollout deadline is reloaded
//     into nodelink and the node's connections on this instance are
//     re-checked;
//   - ferun_reconcile with a node name: the node's inventory is reconciled
//     when this instance is connected to it;
//   - "" on either (the listener reconnected, or a subscription
//     overflowed: a notification may have been missed): every node's
//     rollout deadline is reloaded, every connection re-checked, and every
//     connected node's inventory reconciled;
//   - a node's last connection gone: the leader forgets which of the
//     mesh's peers it sent the node, which gets them whole next time.
//
// A stopping instance keeps its node connections and uplinks until §7 step
// 5, and revocations, removals and forks must still close them meanwhile.
// So ctx must outlive §7 steps 1 to 4: it is not the context that ends at
// SIGTERM; it ends in step 5, with the node connections.
func (s *Service) Run(ctx context.Context) error {
	nodes, unsubscribeNodes := s.d.Subscribe(db.ChanNode)
	defer unsubscribeNodes()
	reconciles, unsubscribeReconciles := s.d.Subscribe(db.ChanReconcile)
	defer unsubscribeReconciles()
	var conns <-chan nodelink.ConnEvent
	if s.links != nil {
		var unsubscribeConns func()
		conns, unsubscribeConns = s.links.Subscribe()
		defer unsubscribeConns()
	}
	defer s.stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-conns:
			switch {
			case !ok:
				conns = nil
			case !ev.Connected:
				// Its next connection gets its peers whole.
				s.mesh.forget(ev.Node)
			}
		case node := <-nodes:
			if node == "" || node == "*" {
				s.refreshAll()
			} else {
				s.refreshNode(node)
			}
		case node := <-reconciles:
			switch {
			case node == "" || node == "*":
				s.refreshAll()
			case s.links != nil && s.links.Connected(node):
				s.requestReconcile(node)
			}
		}
	}
}

// rolloutDeadline turns the rollout grace a node has left, as the
// database measured it, into a deadline on this instance's clock for
// nodelink: zero when none is left. So the database's clock decides when
// the grace ends, whatever this instance's clock reads.
func (s *Service) rolloutDeadline(left time.Duration) time.Time {
	if left <= 0 {
		return time.Time{}
	}
	return s.now().Add(left)
}

// refreshNode reloads a node's rollout deadline and re-checks its
// connections, in the background, coalesced per node.
func (s *Service) refreshNode(node string) {
	if s.links == nil {
		return
	}
	s.coalesce(&s.refreshes, node, func(ctx context.Context) {
		qctx, cancel := context.WithTimeout(ctx, rpcTimeout)
		left, err := s.st.NodeStoppingFor(qctx, node)
		cancel()
		switch {
		case err == nil:
			s.links.SetStoppingUntil(node, s.rolloutDeadline(left))
		case !errors.Is(err, store.ErrNoRow):
			s.rl.warn("refresh", "cannot reload a node's rollout deadline", "node", node, "err", err)
		}
		s.links.Recheck(ctx, node)
	})
}

// refreshAll reloads every node's rollout deadline, re-checks every
// connection (spread over at least 5 s by nodelink) and requests a
// reconcile of every connected node.
func (s *Service) refreshAll() {
	if s.links == nil {
		return
	}
	s.coalesce(&s.refreshes, allKey, func(ctx context.Context) {
		qctx, cancel := context.WithTimeout(ctx, rpcTimeout)
		left, err := s.st.NodesStoppingFor(qctx)
		cancel()
		if err != nil {
			s.rl.warn("refresh", "cannot reload the nodes' rollout deadlines", "err", err)
		}
		for node, d := range left {
			s.links.SetStoppingUntil(node, s.rolloutDeadline(d))
		}
		s.links.RecheckAll(ctx)
		for _, node := range s.links.Nodes() {
			s.requestReconcile(node)
		}
	})
}

// RequestReconcile asks for an inventory reconcile of node, as fleet's own
// triggers do: for a caller that cannot reach the node (the engine's Sync
// without a connection, §9) and wants an instance that can to reconcile
// it.
func (s *Service) RequestReconcile(node string) { s.requestReconcile(node) }

// requestReconcile asks for an inventory reconcile of node (§8 "Reconcile
// requests"). Connected to it, this instance claims the reconcile in the
// database (at most one per node per 30 s across instances) and runs it in
// the background, one at a time per node. Not connected, it asks the
// instances that are with ferun_reconcile, at most once per 5 s per node.
func (s *Service) requestReconcile(node string) {
	if s.links == nil {
		return
	}
	if !s.links.Connected(node) {
		s.notifyReconcile(node)
		return
	}
	s.coalesce(&s.reconciles, node, func(ctx context.Context) {
		qctx, cancel := context.WithTimeout(ctx, rpcTimeout)
		claimed, err := s.st.ClaimReconcile(qctx, node, reconcileClaimEvery)
		cancel()
		if err != nil {
			s.rl.warn("reconcile-claim", "cannot claim a node's inventory reconcile", "node", node, "err", err)
			return
		}
		if !claimed || s.hooks == nil {
			return
		}
		rctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
		defer cancel()
		if err := s.hooks.ReconcileNode(rctx, node); err != nil && ctx.Err() == nil {
			s.rl.warn("reconcile", "inventory reconcile failed", "node", node, "err", err)
		}
	})
}

// notifyReconcile sends ferun_reconcile for node, at most once per 5 s per
// node from this instance.
func (s *Service) notifyReconcile(node string) {
	now := time.Now()
	s.mu.Lock()
	if last, ok := s.notified[node]; ok && now.Sub(last) < reconcileNotifyEvery {
		s.mu.Unlock()
		return
	}
	s.notified[node] = now
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(s.bg, rpcTimeout)
	defer cancel()
	if err := db.Notify(ctx, s.d.Pool, db.ChanReconcile, node); err != nil {
		s.rl.warn("reconcile-notify", "cannot ask other instances for a reconcile", "node", node, "err", err)
	}
}
