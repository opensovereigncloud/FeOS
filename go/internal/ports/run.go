// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ports

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/podid"
)

// Run keeps the pools true to the sandboxes until ctx ends: on
// ferun_sandbox for a sandbox with pools or upgraded connections, it reads
// the row, retires the pools whose pod is no longer the running one on
// their node (the sandbox left running, moved, or was deleted and created
// again) and closes the upgraded connections to such a pod, whose clients
// reconnect to where it runs next; when a node's last connection to this
// instance is gone, it retires the pools on that node; and it evicts the
// pools unused for a while. At its end every Forward stream ends.
func (h *Handler) Run(ctx context.Context) error {
	defer h.cancelBase()
	var notes <-chan string
	if h.db != nil {
		ch, unsubscribe := h.db.Subscribe(db.ChanSandbox)
		defer unsubscribe()
		notes = ch
	}
	events, unsubscribe := h.conns.Subscribe()
	defer unsubscribe()
	tick := time.NewTicker(h.t.sweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			h.retireWhere(func(*entry) bool { return true })
			h.sweep()
			return nil
		case p := <-notes:
			h.recheck(ctx, p)
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if !ev.Connected {
				h.retireWhere(func(e *entry) bool { return e.node == ev.Node })
			}
		case <-tick.C:
			h.sweep()
		}
	}
}

// recheck reads the rows of the sandboxes a ferun_sandbox payload names
// ("" or "*": every sandbox with pools or upgraded connections), retires
// their stale pools and closes their stale upgraded connections.
func (h *Handler) recheck(ctx context.Context, payload string) {
	var ids []podid.ID
	h.mu.Lock()
	known := map[podid.ID]bool{}
	for k := range h.transports {
		known[k.id] = true
	}
	for u := range h.upgraded {
		known[u.lease.SandboxID] = true
	}
	if payload == "" || payload == "*" {
		for id := range known {
			ids = append(ids, id)
		}
	} else if id, err := podid.Parse(payload); err == nil && known[id] {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	if h.st == nil {
		return
	}
	for _, id := range ids {
		rctx, cancel := context.WithTimeout(ctx, h.t.rowTimeout)
		row, err := h.st.GetSandbox(rctx, id)
		cancel()
		switch {
		case errors.Is(err, model.ErrNotFound):
			h.retireWhere(func(e *entry) bool { return e.key.id == id })
			h.closeUpgradedWhere(func(l model.Lease) bool { return l.SandboxID == id })
		case err != nil:
			if ctx.Err() == nil {
				h.rl.warn("row", "forward: cannot read a sandbox's row", "sandbox", id, "err", err)
			}
		default:
			h.retireWhere(func(e *entry) bool {
				return e.key.id == id && (row.ClientState != model.StateRunning || row.Node != e.node ||
					row.Incarnation != e.incarnation)
			})
			h.closeUpgradedWhere(func(l model.Lease) bool {
				return l.SandboxID == id && (row.ClientState != model.StateRunning || row.Node != l.Node ||
					row.Incarnation != l.Incarnation)
			})
		}
	}
}

// upgradedConn is the backend side of an upgraded connection (a
// WebSocket, a hot-reload socket): the body of the 101 response, which
// the proxy pipes to the client's hijacked connection until either ends.
// lease is where it goes: the sandbox's pod on its node.
type upgradedConn struct {
	io.ReadWriteCloser
	h     *Handler
	lease model.Lease
	once  sync.Once
}

// Close ends the connection and forgets it; it may be called more than
// once (the proxy and proxyHeld both close it).
func (u *upgradedConn) Close() error {
	err := u.ReadWriteCloser.Close()
	u.once.Do(func() {
		u.h.mu.Lock()
		delete(u.h.upgraded, u)
		u.h.mu.Unlock()
	})
	return err
}

// trackUpgrade records an upgraded connection and counts its request,
// which upgradeEnded uncounts once its hold is released. After
// CloseUpgraded it is closed at once.
func (h *Handler) trackUpgrade(rwc io.ReadWriteCloser, lease model.Lease) *upgradedConn {
	u := &upgradedConn{ReadWriteCloser: rwc, h: h, lease: lease}
	h.mu.Lock()
	h.upgraded[u] = struct{}{}
	if h.upActive == 0 {
		h.upIdle = make(chan struct{})
	}
	h.upActive++
	closing := h.upClosing
	h.mu.Unlock()
	if closing {
		_ = u.Close()
	}
	return u
}

// closeUpgradedWhere closes the upgraded connections whose lease match
// holds: their holds go once their requests end, so that a sandbox that
// leaves running, as a drain moves it (§9), is not kept by a WebSocket.
func (h *Handler) closeUpgradedWhere(match func(model.Lease) bool) {
	h.mu.Lock()
	var conns []*upgradedConn
	for u := range h.upgraded {
		if match(u.lease) {
			conns = append(conns, u)
		}
	}
	h.mu.Unlock()
	for _, u := range conns {
		_ = u.Close()
	}
}

func (h *Handler) upgradeEnded() {
	h.mu.Lock()
	h.upActive--
	if h.upActive == 0 {
		close(h.upIdle)
	}
	h.mu.Unlock()
}

// CloseUpgraded closes the upgraded connections (§7 step 2, once the HTTP
// server's shutdown has returned, which neither waits for nor tracks
// them) and waits, briefly, until their requests have released their
// forward holds. Their clients reconnect to another instance. Upgrades
// that arrive afterwards are closed at once.
func (h *Handler) CloseUpgraded() {
	h.mu.Lock()
	h.upClosing = true
	conns := make([]*upgradedConn, 0, len(h.upgraded))
	for u := range h.upgraded {
		conns = append(conns, u)
	}
	active, idle := h.upActive, h.upIdle
	h.mu.Unlock()
	for _, u := range conns {
		_ = u.Close()
	}
	if active == 0 {
		return
	}
	t := time.NewTimer(h.t.upgradedWait)
	defer t.Stop()
	select {
	case <-idle:
	case <-t.C:
		h.log.Warn("upgraded connections still open after closing them", "wait", h.t.upgradedWait)
	}
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
