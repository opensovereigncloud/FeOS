// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"strings"
	"time"
)

// The defaults of D19: the identity is lost after refusals that persist
// this long, over at least this many connection attempts.
const (
	refusalWindow   = 5 * time.Minute
	refusalAttempts = 3
)

// refusals is D19's policy, as a state machine without clocks or I/O: it
// is told of every connection attempt the control plane refused (with
// PERMISSION_DENIED and its text) and of every successful Hello, and says
// when the identity is lost.
//
// A single refusal never loses the identity: the control plane may refuse
// for a while (a database restored from a backup, a renewal not yet
// recorded) and recover. The identity is lost when a refusal says the
// ingress was removed, an operator's decision; or when refusals persist
// for window over at least attempts connection attempts without a
// successful Hello in between.
type refusals struct {
	window   time.Duration
	attempts int

	count     int       // refused attempts since the last successful Hello
	first     time.Time // the first of them
	total     int       // refused attempts ever, for the status
	lastText  string
	lastAt    time.Time
	helloAt   time.Time // the last successful Hello
	isLost    bool
	isRemoved bool
}

func newRefusals(window time.Duration, attempts int) refusals {
	return refusals{window: window, attempts: attempts}
}

// isRemoval reports whether a refusal's text says that the ingress was
// removed ("ingress <n> was removed; join it again").
func isRemoval(text string) bool { return strings.Contains(text, "was removed") }

// observe records a connection attempt refused at now with text.
func (r *refusals) observe(text string, now time.Time) {
	if r.count == 0 {
		r.first = now
	}
	r.count++
	r.total++
	r.lastText, r.lastAt = text, now
	switch {
	case isRemoval(text):
		r.isLost, r.isRemoved = true, true
	case r.count >= r.attempts && now.Sub(r.first) >= r.window:
		r.isLost = true
	}
}

// connected records a successful Hello at now: the refusals before it no
// longer count.
func (r *refusals) connected(now time.Time) {
	r.count = 0
	r.first = time.Time{}
	r.helloAt = now
}

// lost reports whether the identity is lost, and whether because the
// ingress was removed.
func (r *refusals) lost() (lost bool, removed bool) { return r.isLost, r.isRemoved }

// reset starts over for a new identity, keeping what the status shows.
func (r *refusals) reset() {
	r.count = 0
	r.first = time.Time{}
	r.isLost, r.isRemoved = false, false
}
