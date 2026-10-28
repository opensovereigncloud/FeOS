// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

func (c *client) hold(t *testing.T, req *wire.HoldRequest) *wire.HoldResponse {
	t.Helper()
	resp, err := c.api.Hold(bg, req)
	if err != nil {
		t.Fatalf("hold %+v: %v", req, err)
	}
	return resp
}

// holdsOf returns the holds of ingress name's holder on the sandbox id.
func (h *harness) holdsOf(id podid.ID, name string) []store.Hold {
	h.t.Helper()
	holds, err := h.st.ListHolds(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return slices.DeleteFunc(holds, func(x store.Hold) bool { return x.Holder != store.IngressHolder(name) })
}

// Hold admits, renews and releases the ingress's holds in the database,
// passes on the store's refusals and refuses invalid IDs, and Leaving
// releases every hold and records that the ingress left.
func TestHold(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.world()
	env := h.mkEnv("web", webSpec, true)
	sb := h.mkInstance(env, "web", model.PhaseReady, model.StateRunning, 8080)
	second := h.mkInstance(env, "web", model.PhaseReady, model.StateRunning, 8080)
	paused := h.mkInstance(env, "web", model.PhaseReady, model.StatePaused, 8080)
	private := h.mkInstance(h.mkEnv("priv", webSpec, false), "web", model.PhaseReady, model.StateRunning, 8080)
	busy := h.mkInstance(env, "web", model.PhaseReady, model.StateRunning, 8080)
	h.exec(`UPDATE sandboxes SET op = 'pause', op_id = gen_random_uuid(), op_owner = 'i2', op_target = 'paused',
		op_deadline = now() + interval '1 minute' WHERE id = $1`, busy)
	gone := podid.Mint()
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	h.exec("UPDATE ingresses SET last_seen = now() - interval '1 hour' WHERE name = 'in-0'")
	before := h.ingress("in-0").LastSeen

	// Admitted, refused, invalid; a repeated ID is admitted once.
	resp := c.hold(t, &wire.HoldRequest{Hold: []string{sb.String(), "bogus", gone.String(), private.String(),
		paused.String(), busy.String(), sb.String()}})
	want := &wire.HoldResponse{Held: []string{sb.String()}, LeaseSeconds: 30, Refused: []wire.HoldRefusal{
		{Instance: "bogus", Reason: "invalid"},
		{Instance: gone.String(), Reason: store.IngressHoldGone},
		{Instance: private.String(), Reason: store.IngressHoldNotPublic},
		{Instance: paused.String(), Reason: store.IngressHoldNotRunning},
		{Instance: busy.String(), Reason: store.IngressHoldBusy},
	}}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("hold %+v, want %+v", resp, want)
	}
	now, err := h.st.Now(bg)
	if err != nil {
		t.Fatal(err)
	}
	holds := h.holdsOf(sb, "in-0")
	if len(holds) != 1 || holds[0].Kind != model.HoldForward || holds[0].InstanceID != "i1" ||
		holds[0].LeaseUntil.Sub(now) < 25*time.Second || holds[0].LeaseUntil.Sub(now) > 31*time.Second {
		t.Fatalf("holds %+v at %v", holds, now)
	}
	if seen := h.ingress("in-0").LastSeen; !seen.After(before.Add(time.Minute)) {
		t.Fatalf("not seen: %v", seen)
	}
	for _, id := range []podid.ID{private, paused, busy} {
		if holds := h.holdsOf(id, "in-0"); len(holds) != 0 {
			t.Fatalf("a refused hold on %s: %+v", id, holds)
		}
	}

	// Renewed: the lease starts again.
	h.exec("UPDATE sandbox_holds SET lease_until = now() + interval '1 second' WHERE sandbox_id = $1", sb)
	if resp := c.hold(t, &wire.HoldRequest{Hold: []string{sb.String(), second.String()}}); len(resp.Held) != 2 {
		t.Fatalf("renew %+v", resp)
	}
	now, _ = h.st.Now(bg)
	if holds := h.holdsOf(sb, "in-0"); len(holds) != 1 || holds[0].LeaseUntil.Sub(now) < 25*time.Second {
		t.Fatalf("renewed %+v at %v", holds, now)
	}

	// Released; an invalid ID is refused.
	resp = c.hold(t, &wire.HoldRequest{Release: []string{sb.String(), "nope"}})
	if len(resp.Held) != 0 || !reflect.DeepEqual(resp.Refused, []wire.HoldRefusal{{Instance: "nope", Reason: "invalid"}}) {
		t.Fatalf("release %+v", resp)
	}
	if holds := h.holdsOf(sb, "in-0"); len(holds) != 0 {
		t.Fatalf("released %+v", holds)
	}
	if holds := h.holdsOf(second, "in-0"); len(holds) != 1 {
		t.Fatalf("the other hold %+v", holds)
	}

	// Leaving, after a hold in the same request: every hold released, the
	// ingress left.
	c.hold(t, &wire.HoldRequest{Hold: []string{sb.String()}, Leaving: true})
	for _, id := range []podid.ID{sb, second} {
		if holds := h.holdsOf(id, "in-0"); len(holds) != 0 {
			t.Fatalf("after leaving %s: %+v", id, holds)
		}
	}
	if g := h.ingress("in-0"); g.LeftAt.IsZero() {
		t.Fatalf("not left: %+v", g)
	}
	// A hold later is seen again: no longer left.
	c.hold(t, &wire.HoldRequest{Hold: []string{sb.String()}})
	if g := h.ingress("in-0"); !g.LeftAt.IsZero() {
		t.Fatalf("still left: %+v", g)
	}
}

// Each list holds at most wire.MaxHoldIDs.
func TestHoldLimits(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	full := slices.Repeat([]string{"x"}, wire.MaxHoldIDs)
	over := append(slices.Clone(full), "x")
	_, err := c.api.Hold(bg, &wire.HoldRequest{Hold: over})
	wantCode(t, "hold", err, codes.InvalidArgument, "more than 10000")
	_, err = c.api.Hold(bg, &wire.HoldRequest{Release: over})
	wantCode(t, "release", err, codes.InvalidArgument, "more than 10000")
	resp := c.hold(t, &wire.HoldRequest{Hold: full})
	if len(resp.Refused) != wire.MaxHoldIDs || len(resp.Held) != 0 || !strings.EqualFold(resp.Refused[0].Reason, "invalid") {
		t.Fatalf("10000 invalid IDs: %d refused, %d held", len(resp.Refused), len(resp.Held))
	}
}

// A statement that times out while the Hold goes on is UNAVAILABLE, not
// DEADLINE_EXCEEDED: the ingress sends the request again.
func TestHoldStalls(t *testing.T) {
	h := newHarness(t, harnessOpts{dbTimeout: 200 * time.Millisecond})
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820")
	h.lockIngresses()
	_, err := c.api.Hold(bg, &wire.HoldRequest{Leaving: true})
	wantCode(t, "a stalled leave", err, codes.Unavailable, "leave")
}
