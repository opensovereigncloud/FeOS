// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// The idle holds' timing (D11).
const (
	// holdAdmitTimeout bounds the call that holds an instance before a
	// request goes to it.
	holdAdmitTimeout = 2 * time.Second
	// holdEvery is the renew loop's period, and how long after its last
	// request an instance counts as active.
	holdEvery = 10 * time.Second
	// holdMargin is taken off a hold's lease: a hold counts as valid
	// until its send time + lease − margin, so the renew loop always
	// renews it before the control plane lets it lapse.
	holdMargin = 10 * time.Second
	// holdLease is the lease assumed when an answer names none.
	holdLease = 30 * time.Second
	// holdRetry is how long after a failed admit an instance's requests
	// go on without calling again; the renew loop holds it meanwhile.
	holdRetry = 5 * time.Second
	// holdCallTimeout bounds one call of the renew loop.
	holdCallTimeout = 5 * time.Second
)

// holds holds, at the control plane, the instances of services with an
// idle policy that requests go to (D11), the way the port door does: the
// idle loop pauses no instance that a live hold names, and the hold is
// admitted under the sandbox's row lock, so no pause can freeze a request
// that reached the pod. The first request to an instance whose hold is
// not valid admits it (single-flight, within 2 s); every 10 s the
// instances with a request in flight or in the last 10 s are held again
// and the quiet ones released, so that the idle timer starts when the
// last request ends.
//
// When the control plane cannot be reached, the request goes on anyway:
// while an ingress is cut off, the control plane does not pause public
// environments' services for idleness (the idle fence).
type holds struct {
	holder Holder
	log    *slog.Logger
	m      *metrics
	now    func() time.Time

	mu       sync.Mutex
	inst     map[string]*holdState
	draining bool
}

// holdState is what the ingress knows of one instance's hold.
type holdState struct {
	// validUntil: the hold counts as valid until then.
	validUntil time.Time
	// held: the control plane holds the instance for this ingress, as
	// far as the ingress knows; a quiet instance is then released.
	held bool
	// retryAfter: a failed admit lets requests go on unheld until then.
	retryAfter time.Time
	inflight   int
	lastReq    time.Time
	admitting  *holdFlight
	// releasing is closed when the release of the hold in flight is
	// done: an admit waits for it, so that a release never lands after
	// the admit of a new request.
	releasing chan struct{}
}

// holdFlight is an admit in flight, which concurrent requests wait for.
type holdFlight struct {
	done    chan struct{}
	refused bool
}

func newHolds(holder Holder, log *slog.Logger, m *metrics) *holds {
	return &holds{holder: holder, log: log, m: m, now: time.Now, inst: map[string]*holdState{}}
}

// acquire counts a request to an instance and makes sure the instance is
// held before the request goes on. refused: the control plane refused
// to hold it (it is not running, or busy with an operation), and the
// request must go elsewhere. Otherwise release ends the request's count.
// Requests that come while an admit is in flight share its result. A
// request whose context ends while it waits goes on as well; its forward
// fails at once.
func (h *holds) acquire(ctx context.Context, id string) (release func(), refused bool) {
	h.mu.Lock()
	st := h.inst[id]
	if st == nil {
		st = &holdState{}
		h.inst[id] = st
	}
	st.inflight++
	var once sync.Once
	release = func() {
		once.Do(func() {
			h.mu.Lock()
			st.inflight--
			st.lastReq = h.now()
			h.mu.Unlock()
		})
	}
	for {
		now := h.now()
		if h.draining || now.Before(st.validUntil) || now.Before(st.retryAfter) {
			h.mu.Unlock()
			return release, false
		}
		if ch := st.releasing; ch != nil {
			h.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return release, false
			}
			h.mu.Lock()
			continue
		}
		f := st.admitting
		if f != nil {
			h.mu.Unlock()
			select {
			case <-f.done:
			case <-ctx.Done():
				return release, false
			}
		} else {
			f = &holdFlight{done: make(chan struct{})}
			st.admitting = f
			h.mu.Unlock()
			f.refused = h.admit(ctx, id, st)
			h.mu.Lock()
			st.admitting = nil
			close(f.done)
			h.mu.Unlock()
		}
		if f.refused {
			// A refused request is no request to the instance.
			h.mu.Lock()
			st.inflight--
			h.mu.Unlock()
			return nil, true
		}
		return release, false
	}
}

