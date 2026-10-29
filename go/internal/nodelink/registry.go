// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
)

// ConnEvent says that a node got its first current connection on this
// instance (Connected) or has none left.
type ConnEvent struct {
	Node      string
	Connected bool
}

// Conns is how ferund reaches nodes: the registry of this instance's node
// connections.
type Conns interface {
	// Conn returns the node's current connection (budgeted), waiting up to
	// the node-wait, or the node's rollout deadline when later, or ctx;
	// then codes.Unavailable.
	Conn(ctx context.Context, node string) (grpc.ClientConnInterface, error)
	// WaitConnected waits like Conn until the node has a current
	// connection, without returning it.
	WaitConnected(ctx context.Context, node string) error
	Connected(node string) bool            // a current connection on this instance
	Connections(node string) int           // current and draining, on this instance
	Nodes() []string                       // nodes connected to this instance
	Subscribe() (<-chan ConnEvent, func()) // first connection / last one gone
}

var _ Conns = (*Server)(nil)

type linkKind int

const (
	nodeLink   linkKind = iota // feos-node/1: the instance is the gRPC client
	uplinkLink                 // h2 with a certificate: the instance is the server
)

// link is one connection of a node with a certificate: a node connection
// or an uplink. Fields below mu are guarded by Server.mu.
type link struct {
	kind                linkKind
	node, serial, addr  string
	notBefore, notAfter time.Time
	raw                 net.Conn         // the TLS connection (an *srvConn for uplinks)
	cc                  *grpc.ClientConn // node connections

	// mu: Server.mu
	seq        uint64 // registration order
	closed     bool
	notReady   bool // the channel left READY: never current again
	retired    bool // ErrCertSuperseded on a re-check: never current again
	superseded bool // CheckNode returned a revocation time for the certificate
	revokeAt   time.Time
	// revokeGen counts the arming of revokeTimer, so that a timer that
	// fired while it was re-armed does nothing; revokeBackoff is the delay
	// of re-arming it while the revocation time has passed by this
	// instance's clock but not yet by the database's.
	revokeGen     uint64
	revokeBackoff time.Duration

	draining   bool
	forceDrain bool // the drain timeout passed while lifecycle calls ran
	active     int  // calls and streams in flight
	lifecycle  int  // among them, lifecycle calls of ops
	long       int  // among them, budgeted long streams
	tools      int  // among them, ToolService calls
	streamSeq  uint64
	streams    map[uint64]context.CancelFunc // budgeted long streams, cut at the drain timeout

	drainTimer, notAfterTimer, revokeTimer, idleTimer *time.Timer
}

// peer is the Peer of an uplink.
func (l *link) peer() Peer {
	return Peer{Node: l.node, Serial: l.serial, Addr: l.addr, NotAfter: l.notAfter}
}

// nodeState is what the registry knows of one node.
type nodeState struct {
	name    string
	links   []*link // registered node connections
	uplinks []*link
	current *link
	// wake is closed and replaced when the node gets a current connection
	// or its rollout deadline changes: what waiters in Conn wait on.
	wake          chan struct{}
	waiters       int
	stoppingUntil time.Time
	retry         *time.Timer // re-check after a transient error
	retryDelay    time.Duration
	// Re-checks of a node run one pass at a time, so that an older answer
	// never lands after a newer one: checking is set while a pass runs,
	// checkWant counts the Recheck calls and checkDone the calls the last
	// finished pass covered; checked is closed and replaced when a pass
	// finishes.
	checking             bool
	checkWant, checkDone uint64
	checked              chan struct{}
}

func (s *Server) nodeLocked(name string) *nodeState {
	st := s.nodes[name]
	if st == nil {
		st = &nodeState{name: name, wake: make(chan struct{}), checked: make(chan struct{})}
		s.nodes[name] = st
	}
	return st
}

// dropIdleLocked forgets a node that has nothing left on this instance.
func (s *Server) dropIdleLocked(st *nodeState) {
	if len(st.links) > 0 || len(st.uplinks) > 0 || st.waiters > 0 || st.current != nil || st.checking ||
		st.stoppingUntil.After(time.Now()) {
		return
	}
	if st.retry != nil {
		st.retry.Stop()
	}
	delete(s.nodes, st.name)
}

