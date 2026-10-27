// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package mcpsrv

import (
	"log/slog"
	"sync"
	"time"
)

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
