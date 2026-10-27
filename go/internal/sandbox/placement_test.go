// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// readyNode is a node that passes every gate for a small pod.
func readyNode(name string) gateInput {
	return gateInput{
		node: store.Node{Name: name, Status: model.NodeReady, Slots: 50, MemHeadroomBytes: 1 << 30, CPUOvercommit: 16,
			CPUCores: 16, MemoryTotalBytes: 64 << 30, ObjectStore: true, ObjectStoreID: "s1"},
		telemetry:    store.Telemetry{MemoryAvailableBytes: 60 << 30, DiskAvailableBytes: 100 << 30},
		hasTelemetry: true, connected: true, vcpus: 2, memMiB: 512, diskBudget: 4 << 30,
	}
}

func TestGates(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*gateInput)
		want string
	}{
		{"ready", func(*gateInput) {}, ""},
		{"down", func(g *gateInput) { g.node.Status = model.NodeDown }, "node down"},
		{"joined", func(g *gateInput) { g.node.Status = model.NodeJoined }, "node joined"},
		{"draining", func(g *gateInput) { g.node.Draining = true }, "draining"},
		{"forked", func(g *gateInput) { g.node.IdentityForkedAt = time.Now() }, "identity forked"},
		{"not connected", func(g *gateInput) { g.connected = false }, "not connected to this instance"},
		{"tombstone", func(g *gateInput) { g.tombstoned = true }, "deleting an earlier pod of this ID"},
		{"avoided", func(g *gateInput) { g.avoid = []string{"x", "n"} }, "a create failed here"},
		{"tap", func(g *gateInput) { g.tap = "11111111-0000-4000-8000-000000000000" },
			"TAP prefix taken by sandbox 11111111-0000-4000-8000-000000000000"},
		{"store elsewhere", func(g *gateInput) { g.restore, g.checkpointStore = true, "s2" }, "another object store"},
		{"store same", func(g *gateInput) { g.restore, g.checkpointStore = true, "s1" }, ""},
		{"store any", func(g *gateInput) { g.restore = true }, ""},
		{"no store", func(g *gateInput) { g.restore, g.node.ObjectStore, g.node.ObjectStoreID = true, false, "" }, "another object store"},
		{"store gate only for restores", func(g *gateInput) { g.checkpointStore, g.node.ObjectStoreID = "s2", "" }, ""},
		{"no slots", func(g *gateInput) { g.node.Slots = 0 }, "no slots"},
		{"slots full", func(g *gateInput) { g.node.Slots, g.booked.Count = 3, 3 }, "slots full (3 of 3)"},
		{"cpu", func(g *gateInput) { g.node.CPUCores, g.node.CPUOvercommit, g.booked.VCPUs = 2, 1.5, 2 }, "cpu gate (2+2 of 3 vCPUs)"},
		{"overcommit 0", func(g *gateInput) { g.node.CPUOvercommit, g.booked.VCPUs = 0, 1000 }, ""},
		{"cores 0", func(g *gateInput) { g.node.CPUCores, g.booked.VCPUs = 0, 1000 }, ""},
		{"memory", func(g *gateInput) { g.node.MemoryTotalBytes, g.booked.MemoryMiB = 2<<30, 800 },
			"memory gate (800MiB+512MiB of 1024MiB MiB)"},
		{"free memory", func(g *gateInput) { g.telemetry.MemoryAvailableBytes = 1<<30 + 100<<20 }, "free memory gate (100 MiB available)"},
		{"disk", func(g *gateInput) { g.telemetry.DiskAvailableBytes = 1 << 30 }, "disk gate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := readyNode("n")
			c.mod(&in)
			got := ""
			if r := in.check(placementGates); r != nil {
				got = r.text
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
	// The order: the first gate that refuses names the reason.
	in := readyNode("n")
	in.node.Status, in.node.Draining, in.node.Slots = model.NodeDown, true, 0
	if r := in.check(placementGates); r == nil || r.gate != gateStatus {
		t.Fatalf("first refusal %+v", r)
	}
	// Admission on a pinned node looks at no tombstone, TAP or live
	// figure.
	in = readyNode("n")
	in.tombstoned, in.tap, in.telemetry = true, legacyID(), store.Telemetry{}
	if r := in.check(admitGates); r != nil {
		t.Fatalf("admission refused %+v", r)
	}
	// Scores: the scarcest resource's free share.
	in = readyNode("n")
	// Memory capacity 63 GiB, 31.5 GiB booked with the pod: half free.
	in.booked.MemoryMiB = 31<<10 - 512 + 512
	if s := in.score(); s != 0.5 {
		t.Fatalf("score %v", s)
	}
	// CPU is the scarcest: 256 vCPUs, 192 booked with the pod.
	in.booked.MemoryMiB, in.booked.VCPUs = 0, 190
	if s := in.score(); s != 0.25 {
		t.Fatalf("score %v", s)
	}
}

func TestPlacementPolicies(t *testing.T) {
	for _, policy := range []string{PlacementBinpack, PlacementSpread} {
		t.Run(policy, func(t *testing.T) {
			h := newHarness(t, func(c *Config) { c.Placement = policy })
			h.addNode("n1")
			h.addNode("n2")
			first := h.running(8192)
			second := h.running(512)
			want := first.Node
			if policy == PlacementSpread {
				want = map[string]string{"n1": "n2", "n2": "n1"}[first.Node]
			}
			if second.Node != want {
				t.Fatalf("%s placed the second sandbox on %s after the first on %s", policy, second.Node, first.Node)
			}
			// The audit, the placement's activity row, names every
			// candidate's verdict.
			h.placements(second.ID, "i1", 1)
			d := h.activityNamed(second.ID, activity.KindPlacement, fmt.Sprintf("placed on %s (create)", want))
			if cands, _ := d["candidates"].([]any); len(cands) != 2 || d["node"] != want || d["trigger"] != string(TriggerEnsure) {
				t.Fatalf("placement %v", d)
			}
		})
	}
}

func TestPlacementGatesIntegrated(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	h.addNode("n2", func(c *testnode.Config) { c.ObjectStore = testnode.NewObjectStore("store-2") })
	e := h.eng()

	// avoid_nodes and a tombstone keep a pod off a node.
	sb := h.sandbox(512)
	h.exec("UPDATE sandboxes SET avoid_nodes = '{n1}' WHERE id = $1", sb.ID)
	h.tombstone(sb.ID, "n2", uuid.New(), time.Now().Add(time.Hour))
	p, err := e.place(bg, h.row(sb.ID), OpCreate)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.candidates) != 0 || p.noCapacity() !=
		"no capacity (retryable): no feasible node; n1: a create failed here; n2: deleting an earlier pod of this ID" {
		t.Fatalf("placement %+v: %s", p, p.noCapacity())
	}
	if len(p.tombstoneOnly) != 1 || p.tombstoneOnly[0] != "n2" {
		t.Fatalf("tombstone-only nodes %v", p.tombstoneOnly)
	}

	// A TAP prefix another sandbox holds on a node: both have UUIDs (made
	// before ADR 0019), which share it.
	a := h.sandboxWithID(legacyID(), 512)
	h.ensure(a.ID)
	a = h.row(a.ID)
	twin := podid.ID(a.ID.String()[:8] + "-0000-4000-8000-000000000001")
	if twin != a.ID {
		b := h.sandboxWithID(twin, 512)
		p, err := e.place(bg, b, OpCreate)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range p.candidates {
			if c == a.Node {
				t.Fatalf("placed beside the TAP prefix's holder: %+v", p)
			}
		}
		h.ensure(b.ID)
		if h.row(b.ID).Node == a.Node {
			t.Fatal("both on one node")
		}
	}

	// A restore goes to a node of its checkpoint's object store only.
	c := h.running(512)
	h.startOp(c.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: c.Node})
	h.startOp(c.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: c.Node})
	row := h.row(c.ID)
	p, err = e.place(bg, row, OpRestore)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.candidates) != 1 || p.candidates[0] != c.Node {
		t.Fatalf("restore candidates %v (%+v)", p.candidates, p.verdicts)
	}
	for _, v := range p.verdicts {
		if v.Node != c.Node && v.Reason != "another object store" {
			t.Fatalf("verdict %+v", v)
		}
	}
}