// checkGen counts the re-checks of a node and the RecheckAll sweeps. It is
// captured before an accept check: a re-check that ran while the
// connection was not yet registered is repeated for it after registration.
type checkGen struct{ node, all uint64 }

func (s *Server) genOf(node string) checkGen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.genLocked(node)
}

func (s *Server) genLocked(node string) checkGen {
	return checkGen{node: s.gens[node], all: s.allGen}
}

// newer orders node connections for currency: a certificate not
// superseded first, then the newest certificate, then the newest
// registration.
func newer(a, b *link) bool {
	if a.superseded != b.superseded {
		return !a.superseded
	}
	if !a.notBefore.Equal(b.notBefore) {
		return a.notBefore.After(b.notBefore)
	}
	return a.seq > b.seq
}

// registerNode adds a READY node connection and makes the node's current
// connection the right one again.
func (s *Server) registerNode(l *link, revokeAt time.Time, gen checkGen) {
	var d deferred
	s.mu.Lock()
	if s.shut {
		s.mu.Unlock()
		l.cc.Close()
		l.raw.Close()
		return
	}
	st := s.nodeLocked(l.node)
	s.seq++
	l.seq = s.seq
	st.links = append(st.links, l)
	l.notAfterTimer = time.AfterFunc(time.Until(l.notAfter), func() { s.expire(l) })
	s.applyRevokeLocked(l, revokeAt)
	recheck := s.genLocked(l.node) != gen
	s.updateCurrentLocked(st, &d)
	superseded := l.superseded
	s.wg.Add(1)
	s.mu.Unlock()
	d.run()
	go func() {
		defer s.wg.Done()
		s.watchState(l)
	}()
	s.log.Info("node connected", "node", l.node, "serial", l.serial, "peer", l.addr, "superseded", superseded)
	if recheck {
		s.goOwned(func() { s.Recheck(s.ctx, l.node) })
	}
}

// registerUplink adds an uplink, unless the node has its maximum of
// uplinks that are not superseded.
func (s *Server) registerUplink(l *link, revokeAt time.Time, gen checkGen) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shut {
		return false
	}
	st := s.nodeLocked(l.node)
	// Any revocation time means superseded: whether it still lies ahead is
	// the database's decision, made in CheckNode.
	superseded := !revokeAt.IsZero()
	if !superseded {
		n := 0
		for _, u := range st.uplinks {
			if !u.superseded && !u.closed {
				n++
			}
		}
		if n >= s.maxUplinks {
			s.dropIdleLocked(st)
			return false
		}
	}
	s.seq++
	l.seq = s.seq
	st.uplinks = append(st.uplinks, l)
	l.notAfterTimer = time.AfterFunc(time.Until(l.notAfter), func() { s.expire(l) })
	s.applyRevokeLocked(l, revokeAt)
	if l.superseded {
		// Superseded already at accept: it answers its first calls with the
		// reason, and is closed once it has been idle for a moment.
		l.idleTimer = time.AfterFunc(s.supersededGrace, func() { s.closeIfIdle(l) })
	}
	if s.genLocked(l.node) != gen {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.Recheck(s.ctx, l.node)
		}()
	}
	return true
}

