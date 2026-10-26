// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/leader"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
	"github.com/ironcore-dev/feos/go/internal/podid"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// Sandboxes is what the watches need of the sandbox engine
// (*sandbox.Engine): a read of a sandbox's state and node that never wakes
// it.
type Sandboxes interface {
	State(ctx context.Context, id podid.ID) (state, node string, err error)
}

// timings are the watches' limits and periods; tests shorten them.
type timings struct {
	// lease is a watch's lease, renewed by a PUT, a delivered keepalive,
	// and an event stream's opening and closing.
	lease time.Duration
	// ownerLease is the owner's lease, renewed every ownerRenew.
	ownerLease, ownerRenew time.Duration
	// ackTimeout bounds the wait for the guest's ack of a set, and a
	// PUT's wait for the owner's answer.
	ackTimeout time.Duration
	// keepalive is the event stream's comment interval.
	keepalive time.Duration
	// relinkMin and relinkMax bound the backoff of re-opening a lost
	// guest stream.
	relinkMin, relinkMax time.Duration
	// putPoll is the timer of a PUT's wait for the owner's answer,
	// besides ferun_watch.
	putPoll time.Duration
	// rowTimeout bounds the owner's own reads and writes of the row.
	rowTimeout time.Duration
	// ringSize is the replay depth; perSandbox the guest streams of one
	// sandbox on this instance.
	ringSize, perSandbox int
	// expiryEvery and expiryBatch are the leader's expiry loop.
	expiryEvery time.Duration
	expiryBatch int
}

func defaultTimings() timings {
	return timings{
		lease:       2 * time.Minute,
		ownerLease:  30 * time.Second,
		ownerRenew:  10 * time.Second,
		ackTimeout:  15 * time.Second,
		keepalive:   25 * time.Second,
		relinkMin:   time.Second,
		relinkMax:   30 * time.Second,
		putPoll:     db.DefaultPoll,
		rowTimeout:  10 * time.Second,
		ringSize:    1024,
		perSandbox:  16,
		expiryEvery: time.Minute,
		expiryBatch: 256,
	}
}

// Service is the watches of one instance: the handlers of the four routes
// (sandbox class; path values {id} and {wid}), and the watches this
// instance owns.
type Service struct {
	sb    Sandboxes
	conns nodelink.Conns
	db    *db.DB
	st    *store.Store
	id    string
	log   *slog.Logger
	rl    *rateLog
	t     timings

	// base is the parent of the guest streams and of the owners' work;
	// Run's end cancels it.
	base       context.Context
	cancelBase context.CancelFunc

	mu sync.Mutex
	// owned are the watches this instance owns, by id.
	owned map[uuid.UUID]*owned
	// links counts the guest streams per sandbox (the cap).
	links map[podid.ID]int
	// ending is closed by EndStreams.
	ending  chan struct{}
	endOnce sync.Once
}

// New returns the watch service of the instance instanceID.
func New(s Sandboxes, conns nodelink.Conns, d *db.DB, instanceID string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "watch")
	svc := &Service{
		sb: s, conns: conns, db: d, st: store.New(d), id: instanceID, log: log, rl: newRateLog(log),
		t:      defaultTimings(),
		owned:  map[uuid.UUID]*owned{},
		links:  map[podid.ID]int{},
		ending: make(chan struct{}),
	}
	svc.base, svc.cancelBase = context.WithCancel(context.Background())
	return svc
}

// Run is the owner's side until ctx ends: it renews the owner's leases,
// and acts on ferun_watch (a new set, a new owner, a deleted watch),
// ferun_sandbox (a sandbox that runs again, or is gone) and the node
// connections (a lost one detaches the streams on it, a new one re-opens
// them). At its end the instance lets go of its watches and of their
// leases, so that another instance takes them over at once.
func (s *Service) Run(ctx context.Context) error {
	defer s.shutdown()
	watchNotes, unsubscribeWatch := s.db.Subscribe(db.ChanWatch)
	defer unsubscribeWatch()
	sandboxNotes, unsubscribeSandbox := s.db.Subscribe(db.ChanSandbox)
	defer unsubscribeSandbox()
	events, unsubscribeConns := s.conns.Subscribe()
	defer unsubscribeConns()
	renew := time.NewTicker(s.t.ownerRenew)
	defer renew.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case p := <-watchNotes:
			s.signalWhere(kickOf, func(o *owned) bool { return p == "" || p == "*" || o.id.String() == p })
		case p := <-sandboxNotes:
			s.signalWhere(relinkOf, func(o *owned) bool { return p == "" || p == "*" || o.sandbox.String() == p })
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			s.onConn(ev)
		case <-renew.C:
			s.renew(ctx)
		}
	}
}

// ownedList returns the owned watches match selects.
func (s *Service) ownedList(match func(o *owned) bool) []*owned {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*owned
	for _, o := range s.owned {
		if match(o) {
			out = append(out, o)
		}
	}
	return out
}

// The supervisors' signals: kick reads the row and acts on it; relink
// also opens a lost stream at once.
func kickOf(o *owned) chan struct{}   { return o.kick }
func relinkOf(o *owned) chan struct{} { return o.relink }

// signalWhere signals the supervisors of the owned watches match selects.
// A ferun_sandbox relinks: the sandbox's state changed (it may run again),
// which nothing an owner does causes.
func (s *Service) signalWhere(signal func(o *owned) chan struct{}, match func(o *owned) bool) {
	for _, o := range s.ownedList(match) {
		o.signal(signal(o))
	}
}

