// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/ironcore-dev/feos/go/internal/model"
)

// Recheck runs CheckNode again for every connection of node on this
// instance, node connections and uplinks, and acts on the answers: a
// refusal closes a connection at once, a superseded certificate past its
// grace closes it as the drain timeout does, a revocation time marks it
// superseded and arms its re-check. A transient error changes nothing;
// the node is re-checked again with backoff (1 s doubling to 30 s) until
// every check gets an answer.
//
// The re-checks of one node run one pass at a time, so that the answer of
// an older check never lands after a newer one's (a sweep's check that
// began before a renewal committed must not undo the renewal's). A call
// while a pass runs waits for one more pass, which covers every call that
// arrived meanwhile, and returns when it has finished or ctx ends.
func (s *Server) Recheck(ctx context.Context, node string) {
	s.mu.Lock()
	if s.shut {
		s.mu.Unlock()
		return
	}
	s.gens[node]++
	st := s.nodes[node]
	if st == nil {
		s.mu.Unlock()
		return
	}
	st.checkWant++
	want := st.checkWant
	if st.checking {
		for st.checking && st.checkDone < want && !s.shut {
			ch := st.checked
			s.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return
			}
			s.mu.Lock()
		}
		s.mu.Unlock()
		return
	}
	st.checking = true
	for {
		target := st.checkWant
		links := make([]*link, 0, len(st.links)+len(st.uplinks))
		links = append(links, st.links...)
		links = append(links, st.uplinks...)
		s.mu.Unlock()

		transient := false
		for _, l := range links {
			revokeAt, err := s.check(ctx, node, l.serial, l.addr, false)
			if s.apply(l, revokeAt, err) {
				transient = true
			}
		}

		s.mu.Lock()
		st.checkDone = target
		close(st.checked)
		st.checked = make(chan struct{})
		s.retryLocked(st, transient)
		if st.checkWant == target || s.shut {
			break
		}
	}
	st.checking = false
	s.dropIdleLocked(st)
	s.mu.Unlock()
}

// retryLocked arms the re-check of a node after a pass with a transient
// error, with backoff, and clears it after a pass that got every answer.
func (s *Server) retryLocked(st *nodeState, transient bool) {
	if !transient || s.shut {
		st.retryDelay = 0
		if st.retry != nil {
			st.retry.Stop()
			st.retry = nil
		}
		return
	}
	st.retryDelay = min(max(2*st.retryDelay, s.recheckMinBackoff), s.recheckMaxBackoff)
	if st.retry != nil {
		st.retry.Stop()
	}
	node := st.name
	st.retry = time.AfterFunc(st.retryDelay, func() { s.goOwned(func() { s.Recheck(s.ctx, node) }) })
}

// apply acts on one re-check's answer; it reports a transient error.
func (s *Server) apply(l *link, revokeAt time.Time, err error) (transient bool) {
	var d deferred
	defer func() { d.run() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.closed {
		return false
	}
	switch {
	case err == nil:
		s.applyRevokeLocked(l, revokeAt)
		if l.kind == uplinkLink {
			if l.superseded && l.active == 0 {
				s.closeLinkLocked(l, "superseded certificate", &d)
			}
			return false
		}
		if st := s.nodes[l.node]; st != nil {
			s.updateCurrentLocked(st, &d)
		}
		return false
	case errors.Is(err, model.ErrNodeRefused):
		s.log.Info("node refused on re-check", "node", l.node, "serial", l.serial, "err", err)
		s.closeLinkLocked(l, "refused: "+err.Error(), &d)
		return false
	case errors.Is(err, model.ErrCertSuperseded):
		if l.kind == uplinkLink {
			s.closeLinkLocked(l, "superseded certificate", &d)
		} else {
			s.retireLocked(l, &d)
		}
		return false
	default:
		s.rl.warn("recheck", "nodes' port: re-check failed; connection kept, retrying", "node", l.node, "serial", l.serial, "err", err)
		return true
	}
}

// RecheckAll re-checks every node with a connection on this instance,
// spread over at least 5 s (the hub's reconnect signal, after which any
// NOTIFY may have been missed). It returns at once: the sweep runs in the
// port's own goroutine, and calls while one runs are coalesced into one
// more sweep after it.
func (s *Server) RecheckAll(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	if s.shut {
		s.mu.Unlock()
		return
	}
	s.allGen++
	if s.sweeping {
		s.sweepDue = true
		s.mu.Unlock()
		return
	}
	s.sweeping = true
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		s.sweep()
	}()
}

func (s *Server) sweep() {
	for {
		s.mu.Lock()
		names := make([]string, 0, len(s.nodes))
		for name, st := range s.nodes {
			if len(st.links) > 0 || len(st.uplinks) > 0 {
				names = append(names, name)
			}
		}
		s.mu.Unlock()
		sort.Strings(names)
		var gap time.Duration
		if len(names) > 1 {
			gap = s.recheckSpread / time.Duration(len(names)-1)
		}
		for i, name := range names {
			if i > 0 {
				t := time.NewTimer(gap)
				select {
				case <-t.C:
				case <-s.ctx.Done():
					t.Stop()
				}
			}
			if s.ctx.Err() != nil {
				break
			}
			s.Recheck(s.ctx, name)
		}
		s.mu.Lock()
		if !s.sweepDue || s.shut {
			s.sweeping = false
			s.mu.Unlock()
			return
		}
		s.sweepDue = false
		s.mu.Unlock()
	}
}
