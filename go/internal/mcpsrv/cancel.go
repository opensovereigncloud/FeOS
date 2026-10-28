// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package mcpsrv

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// cancelPayload is the payload of ferun_cancel.
type cancelPayload struct {
	Kind string `json:"kind"`
	Key  string `json:"key,omitempty"`
	// Client is the client whose part of the door's memory a cancel of
	// kind mcp goes into (clientOf). An older instance sends none.
	Client string `json:"client,omitempty"`
}

// maxCancelKey bounds a cancel key that is broadcast: pg_notify takes
// 8000 bytes, and request ids are short.
const maxCancelKey = 1024

// The cancels of calls with an id (ADR 0023). Such a call outlives its
// request, so a notifications/cancelled has to find it after the request
// ended: the go-sdk's client retires a call when its context ends, which
// hangs up the call's request, and sends notifications/cancelled after
// that, on a goroutine of its own (mcp/transport.go:300-329). The cancel
// thus arrives after the hang-up, or, on another connection that is
// faster, before the call's request reached its handler. The door keeps
// the call of a request for cancelLinger after the request ended, and
// remembers a cancel that found no request for recentFor, so that the
// request it names starts nothing when it arrives.
//
// Both memories are kept per client, the token that sent the request or
// the cancel (clientOf): a client has at most maxPerClient entries in
// each, and one more evicts the client's own oldest ended entry, never
// another client's, so that no client's churn flushes the entries that
// another client's cancels need. A client that has maxPerClient requests
// with an id running on the instance is refused the next one. Ended
// entries are listed in the order they end, so that their expiry and
// eviction cost O(1) under s.mu and nothing is scanned (memory).
//
// Only session (initialize) clients have such cancels: the go-sdk's
// default protocol (server/discover) has no session id, so the door maps
// none of its cancels to a call, and its clients cancel with REST DELETE
// /api/v1/sandboxes/{id}/calls/{key}.
const (
	// cancelLinger is how long the call with an id of a request stays
	// cancellable under the request's key after the request ended.
	cancelLinger = 30 * time.Second
	// recentFor is how long a cancel that found no request is
	// remembered.
	recentFor = 30 * time.Second
	// maxPerClient bounds one client's entries in the memory of calls
	// with an id (its requests that run, and those that ended within
	// cancelLinger), and its remembered cancels.
	maxPerClient = 256
	// cancelCallTimeout bounds the cancel of a call with an id.
	cancelCallTimeout = 5 * time.Second
)

// errClientFull refuses a request with an id whose client has
// maxPerClient requests with an id running on this instance: the
// retryable answer of a full cap.
var errClientFull = fmt.Errorf("%w (at most %d running requests with an id per token on this instance)",
	tools.ErrCallCap, maxPerClient)

// clientOf names the client whose token has the hash tokenHash, in the
// door's memory and in ferun_cancel: a short digest of the hash, so that
// the hash itself does not travel.
func clientOf(tokenHash string) string {
	sum := sha256.Sum256([]byte("ferun-mcp-client\x00" + tokenHash))
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}

// keyedCancel is the call with an id that a request of a session runs or
// waits for: what a cancel under the request's key cancels.
type keyedCancel struct {
	sandbox podid.ID
	key     string
	// cancelled is set by the first cancel that reached the entry. The
	// call is cancelled once, when the entry is cancelled and its request
	// ended: then the request's call, if it started one, is registered
	// with the Runner, and no cancel can come before it.
	cancelled bool
}

// memory is what the door remembers for a while under keys: entries,
// each in the part of the client that made it. An entry is pinned while
// its request runs, and ended after that: it counts for linger more, then
// goes. A client's part holds at most perClient entries; one more evicts
// the client's own oldest ended entry, and is refused when all of them
// are pinned. Ended entries are listed in the order they ended, which is
// the order of their ends since linger is fixed: door-wide for the
// expiry, per client for the eviction. Every operation is O(1), the
// expiry amortised. s.mu guards it, and its clock never goes back.
type memory[V any] struct {
	linger    time.Duration
	perClient int
	entries   map[string]*memEntry[V]
	clients   map[string]*memClient
	ended     list.List // of *memEntry[V], by until
}

