// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// An attached service's idle: seconds, 0 for never, nil for none.
func TestAttachedIdleOf(t *testing.T) {
	for in, want := range map[string]int{"15m0s": 900, spec.IdleNever: 0, "1h": 3600} {
		if got := attachedIdleOf(spec.Service{Idle: in}); got == nil || *got != want {
			t.Errorf("%q: %v, want %d", in, got, want)
		}
	}
	for _, in := range []string{"", "soon"} {
		if got := attachedIdleOf(spec.Service{Idle: in}); got != nil {
			t.Errorf("%q: %d, want none", in, *got)
		}
	}
}

// A process the look started keeps its sandbox awake for the budget; a
// ready one, or one whose process is gone, no longer; an unchanged process
// keeps the time it had; the plan's idle is taken, the row's pointer kept
// when it is the same, so that a look that changes nothing writes nothing.
func TestKeepAwake(t *testing.T) {
	now := time.Date(2026, 10, 30, 21, 0, 0, 0, time.UTC)
	budget := 10 * time.Minute
	fifteen := 900
	p := &attachedPlan{idle: &fifteen}

	before := store.EnvAttached{Phase: model.PhaseStarting}
	after := before
	after.ProcessID = "p1"
	keepAwake(&after, before, p, now, budget)
	if !after.AwakeUntil.Equal(now.Add(budget)) || after.IdleAfterS == nil || *after.IdleAfterS != 900 {
		t.Fatalf("started: %+v", after)
	}

	before = after
	later := before
	keepAwake(&later, before, &attachedPlan{idle: &fifteen}, now.Add(time.Minute), budget)
	if later != before {
		t.Fatalf("a look that changes nothing: %+v, was %+v", later, before)
	}

	ready := before
	ready.Phase = model.PhaseReady
	keepAwake(&ready, before, p, now, budget)
	if !ready.AwakeUntil.IsZero() {
		t.Fatalf("ready: %+v", ready)
	}

	gone := before
	gone.ProcessID, gone.Phase = "", model.PhaseExited
	keepAwake(&gone, before, &attachedPlan{}, now, budget)
	if !gone.AwakeUntil.IsZero() || gone.IdleAfterS != nil {
		t.Fatalf("gone, no idle: %+v", gone)
	}
}
