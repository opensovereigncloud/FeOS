// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package mcpsrv

import (
	"context"
	"encoding/json"

	"github.com/gardenlinux/feos/go/internal/db"
)

// cancelPayload is the payload of ferun_cancel.
type cancelPayload struct {
	Kind string `json:"kind"`
	Key  string `json:"key,omitempty"`
}

// maxCancelKey bounds a cancel key that is broadcast: pg_notify takes
// 8000 bytes, and request ids are short.
const maxCancelKey = 1024

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

// cancelKey cancels this instance's calls under key and returns how many.
func (s *Server) cancelKey(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.calls[key] {
		cancel()
	}
	return len(s.calls[key])
}

// broadcastCancel handles a notifications/cancelled of a verified session:
// the call it names may run on any instance, so every instance is told
// with ferun_cancel, this one at once.
func (s *Server) broadcastCancel(ctx context.Context, session string, params json.RawMessage) {
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
	s.cancelKey(key)
	if s.d == nil {
		return
	}
	payload, err := json.Marshal(cancelPayload{Kind: "mcp", Key: key})
	if err != nil {
		return
	}
	if err := db.Notify(ctx, s.d.Pool, db.ChanCancel, string(payload)); err != nil && ctx.Err() == nil {
		s.rl.warn("cancel", "mcp: broadcast a cancellation", "err", err)
	}
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
			var p cancelPayload
			if payload == "" || json.Unmarshal([]byte(payload), &p) != nil || p.Kind != "mcp" || p.Key == "" {
				continue
			}
			if n := s.cancelKey(p.Key); n > 0 {
				s.log.Debug("mcp: tool call cancelled", "calls", n)
			}
		}
	}
}