// memClient is one client's part of a memory.
type memClient struct {
	n     int       // its entries, pinned and ended
	ended list.List // of its ended *memEntry, by until
}

// memEntry is one entry of a memory.
type memEntry[V any] struct {
	key, client string
	// until is when the entry stops counting; zero while it is pinned.
	until time.Time
	val   V
	// all and own are its elements in the memory's and its client's
	// lists once it ended; gone is set when it left the memory.
	all, own *list.Element
	gone     bool
}

func newMemory[V any](linger time.Duration, perClient int) *memory[V] {
	return &memory[V]{linger: linger, perClient: perClient, entries: map[string]*memEntry[V]{}, clients: map[string]*memClient{}}
}

// pinned reports whether the entry's request still runs.
func (e *memEntry[V]) pinned() bool { return e.until.IsZero() }

// get is the entry under key that counts at now, or nil.
func (m *memory[V]) get(key string, now time.Time) *memEntry[V] {
	m.expire(now)
	return m.entries[key]
}

// pin adds a pinned entry with val under key for client, in place of the
// entry under key, which goes even when the new one is refused. false:
// the client's part is full of pinned entries.
func (m *memory[V]) pin(client, key string, val V, now time.Time) (*memEntry[V], bool) {
	m.expire(now)
	if old := m.entries[key]; old != nil {
		m.remove(old)
	}
	if c := m.clients[client]; c != nil && c.n >= m.perClient {
		oldest := c.ended.Front()
		if oldest == nil {
			return nil, false
		}
		m.remove(oldest.Value.(*memEntry[V]))
	}
	c := m.clients[client]
	if c == nil {
		c = &memClient{}
		m.clients[client] = c
	}
	c.n++
	e := &memEntry[V]{key: key, client: client, val: val}
	m.entries[key] = e
	return e, true
}

// end ends the pinned entry e at now: it counts for linger more. An entry
// that ended or left the memory stays as it is.
func (m *memory[V]) end(e *memEntry[V], now time.Time) {
	if e.gone || !e.pinned() {
		return
	}
	e.until = now.Add(m.linger)
	e.all = m.ended.PushBack(e)
	e.own = m.clients[e.client].ended.PushBack(e)
}

// add adds an ended entry with val under key for client, which counts
// for linger from now, in place of the entry under key.
func (m *memory[V]) add(client, key string, val V, now time.Time) {
	if e, ok := m.pin(client, key, val, now); ok {
		m.end(e, now)
	}
}

// expire removes the ended entries whose time passed at now: the front of
// the door-wide list.
func (m *memory[V]) expire(now time.Time) {
	for front := m.ended.Front(); front != nil; front = m.ended.Front() {
		e := front.Value.(*memEntry[V])
		if now.Before(e.until) {
			return
		}
		m.remove(e)
	}
}

// remove takes e out of the memory.
func (m *memory[V]) remove(e *memEntry[V]) {
	if e.gone {
		return
	}
	e.gone = true
	delete(m.entries, e.key)
	c := m.clients[e.client]
	if e.all != nil {
		m.ended.Remove(e.all)
		c.ended.Remove(e.own)
		e.all, e.own = nil, nil
	}
	if c.n--; c.n == 0 {
		delete(m.clients, e.client)
	}
}

// register makes the call cancellable under key; the returned function
// removes it.
func (s *Server) register(key string, cancel context.CancelFunc) func() {
	s.mu.Lock()
	s.seq++
	n := s.seq
	set := s.calls[key]
	if set == nil {
		set = map[uint64]context.CancelFunc{}
		s.calls[key] = set
	}
	set[n] = cancel
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(set, n)
		if len(s.calls[key]) == 0 {
			delete(s.calls, key)
		}
		s.mu.Unlock()
	}
}

// registerKeyed makes the call with the id key in the sandbox id
// cancellable under the request's key mcpKey while the request runs, in
// the part of client; the returned function keeps it so for cancelLinger
// more, and cancels the call if a cancel reached the entry while the
// request ran. errClientFull when client has maxPerClient requests with
// an id running here.
func (s *Server) registerKeyed(client, mcpKey string, id podid.ID, key string) (func(), error) {
	s.mu.Lock()
	e, ok := s.keyed.pin(client, mcpKey, keyedCancel{sandbox: id, key: key}, s.now())
	s.mu.Unlock()
	if !ok {
		return nil, errClientFull
	}
	return func() {
		s.mu.Lock()
		s.keyed.end(e, s.now())
		cancelled := e.val.cancelled
		s.mu.Unlock()
		if cancelled {
			go s.cancelCall(e.val.sandbox, e.val.key)
		}
	}, nil
}

