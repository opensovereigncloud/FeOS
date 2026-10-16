// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/apierr"
	"github.com/ironcore-dev/feos/go/internal/auth"
	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

var errNoSandbox = apierr.New(http.StatusNotFound, "no such sandbox")

// sandboxOf is the sandbox the request's token named (the sandbox class
// put it in the context).
func sandboxOf(r *http.Request) (auth.Sandbox, error) {
	sb, ok := auth.SandboxFrom(r.Context())
	if !ok {
		return auth.Sandbox{}, errNoSandbox
	}
	return sb, nil
}

// lookup reads the watch {wid} of the request's sandbox. A watch of
// another sandbox is not found rather than forbidden.
func (s *Service) lookup(r *http.Request) (store.Watch, error) {
	sb, err := sandboxOf(r)
	if err != nil {
		return store.Watch{}, err
	}
	wid, err := uuid.Parse(r.PathValue("wid"))
	if err != nil {
		return store.Watch{}, errNoWatch
	}
	row, err := s.st.GetWatch(r.Context(), wid)
	if errors.Is(err, store.ErrNoRow) {
		return store.Watch{}, errNoWatch
	}
	if err != nil {
		return store.Watch{}, fmt.Errorf("watch lookup: %w", err)
	}
	if row.SandboxID != sb.ID {
		return store.Watch{}, errNoWatch
	}
	return row, nil
}