// admit holds one instance, for the requests that wait for it. It
// reports whether the control plane refused; a failed call lets the
// requests go on unheld for holdRetry.
func (h *holds) admit(ctx context.Context, id string, st *holdState) (refused bool) {
	sent := h.now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), holdAdmitTimeout)
	defer cancel()
	h.m.holdCalls.Add(1)
	res, err := h.holder.Hold(ctx, wire.HoldRequest{Hold: []string{id}})
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.m.holdAdmitFailures.Add(1)
		st.retryAfter = sent.Add(holdRetry)
		h.log.Debug("hold an instance failed; the request goes on", "instance", id, "err", err)
		return false
	}
	for _, r := range res.Refused {
		if r.Instance == id {
			h.m.holdRefusals.Add(1)
			st.held, st.validUntil = false, time.Time{}
			h.log.Debug("the control plane refused to hold an instance", "instance", id, "reason", r.Reason)
			return true
		}
	}
	if slices.Contains(res.Held, id) {
		st.held, st.validUntil = true, sent.Add(leaseOf(res)-holdMargin)
		return false
	}
	h.m.holdAdmitFailures.Add(1)
	st.retryAfter = sent.Add(holdRetry)
	h.log.Debug("the control plane neither held nor refused an instance", "instance", id)
	return false
}

func leaseOf(res wire.HoldResponse) time.Duration {
	if res.LeaseSeconds <= 0 {
		return holdLease
	}
	return time.Duration(res.LeaseSeconds) * time.Second
}

// pass is one turn of the renew loop: it holds again every instance with
// a request in flight or in the last holdEvery, and releases the held
// ones without. A failed call leaves the holds to lapse by their lease.
func (h *holds) pass(ctx context.Context) {
	if h.holder == nil {
		return
	}
	now := h.now()
	var renew, release []string
	h.mu.Lock()
	for id, st := range h.inst {
		switch {
		case st.admitting != nil || st.releasing != nil:
		case st.inflight > 0 || now.Sub(st.lastReq) < holdEvery:
			renew = append(renew, id)
		case st.held:
			// From now on the hold is not valid: a new request admits
			// it again once the release is done.
			st.held, st.validUntil = false, time.Time{}
			st.releasing = make(chan struct{})
			release = append(release, id)
		default:
			delete(h.inst, id)
		}
	}
	h.mu.Unlock()
	slices.Sort(renew)
	slices.Sort(release)
	for len(renew) > 0 || len(release) > 0 {
		var req wire.HoldRequest
		req.Hold, renew = cut(renew, wire.MaxHoldIDs)
		req.Release, release = cut(release, wire.MaxHoldIDs)
		h.call(ctx, now, req)
	}
}

// call sends one request of the renew loop and records its answer.
func (h *holds) call(ctx context.Context, sent time.Time, req wire.HoldRequest) {
	ctx, cancel := context.WithTimeout(ctx, holdCallTimeout)
	defer cancel()
	h.m.holdCalls.Add(1)
	res, err := h.holder.Hold(ctx, req)
	h.mu.Lock()
	defer h.mu.Unlock()
	// A release is done whether or not the call went through: the hold
	// lapses by its lease otherwise.
	for _, id := range req.Release {
		st := h.inst[id]
		if st == nil {
			continue
		}
		if st.releasing != nil {
			close(st.releasing)
			st.releasing = nil
		}
		if st.inflight == 0 && st.admitting == nil && sent.Sub(st.lastReq) >= holdEvery {
			delete(h.inst, id)
		}
	}
	if err != nil {
		h.m.holdRenewFailures.Add(1)
		h.log.Debug("renew the idle holds failed", "hold", len(req.Hold), "release", len(req.Release), "err", err)
		return
	}
	valid := sent.Add(leaseOf(res) - holdMargin)
	for _, id := range res.Held {
		if st := h.inst[id]; st != nil {
			st.held, st.validUntil = true, valid
		}
	}
	for _, r := range res.Refused {
		if st := h.inst[r.Instance]; st != nil {
			st.held, st.validUntil = false, time.Time{}
		}
	}
}

func cut(ids []string, n int) (head, rest []string) {
	if len(ids) <= n {
		return ids, nil
	}
	return ids[:n], ids[n:]
}

// drain stops admits: a request goes on unheld from now on. The renew
// loop goes on for the requests in flight.
func (h *holds) drain() {
	h.mu.Lock()
	h.draining = true
	h.mu.Unlock()
}

// held lists the instances whose hold is valid, sorted.
func (h *holds) held() []string {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	var ids []string
	for _, id := range slices.Sorted(maps.Keys(h.inst)) {
		if st := h.inst[id]; st.held && now.Before(st.validUntil) {
			ids = append(ids, id)
		}
	}
	return ids
}