// cancelKey cancels this instance's calls under key and returns how many:
// the requests that run under it, and the call with an id of a request
// under it that runs or ended within cancelLinger. A cancel that finds
// none is remembered for recentFor in the part of client
// (recentlyCancelled).
//
// A request that runs is cancelled at once, which ends its wait; its call
// with an id is cancelled when the request ended (registerKeyed), so that
// a call the request was starting meanwhile is registered by then. The
// call of a request that ended is cancelled at once.
func (s *Server) cancelKey(client, key string) int {
	s.mu.Lock()
	now := s.now()
	cancels := make([]context.CancelFunc, 0, len(s.calls[key]))
	for _, cancel := range s.calls[key] {
		cancels = append(cancels, cancel)
	}
	n := len(cancels)
	var ended *memEntry[keyedCancel]
	if e := s.keyed.get(key, now); e != nil {
		n++
		if !e.val.cancelled {
			e.val.cancelled = true
			if !e.pinned() {
				ended = e
			}
		}
	}
	if n == 0 {
		s.recent.add(client, key, struct{}{}, now)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	if ended != nil {
		go s.cancelCall(ended.val.sandbox, ended.val.key)
	}
	return n
}

// recentlyCancelled reports whether a cancel under key came within
// recentFor and found no request: the request under key, which comes
// after its cancel, starts nothing.
func (s *Server) recentlyCancelled(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recent.get(key, s.now()) != nil
}

// cancelCall cancels the sandbox's call with the id key, on whichever
// instance runs it (tools.Runner.CancelCall), within cancelCallTimeout.
func (s *Server) cancelCall(id podid.ID, key string) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelCallTimeout)
	defer cancel()
	ok, err := s.runner.CancelCall(ctx, id, key)
	switch {
	case err != nil:
		s.rl.warn("cancel-call", "mcp: cancel a tool call with an id", "sandbox", id, "err", err)
	case ok:
		s.log.Debug("mcp: tool call with an id cancelled", "sandbox", id)
	}
}

// broadcastCancel handles a notifications/cancelled of a verified session
// of client: the call it names may run on any instance, so every instance
// is told with ferun_cancel, this one at once.
func (s *Server) broadcastCancel(ctx context.Context, client, session string, params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
		return
	}
	key := callKey(session, p.RequestID)
	if len(key) > maxCancelKey {
		return
	}
	s.cancelKey(client, key)
	if s.d == nil {
		return
	}
	payload, err := json.Marshal(cancelPayload{Kind: "mcp", Key: key, Client: client})
	if err != nil {
		return
	}
	if err := db.Notify(ctx, s.d.Pool, db.ChanCancel, string(payload)); err != nil && ctx.Err() == nil {
		s.rl.warn("cancel", "mcp: broadcast a cancellation", "err", err)
	}
}

// remoteCancel cancels this instance's calls that a ferun_cancel payload
// of kind mcp names, and returns how many (cancelKey); any other payload
// is ignored. A payload of an older instance names no client: its cancel
// goes into the part of the key's session.
func (s *Server) remoteCancel(payload string) int {
	var p cancelPayload
	if payload == "" || json.Unmarshal([]byte(payload), &p) != nil || p.Kind != "mcp" || p.Key == "" {
		return 0
	}
	client := p.Client
	if client == "" {
		session, _, _ := strings.Cut(p.Key, ":")
		client = "session:" + session
	}
	return s.cancelKey(client, p.Key)
}

// Run cancels this instance's tool calls that another instance was asked
// to cancel (ferun_cancel of kind mcp), until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	if s.d == nil {
		<-ctx.Done()
		return nil
	}
	ch, unsubscribe := s.d.Subscribe(db.ChanCancel)
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return nil
		case payload := <-ch:
			if n := s.remoteCancel(payload); n > 0 {
				s.log.Debug("mcp: tool call cancelled", "calls", n)
			}
		}
	}
}