// applyRevokeLocked records a check's revocation time. Any revocation time
// marks the link superseded: CheckNode decides on the database clock that
// it still lies ahead (§2), and this instance's clock never overrules it.
// The link's re-check (node connection) or close (uplink) is armed at that
// time. When it has already passed by this instance's clock, which runs
// ahead of the database's, it is armed after a backoff (1 s doubling to
// 30 s), so that a re-check that lands before the database reaches the
// revocation time is repeated until the database answers
// ErrCertSuperseded. Only the zero time clears it.
func (s *Server) applyRevokeLocked(l *link, revokeAt time.Time) {
	if revokeAt.IsZero() {
		if l.superseded {
			l.superseded = false
			l.revokeAt = time.Time{}
			l.revokeBackoff = 0
			l.revokeGen++
			if l.revokeTimer != nil {
				l.revokeTimer.Stop()
				l.revokeTimer = nil
			}
		}
		return
	}
	l.superseded = true
	d := time.Until(revokeAt)
	if d > 0 {
		if l.revokeAt.Equal(revokeAt) && l.revokeTimer != nil {
			return // armed for this time already
		}
		l.revokeBackoff = 0
	} else {
		l.revokeBackoff = min(max(2*l.revokeBackoff, s.recheckMinBackoff), s.recheckMaxBackoff)
		d = l.revokeBackoff
	}
	l.revokeAt = revokeAt
	if l.revokeTimer != nil {
		l.revokeTimer.Stop()
	}
	l.revokeGen++
	gen := l.revokeGen
	l.revokeTimer = time.AfterFunc(d, func() { s.revocationDue(l, gen) })
}

// revocationDue runs when a superseded certificate's revocation takes
// effect: a node connection is re-checked, an uplink is closed. A timer
// that was re-armed or cleared meanwhile does nothing.
func (s *Server) revocationDue(l *link, gen uint64) {
	var d deferred
	s.mu.Lock()
	due := !l.closed && l.superseded && l.revokeGen == gen
	if due && l.kind == uplinkLink {
		s.closeLinkLocked(l, "superseded certificate revoked", &d)
	}
	s.mu.Unlock()
	d.run()
	if due && l.kind == nodeLink {
		s.goOwned(func() { s.Recheck(s.ctx, l.node) })
	}
}

// updateCurrentLocked picks the node's current connection and makes every
// other one drain: an idle draining connection is closed at once, a busy
// one when its calls end or at the drain timeout.
func (s *Server) updateCurrentLocked(st *nodeState, d *deferred) {
	var best *link
	for _, l := range st.links {
		if l.closed || l.notReady || l.retired || l.forceDrain {
			continue
		}
		if best == nil || newer(l, best) {
			best = l
		}
	}
	prev := st.current
	st.current = best
	var idle []*link
	for _, l := range st.links {
		if l == best {
			if l.draining {
				l.draining = false
				if l.drainTimer != nil {
					l.drainTimer.Stop()
				}
			}
			continue
		}
		if !l.draining {
			l.draining = true
			l.drainTimer = time.AfterFunc(s.cfg.ConnDrainTimeout, func() { s.drainTimeout(l) })
		}
		if l.active == 0 {
			idle = append(idle, l)
		}
	}
	for _, l := range idle {
		s.closeLinkLocked(l, "drained", d)
	}
	switch {
	case prev == nil && best != nil:
		close(st.wake)
		st.wake = make(chan struct{})
		s.emitLocked(ConnEvent{Node: st.name, Connected: true})
		if s.cfg.OnNodeConn != nil {
			node := st.name
			d.add(func() { go s.cfg.OnNodeConn(node) })
		}
	case prev != nil && best == nil:
		s.emitLocked(ConnEvent{Node: st.name, Connected: false})
	}
}

// closeLinkLocked takes a link out of the registry; its connection is
// closed after Server.mu is released, in a goroutine, since a TLS close
// may wait for the peer. The caller recomputes the current connection
// when the link may have been it.
func (s *Server) closeLinkLocked(l *link, reason string, d *deferred) {
	if l.closed {
		return
	}
	l.closed = true
	for _, t := range []*time.Timer{l.drainTimer, l.notAfterTimer, l.revokeTimer, l.idleTimer} {
		if t != nil {
			t.Stop()
		}
	}
	for _, cancel := range l.streams {
		d.add(cancel)
	}
	l.streams = nil
	if st := s.nodes[l.node]; st != nil {
		st.links = remove(st.links, l)
		st.uplinks = remove(st.uplinks, l)
		if st.current == l {
			s.updateCurrentLocked(st, d)
		}
		s.dropIdleLocked(st)
	}
	kind := "node connection"
	if l.kind == uplinkLink {
		kind = "uplink"
	}
	s.log.Info(kind+" closed", "node", l.node, "serial", l.serial, "peer", l.addr, "reason", reason)
	d.add(func() {
		go func() {
			if l.cc != nil {
				l.cc.Close()
			}
			l.raw.Close()
		}()
	})
}