// TestShortIDsShareNoTAP: a short ID names its TAP device whole (ADR
// 0019), so the TAP gate refuses none, also not beside a UUID whose first
// 8 hex digits begin the short ID; two UUIDs that share them still do.
func TestShortIDsShareNoTAP(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	legacy := h.sandboxWithID("abcdef12-0000-4000-8000-000000000000", 512)
	h.ensure(legacy.ID)
	for _, id := range []podid.ID{"abcdef12xyz", "abcdef12xyy", podid.Mint()} {
		sb := h.sandboxWithID(id, 512)
		p, err := e.place(bg, sb, OpCreate)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.candidates) != 1 || p.candidates[0] != "n1" {
			t.Fatalf("%s: candidates %v (%+v)", id, p.candidates, p.verdicts)
		}
		if lease := h.ensure(sb.ID); lease.Node != "n1" {
			t.Fatalf("%s placed on %s", id, lease.Node)
		}
		h.podState("n1", id, podv1.PodState_POD_STATE_RUNNING)
	}
	h.podState("n1", legacy.ID, podv1.PodState_POD_STATE_RUNNING)
	// A second UUID with the legacy one's first 8 hex digits finds no node.
	twin := h.sandboxWithID("abcdef12-0000-4000-8000-000000000001", 512)
	p, err := e.place(bg, twin, OpCreate)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.candidates) != 0 || p.noCapacity() !=
		"no capacity (retryable): no feasible node; n1: TAP prefix taken by sandbox "+legacy.ID.String() {
		t.Fatalf("twin placement %+v: %s", p, p.noCapacity())
	}
}

func TestConcurrentPlacementNeverOverbooks(t *testing.T) {
	h := newHarness(t)
	h.addInstance("i2")
	h.addNode("n1")
	h.addNode("n2")
	h.exec("UPDATE nodes SET slots = 3")
	const n = 10
	var sbs []store.Sandbox
	for range n {
		sbs = append(sbs, h.sandbox(512))
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, sb := range sbs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(bg, 60*time.Second)
			defer cancel()
			_, errs[i] = h.ins[i%2].eng.Ensure(ctx, sb.ID)
		}()
	}
	wg.Wait()
	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case !containsAny(err.Error(), "no capacity"):
			t.Fatalf("sandbox %d: %v", i, err)
		}
	}
	if ok != 6 {
		t.Fatalf("%d sandboxes placed, want 6", ok)
	}
	for _, node := range []string{"n1", "n2"} {
		var booked int
		h.scalar(&booked, "SELECT count(*) FROM bookings WHERE node = $1", node)
		if booked != 3 {
			t.Fatalf("node %s booked %d of 3 slots", node, booked)
		}
	}
	var running int
	h.scalar(&running, "SELECT count(*) FROM sandboxes WHERE client_state = 'running'")
	if running != 6 {
		t.Fatalf("%d running", running)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