// fail answers err; one that is not the API's own is logged, as its 500
// carries only its text.
func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status >= 500 {
		s.rl.warn("request", "watch: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	apierr.Write(w, err)
}

// Create is POST /api/v1/sandboxes/{id}/watches: a new watch owned by this
// instance, its set placed and acknowledged (or every path failed with
// the reason it is not placed) → 201. On any error the new watch is
// dropped: its id never reached its client.
func (s *Service) Create(w http.ResponseWriter, r *http.Request) {
	sb, err := sandboxOf(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	paths, err := readPaths(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	row, err := s.st.CreateWatch(r.Context(), store.NewWatch{ID: uuid.New(), SandboxID: sb.ID, Paths: paths}, s.id,
		s.t.lease, s.t.ownerLease)
	if errors.Is(err, model.ErrNotFound) {
		s.fail(w, r, errNoSandbox)
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("create the watch: %w", err))
		return
	}
	o := s.adopt(row)
	o.subMu.Lock()
	res, out, err := o.place(r.Context(), paths, row.Gen)
	o.subMu.Unlock()
	if err != nil {
		o.end(false)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.t.rowTimeout)
		if _, derr := s.st.DeleteWatch(ctx, row.ID); derr != nil {
			s.rl.warn("row", "watch: cannot drop a watch whose creation failed; it expires", "watch", row.ID, "err", derr)
		}
		cancel()
		s.fail(w, r, err)
		return
	}
	o.start()
	o.after(out)
	apierr.WriteJSON(w, http.StatusCreated, res)
}

// Update is PUT /api/v1/sandboxes/{id}/watches/{wid}: the complete set,
// placed by the owner → 200 with its answer. This instance places it when
// it owns the watch, or when the owner's lease has ended (it takes the
// watch over); otherwise it writes the set to the row and waits for the
// owner's answer.
func (s *Service) Update(w http.ResponseWriter, r *http.Request) {
	paths, err := readPaths(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	row, err := s.lookup(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var res any
	switch o := s.ownedAs(row); {
	case o != nil:
		res, err = s.put(r.Context(), o, paths)
	case row.OwnerLive && row.Owner != s.id:
		res, err = s.putRemote(r.Context(), row.ID, paths)
	default:
		res, err = s.putTaken(r.Context(), row.ID, paths)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, res)
}

// put places a set on a watch this instance owns: the row first (the set
// and its generation, the lease), then the guest.
func (s *Service) put(ctx context.Context, o *owned, paths []string) (any, error) {
	o.subMu.Lock()
	defer o.subMu.Unlock()
	row, err := s.st.SetWatchPaths(ctx, o.id, paths, s.t.lease)
	if errors.Is(err, store.ErrNoRow) {
		return nil, errNoWatch
	}
	if err != nil {
		return nil, fmt.Errorf("store the path set: %w", err)
	}
	if row.Owner != s.id || row.Epoch != o.epoch {
		// Taken over meanwhile: the new owner places it.
		return s.waitAnswer(ctx, row.ID, row.Gen)
	}
	res, out, err := o.place(ctx, paths, row.Gen)
	o.after(out)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// putRemote writes a set for the live owner on another instance and
// waits for its answer.
func (s *Service) putRemote(ctx context.Context, wid uuid.UUID, paths []string) (any, error) {
	row, err := s.st.SetWatchPaths(ctx, wid, paths, s.t.lease)
	if errors.Is(err, store.ErrNoRow) {
		return nil, errNoWatch
	}
	if err != nil {
		return nil, fmt.Errorf("store the path set: %w", err)
	}
	return s.waitAnswer(ctx, wid, row.Gen)
}

// putTaken takes over a watch whose owner's lease has ended, and places
// the set.
func (s *Service) putTaken(ctx context.Context, wid uuid.UUID, paths []string) (any, error) {
	o, _, err := s.take(ctx, wid)
	if err != nil {
		return nil, err
	}
	defer o.start()
	return s.put(ctx, o, paths)
}

// take makes this instance the watch's owner in a new epoch (the old
// owner learns it from ferun_watch). The caller places the set and then
// starts the owned watch's supervisor.
func (s *Service) take(ctx context.Context, wid uuid.UUID) (*owned, store.Watch, error) {
	row, err := s.st.TakeWatchOwnership(ctx, wid, s.id, s.t.lease, s.t.ownerLease)
	if errors.Is(err, store.ErrNoRow) {
		return nil, store.Watch{}, errNoWatch
	}
	if err != nil {
		return nil, store.Watch{}, fmt.Errorf("take the watch over: %w", err)
	}
	return s.adopt(row), row, nil
}

// waitAnswer waits, up to the ack timeout, until the row holds the
// owner's answer to the set of generation gen or a later one, and returns
// it. When the owner's lease ends meanwhile, this instance takes the
// watch over and places the row's set itself.
func (s *Service) waitAnswer(ctx context.Context, wid uuid.UUID, gen int64) (any, error) {
	wctx, cancel := context.WithTimeout(ctx, s.t.ackTimeout)
	defer cancel()
	for {
		answer, orphaned, err := s.waitRow(wctx, wid, gen)
		if err == nil && orphaned {
			o, row, err := s.take(ctx, wid)
			if err != nil {
				return nil, err
			}
			if o.epoch != row.Epoch {
				// Another take on this instance holds the watch in a later
				// epoch and places the row's set: its answer is waited for.
				continue
			}
			return s.placeTaken(ctx, o, row)
		}
		return s.answer(ctx, answer, err)
	}
}

// waitRow waits until the row holds the owner's answer to the set of
// generation gen or a later one, or names no live owner (orphaned).
func (s *Service) waitRow(ctx context.Context, wid uuid.UUID, gen int64) (answer json.RawMessage, orphaned bool,
	err error) {
	err = s.db.WaitFor(ctx, db.ChanWatch, wid.String(), s.t.putPoll, func(ctx context.Context) (bool, error) {
		row, err := s.st.GetWatch(ctx, wid)
		if errors.Is(err, store.ErrNoRow) {
			return false, errNoWatch
		}
		if err != nil {
			return false, err
		}
		if row.ResultGen >= gen && row.Result != nil {
			answer = row.Result
			return true, nil
		}
		if !row.OwnerLive {
			orphaned = true
			return true, nil
		}
		return false, nil
	})
	return answer, orphaned, err
}

// placeTaken places the set of the row a take returned on the owned watch
// it made, and starts its supervisor.
func (s *Service) placeTaken(ctx context.Context, o *owned, row store.Watch) (any, error) {
	defer o.start()
	o.subMu.Lock()
	defer o.subMu.Unlock()
	res, out, err := o.place(ctx, row.Paths, row.Gen)
	o.after(out)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// answer is what a PUT that waited for the owner returns: the owner's
// answer, or why there is none.
func (s *Service) answer(ctx context.Context, answer json.RawMessage, err error) (any, error) {
	switch {
	case err == nil:
		// The row holds it as jsonb, which reorders it; it goes out in the
		// owner's shape.
		var res response
		if err := json.Unmarshal(answer, &res); err != nil {
			return nil, fmt.Errorf("the owner's answer: %w", err)
		}
		return res, nil
	case ctx.Err() != nil:
		return nil, errCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return nil, s.errAckTimeout()
	}
	return nil, err
}

// Delete is DELETE /api/v1/sandboxes/{id}/watches/{wid} → 204: the row
// goes, and with ferun_watch its owner lets go of everything.
func (s *Service) Delete(w http.ResponseWriter, r *http.Request) {
	row, err := s.lookup(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	deleted, err := s.st.DeleteWatch(r.Context(), row.ID)
	if err != nil {
		s.fail(w, r, fmt.Errorf("delete the watch: %w", err))
		return
	}
	if !deleted {
		s.fail(w, r, errNoWatch)
		return
	}
	if o := s.ownedAs(row); o != nil {
		o.end(false)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Events is GET /api/v1/sandboxes/{id}/watches/{wid}/events: the event
// stream (SSE), served by the watch's owner; on another instance the
// request takes the watch over first. The stream replays from
// Last-Event-ID; a cursor the ring does not cover (older than the ring,
// or of an earlier owner) gets one stale event for the whole set. It ends
// when the client goes, and when the watch is deleted or expires; when
// the watch moves to another instance or this one stops, it is cut off,
// so that the client reconnects with its cursor.
func (s *Service) Events(w http.ResponseWriter, r *http.Request) {
	row, err := s.lookup(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	select {
	case <-s.ending:
		s.fail(w, r, apierr.New(http.StatusServiceUnavailable, "this instance is stopping"))
		return
	default:
	}
	o := s.ownedAs(row)
	if o == nil {
		var taken store.Watch
		o, taken, err = s.take(r.Context(), row.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		// An owned of a later epoch is placed by the take that made it.
		if o.epoch == taken.Epoch {
			o.subMu.Lock()
			_, out, err := o.place(r.Context(), taken.Paths, taken.Gen)
			o.subMu.Unlock()
			o.start()
			if err != nil {
				// The stream serves anyway; the supervisor places the set.
				out = outRetry
			}
			o.after(out)
		}
	}
	s.stream(w, r, o, lastEventID(r))
}

// stream serves the event stream of o from cursor.
func (s *Service) stream(w http.ResponseWriter, r *http.Request, o *owned, cursor uint64) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Proxies buffer a response body by default, which turns an event
	// stream into a file that arrives at the end.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	// The lease counts from the stream's opening and from its end.
	s.renewLease(r.Context(), o.id)
	defer s.renewLease(context.WithoutCancel(r.Context()), o.id)

	tick := time.NewTicker(s.t.keepalive)
	defer tick.Stop()
	for {
		evs, stale, wake := o.since(cursor)
		if stale {
			ev := o.staleEvent()
			if writeEvent(w, ev) != nil || rc.Flush() != nil {
				return
			}
			cursor = ev.ID
			continue
		}
		if len(evs) > 0 {
			for _, ev := range evs {
				if writeEvent(w, ev) != nil {
					return
				}
				cursor = ev.ID
			}
			if rc.Flush() != nil {
				return
			}
			continue
		}
		select {
		case <-wake:
		case <-tick.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
			// A keepalive that went out is what "the stream is open" means.
			s.renewLease(r.Context(), o.id)
		case <-o.done:
			o.mu.Lock()
			moved := o.moved
			o.mu.Unlock()
			if moved {
				cutOff(rc)
			}
			return
		case <-s.ending:
			cutOff(rc)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// cutOff ends a response as a dropped connection does, so that the client
// reconnects with its cursor: over HTTP/1 the connection closes without
// the closing chunk; over HTTP/2, which cannot be hijacked, a write
// deadline in the past resets the stream (RST_STREAM INTERNAL_ERROR)
// instead of ending it.
func cutOff(rc *http.ResponseController) {
	if conn, _, err := rc.Hijack(); err == nil {
		_ = conn.Close()
		return
	}
	_ = rc.SetWriteDeadline(time.Unix(1, 0))
}

// renewLease extends a watch's lease.
func (s *Service) renewLease(ctx context.Context, wid uuid.UUID) {
	rctx, cancel := context.WithTimeout(ctx, s.t.rowTimeout)
	defer cancel()
	if _, err := s.st.RenewWatchLease(rctx, wid, s.t.lease); err != nil && rctx.Err() == nil {
		s.rl.warn("row", "watch: cannot renew a watch's lease", "watch", wid, "err", err)
	}
}