// closeLink is closeLinkLocked with the lock taken.
func (s *Server) closeLink(l *link, reason string) {
	var d deferred
	s.mu.Lock()
	s.closeLinkLocked(l, reason, &d)
	s.mu.Unlock()
	d.run()
}

func remove(ls []*link, l *link) []*link {
	for i, x := range ls {
		if x == l {
			return append(ls[:i:i], ls[i+1:]...)
		}
	}
	return ls
}

// expire closes a connection when its certificate expires: Go checks the
// validity only in the handshake.
func (s *Server) expire(l *link) { s.closeLink(l, "certificate expired") }

// drainTimeout ends a draining node connection: at once when no lifecycle
// call of an op runs on it; else its budgeted long streams are cut now and
// the connection is closed when the last lifecycle call ends.
func (s *Server) drainTimeout(l *link) {
	var d deferred
	s.mu.Lock()
	if !l.closed && l.draining {
		s.forceDrainLocked(l, "drain timeout", &d)
	}
	s.mu.Unlock()
	d.run()
}

func (s *Server) forceDrainLocked(l *link, reason string, d *deferred) {
	if l.lifecycle == 0 {
		s.closeLinkLocked(l, reason, d)
		return
	}
	l.forceDrain = true
	for id, cancel := range l.streams {
		d.add(cancel)
		delete(l.streams, id)
	}
	s.log.Info("node connection waits for lifecycle calls before it closes", "node", l.node, "serial", l.serial, "reason", reason, "lifecycle", l.lifecycle)
}

// retireLocked handles ErrCertSuperseded on a node connection: it is never
// current again and closes as at the drain timeout.
func (s *Server) retireLocked(l *link, d *deferred) {
	if l.closed {
		return
	}
	l.retired = true
	if st := s.nodes[l.node]; st != nil {
		s.updateCurrentLocked(st, d)
	}
	if !l.closed {
		s.forceDrainLocked(l, "superseded certificate", d)
	}
}

// watchState follows a node connection's channel; once it leaves READY
// (TRANSIENT_FAILURE, SHUTDOWN, or IDLE after a GOAWAY) it takes no new
// calls and drains.
func (s *Server) watchState(l *link) {
	for {
		state := l.cc.GetState()
		if state != connectivity.Ready {
			s.markNotReady(l)
			return
		}
		if !l.cc.WaitForStateChange(s.ctx, state) {
			return
		}
	}
}

// closeIfIdle closes a superseded uplink without calls in flight.
func (s *Server) closeIfIdle(l *link) {
	var d deferred
	s.mu.Lock()
	if !l.closed && l.active == 0 {
		s.closeLinkLocked(l, "superseded certificate", &d)
	}
	s.mu.Unlock()
	d.run()
}

// uplinkClosed takes an uplink out of the registry when its connection
// closed, whoever closed it.
func (s *Server) uplinkClosed(l *link) { s.closeLink(l, "connection closed") }

// SetStoppingUntil holds a node's rollout deadline (§4): Conn and
// WaitConnected wait for the node up to it. The zero time clears it. until
// is on this instance's clock, compared with time.Now: fleet makes it from
// the grace the node has left by the database's clock (time.Now().Add of
// it), never from stopping_until itself, so that the instance's offset
// from the database moves nothing.
func (s *Server) SetStoppingUntil(node string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.nodes[node]
	if st == nil {
		if !until.After(time.Now()) {
			return
		}
		st = s.nodeLocked(node)
	}
	st.stoppingUntil = until
	close(st.wake)
	st.wake = make(chan struct{})
	s.dropIdleLocked(st)
}

// unavailable is the answer for a node without a connection here: a
// RefusalError.
func (s *Server) unavailable(node string) error {
	return refusef(codes.Unavailable, "node %s is not connected to instance %s", node, s.cfg.InstanceID)
}