// onConn detaches the streams on a node whose last connection to this
// instance is gone (their readers get a stale event at once), and has the
// watches without a stream placed again, at once, when a node connects.
func (s *Service) onConn(ev nodelink.ConnEvent) {
	for _, o := range s.ownedList(func(*owned) bool { return true }) {
		o.mu.Lock()
		l := o.link
		o.mu.Unlock()
		switch {
		case !ev.Connected && l != nil && l.node == ev.Node:
			l.cancel()
		case ev.Connected && (l == nil || l.ended()):
			o.signal(o.relink)
		}
	}
}

// renew extends the owner's lease on its watches; those not renewed are
// no longer its own (or gone), which their supervisors confirm and act
// on.
func (s *Service) renew(ctx context.Context) {
	all := s.ownedList(func(*owned) bool { return true })
	if len(all) == 0 {
		return
	}
	held := make([]store.OwnedWatch, 0, len(all))
	for _, o := range all {
		held = append(held, store.OwnedWatch{ID: o.id, Epoch: o.epoch})
	}
	rctx, cancel := context.WithTimeout(ctx, s.t.rowTimeout)
	defer cancel()
	renewed, err := s.st.RenewOwnedWatches(rctx, s.id, held, s.t.ownerLease)
	if err != nil {
		if ctx.Err() == nil {
			s.rl.warn("renew", "watch: cannot renew the owner's leases", "err", err)
		}
		return
	}
	for _, o := range all {
		if !renewed[o.id] {
			o.signal(o.kick)
		}
	}
}

// shutdown lets go of every owned watch and ends the owner's leases.
func (s *Service) shutdown() {
	for _, o := range s.ownedList(func(*owned) bool { return true }) {
		o.end(true)
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.t.rowTimeout)
	defer cancel()
	if _, err := s.st.ReleaseWatchOwnership(ctx, s.id); err != nil {
		s.log.Warn("cannot release the watches this instance owned; they are taken over when their leases end",
			"err", err)
	}
	s.cancelBase()
}

// EndStreams ends this instance's event streams at once, and every one
// that begins afterwards (§7 step 2: the instance stops). The streams are
// cut off (cutOff), as a dropped connection is, so that their clients
// reconnect to another instance with their cursor rather than give up the
// watch.
func (s *Service) EndStreams() {
	s.endOnce.Do(func() { close(s.ending) })
}

// Loops returns the leader's loop of the watches: every minute it deletes
// the watches whose lease has passed (ferun_watch tells their owners).
func (s *Service) Loops() []leader.Loop {
	return []leader.Loop{{
		Name:     "watch-expiry",
		Interval: s.t.expiryEvery,
		Run: func(ctx context.Context) error {
			for {
				ids, err := s.st.DeleteExpiredWatches(ctx, s.t.expiryBatch)
				if err != nil {
					return err
				}
				if len(ids) < s.t.expiryBatch {
					return nil
				}
			}
		},
	}}
}

// adopt makes the row this instance's owned watch in the row's epoch,
// replacing an owned of an earlier epoch. Its supervisor starts with
// start, after the caller's first placing. Two takes on this instance may
// adopt out of order: an owned of the row's epoch or a later one stays,
// and is returned (its epoch tells the caller that its row is not the
// latest).
func (s *Service) adopt(row store.Watch) *owned {
	s.mu.Lock()
	old := s.owned[row.ID]
	if old != nil && old.epoch >= row.Epoch {
		s.mu.Unlock()
		return old
	}
	o := s.newOwned(row)
	s.owned[row.ID] = o
	s.mu.Unlock()
	if old != nil {
		old.end(true)
	}
	return o
}

// forget drops o from the owned watches, if it is still the one there.
func (s *Service) forget(o *owned) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owned[o.id] == o {
		delete(s.owned, o.id)
	}
}

// ownedAs returns the owned watch the row says this instance owns, in the
// row's epoch; nil when the row names another owner or epoch, or this
// instance has let go of it.
func (s *Service) ownedAs(row store.Watch) *owned {
	if row.Owner != s.id {
		return nil
	}
	s.mu.Lock()
	o := s.owned[row.ID]
	s.mu.Unlock()
	if o == nil || o.epoch != row.Epoch || o.isClosed() {
		return nil
	}
	return o
}

// takeLinkSlot takes one of the sandbox's guest-stream slots on this
// instance; false when they are all taken.
func (s *Service) takeLinkSlot(sandbox podid.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links[sandbox] >= s.t.perSandbox {
		return false
	}
	s.links[sandbox]++
	return true
}

func (s *Service) releaseLinkSlot(sandbox podid.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links[sandbox] <= 1 {
		delete(s.links, sandbox)
		return
	}
	s.links[sandbox]--
}

// rateLog logs at most one line per second per kind, counting what it
// suppressed: for lines a remote party can trigger.
type rateLog struct {
	log   *slog.Logger
	mu    sync.Mutex
	kinds map[string]*rateKind
}

type rateKind struct {
	last       time.Time
	suppressed int
}

func newRateLog(log *slog.Logger) *rateLog {
	return &rateLog{log: log, kinds: map[string]*rateKind{}}
}

func (r *rateLog) warn(kind, msg string, args ...any) {
	now := time.Now()
	r.mu.Lock()
	k := r.kinds[kind]
	if k == nil {
		k = &rateKind{}
		r.kinds[kind] = k
	} else if now.Sub(k.last) < time.Second {
		k.suppressed++
		r.mu.Unlock()
		return
	}
	n := k.suppressed
	k.last, k.suppressed = now, 0
	r.mu.Unlock()
	if n > 0 {
		args = append(args, "suppressed", n)
	}
	r.log.Warn(msg, args...)
}