// waitCurrent waits until node has a current connection, up to the node
// wait or the node's rollout deadline when later, or ctx. With reserve it
// takes the call's slot on that connection under the same lock, or fails
// with the budget's error.
func (s *Server) waitCurrent(ctx context.Context, node string, reserve bool, kind callKind) (*link, error) {
	start := time.Now()
	for {
		s.mu.Lock()
		st := s.nodeLocked(node)
		if l := st.current; l != nil {
			if !reserve {
				s.mu.Unlock()
				return l, nil
			}
			err := s.reserveLocked(l, kind)
			s.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return l, nil
		}
		deadline := start.Add(s.cfg.NodeWait)
		if st.stoppingUntil.After(deadline) {
			deadline = st.stoppingUntil
		}
		wait := time.Until(deadline)
		if wait <= 0 || ctx.Err() != nil {
			s.dropIdleLocked(st)
			s.mu.Unlock()
			return nil, s.unavailable(node)
		}
		wake := st.wake
		st.waiters++
		s.mu.Unlock()

		t := time.NewTimer(wait)
		select {
		case <-wake:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
		s.mu.Lock()
		st.waiters--
		s.mu.Unlock()
	}
}

// Conn returns the node's current connection, wrapped by the stream
// budget. Every call through it goes to the connection current at that
// call (waiting as Conn does when the node has none), so that a caller
// that keeps it never puts new calls on a draining connection.
func (s *Server) Conn(ctx context.Context, node string) (grpc.ClientConnInterface, error) {
	if _, err := s.waitCurrent(ctx, node, false, callPlain); err != nil {
		return nil, err
	}
	return &nodeConn{s: s, node: node}, nil
}

// WaitConnected waits like Conn until the node has a current connection.
func (s *Server) WaitConnected(ctx context.Context, node string) error {
	_, err := s.waitCurrent(ctx, node, false, callPlain)
	return err
}

// Connected reports whether the node has a current connection here.
func (s *Server) Connected(node string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.nodes[node]
	return st != nil && st.current != nil
}

// Connections counts the node's connections here, current and draining.
func (s *Server) Connections(node string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.nodes[node]; st != nil {
		return len(st.links)
	}
	return 0
}

// Nodes lists the nodes with a current connection here, sorted.
func (s *Server) Nodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for name, st := range s.nodes {
		if st.current != nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// subscriber receives ConnEvents through an unbounded queue, so that a
// slow reader never blocks the registry and never misses an event.
type subscriber struct {
	mu     sync.Mutex
	queue  []ConnEvent
	signal chan struct{}
	out    chan ConnEvent
	done   chan struct{}
	once   sync.Once
}

func (sub *subscriber) push(ev ConnEvent) {
	sub.mu.Lock()
	sub.queue = append(sub.queue, ev)
	sub.mu.Unlock()
	select {
	case sub.signal <- struct{}{}:
	default:
	}
}

func (sub *subscriber) pump() {
	defer close(sub.out)
	for {
		sub.mu.Lock()
		if len(sub.queue) == 0 {
			sub.mu.Unlock()
			select {
			case <-sub.signal:
				continue
			case <-sub.done:
				return
			}
		}
		ev := sub.queue[0]
		sub.queue = sub.queue[1:]
		sub.mu.Unlock()
		select {
		case sub.out <- ev:
		case <-sub.done:
			return
		}
	}
}

// Subscribe returns the node connection events of this instance until the
// returned function is called, which closes the channel.
func (s *Server) Subscribe() (<-chan ConnEvent, func()) {
	sub := &subscriber{signal: make(chan struct{}, 1), out: make(chan ConnEvent), done: make(chan struct{})}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	go sub.pump()
	return sub.out, func() {
		sub.once.Do(func() {
			s.mu.Lock()
			delete(s.subs, sub)
			s.mu.Unlock()
			close(sub.done)
		})
	}
}

func (s *Server) emitLocked(ev ConnEvent) {
	for sub := range s.subs {
		sub.push(ev)
	}
}
