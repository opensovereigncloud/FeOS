// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

func (h *harness) report(node, serial string, t *fleetv1.Telemetry) *fleetv1.TelemetryResponse {
	h.t.Helper()
	resp, err := h.s.UplinkServer().ReportTelemetry(uplinkCtx(node, serial), t)
	if err != nil {
		h.t.Fatalf("telemetry: %v", err)
	}
	return resp
}

func TestTelemetry(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	h.links.setConnected("n1", true)
	start := uuid.New()
	tel := &fleetv1.Telemetry{
		StartId: start.String(), Hostname: "host\x00one\xff", Arch: "x86_64", CpuModel: "EPYC", CpuCores: math.MaxUint32,
		Load1: math.NaN(), MemoryTotalBytes: math.MaxUint64, MemoryAvailableBytes: 1 << 33,
		DiskTotalBytes: 1 << 40, DiskAvailableBytes: math.MaxInt64 + 10,
		PodsPending: 1, PodsRunning: 2, PodsSuspended: 3, PodsOther: math.MaxUint32,
		Versions:    &fleetv1.Versions{Feosd: "1.2.\x003", Kernel: "6.12.48", Firecracker: []string{"1.10", "1.9"}, Guest: []string{"g1"}, Diskd: "d1"},
		ObjectStore: true, ObjectStoreId: "abc", PodPrefix: "2001:db8:1:2::/64",
	}
	resp := h.report("n1", serial, tel)
	if resp.GetIntervalSeconds() != 5 {
		t.Fatalf("interval %d", resp.GetIntervalSeconds())
	}
	n := h.node("n1")
	if n.Status != model.NodeReady || n.Hostname != "hostone�" || n.CPUCores != math.MaxInt32 ||
		n.MemoryTotalBytes != math.MaxInt64 || n.DiskTotalBytes != 1<<40 || !n.ObjectStore || n.ObjectStoreID != "abc" ||
		n.PodPrefix != "2001:db8:1:2::/64" || n.StartID != start || n.CPUModel != "EPYC" || n.Arch != "x86_64" {
		t.Fatalf("node %+v", n)
	}
	var versions map[string]any
	if err := json.Unmarshal(n.Versions, &versions); err != nil || versions["feosd"] != "1.2.3" || versions["kernel"] != "6.12.48" ||
		len(versions["firecracker"].([]any)) != 2 || versions["diskd"] != "d1" {
		t.Fatalf("versions %s: %v", n.Versions, err)
	}
	tl, err := h.st.GetTelemetry(bg, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if tl.Load1 != 0 || tl.DiskAvailableBytes != math.MaxInt64 || tl.PodsOther != math.MaxInt32 || tl.PodsRunning != 2 ||
		time.Since(tl.LastSeen) > time.Minute {
		t.Fatalf("telemetry %+v", tl)
	}
	// The first start is a new start: a node-scoped node_start row with
	// the start, and a reconcile.
	var starts int
	h.scalar(&starts, "SELECT count(*) FROM activity WHERE node = 'n1' AND kind = 'node_start' AND start_id = $1 AND source = 'ferund'", start)
	if starts != 1 {
		t.Fatalf("%d node_start rows", starts)
	}
	h.expectReconcile("n1")

	// The same start again: nothing new.
	h.allowReconcile("n1")
	h.report("n1", serial, tel)
	h.scalar(&starts, "SELECT count(*) FROM activity WHERE node = 'n1' AND kind = 'node_start'")
	if starts != 1 {
		t.Fatalf("%d node_start rows after the same start", starts)
	}
	h.expectNoReconcile()

	// Down → ready, clearing down_since and the description; draining
	// keeps its description.
	h.exec("UPDATE nodes SET status = 'down', down_since = now(), status_description = 'heartbeat expired' WHERE name = 'n1'")
	h.report("n1", serial, tel)
	if n := h.node("n1"); n.Status != model.NodeReady || !n.DownSince.IsZero() || n.StatusDescription != "" {
		t.Fatalf("node after a report while down %+v", n)
	}
	h.exec("UPDATE nodes SET status = 'down', down_since = now(), draining = true, status_description = 'draining: 3 left' WHERE name = 'n1'")
	h.report("n1", serial, tel)
	if n := h.node("n1"); n.Status != model.NodeReady || n.StatusDescription != "draining: 3 left" {
		t.Fatalf("draining node after a report %+v", n)
	}

	// A new start clears the rollout deadline and tells every instance.
	h.exec("UPDATE nodes SET stopping_until = now() + interval '30 seconds' WHERE name = 'n1'")
	ch, unsubscribe := h.d.Subscribe(db.ChanNode)
	defer unsubscribe()
	start2 := uuid.New()
	tel.StartId = start2.String()
	h.allowReconcile("n1")
	h.report("n1", serial, tel)
	expectPayload(t, ch, "n1")
	if n := h.node("n1"); n.StartID != start2 || !n.StoppingUntil.IsZero() {
		t.Fatalf("node after a new start %+v", n)
	}
	h.expectReconcile("n1")

	tel.StartId = "not-a-uuid"
	_, err = h.s.UplinkServer().ReportTelemetry(uplinkCtx("n1", serial), tel)
	wantCode(t, err, codes.InvalidArgument, "start_id")
	_, err = h.s.UplinkServer().ReportTelemetry(bg, tel)
	wantCode(t, err, codes.Unauthenticated, "")
}

func TestTelemetryInventoryHash(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	h.links.setConnected("n1", true)
	tel := &fleetv1.Telemetry{StartId: uuid.NewString(), InventoryHash: "aaa"}
	h.hooks.setHash("n1", "aaa")
	h.report("n1", serial, tel)
	h.expectReconcile("n1") // the new start
	mismatch := func() time.Time {
		tl, err := h.st.GetTelemetry(bg, "n1")
		if err != nil {
			t.Fatal(err)
		}
		return tl.InventoryMismatchSince
	}
	if !mismatch().IsZero() {
		t.Fatal("a mismatch while the hashes agree")
	}

	// A mismatch is recorded, and a reconcile requested only once it has
	// persisted for 10 s (two reports; ops in flight settle).
	h.allowReconcile("n1")
	tel.InventoryHash = "bbb"
	h.report("n1", serial, tel)
	since := mismatch()
	if since.IsZero() {
		t.Fatal("no mismatch recorded")
	}
	h.expectNoReconcile()
	h.report("n1", serial, tel)
	if !mismatch().Equal(since) {
		t.Fatal("the mismatch's start moved")
	}
	h.expectNoReconcile()
	h.exec("UPDATE node_telemetry SET inventory_mismatch_since = now() - interval '11 seconds' WHERE node = 'n1'")
	h.report("n1", serial, tel)
	h.expectReconcile("n1")

	// The last reconcile saw this hash of the node and could not remove the
	// difference (pods the registry never holds): not again until the
	// node's hash changes.
	h.exec("UPDATE node_telemetry SET reconciled_hash = 'bbb' WHERE node = 'n1'")
	h.allowReconcile("n1")
	h.report("n1", serial, tel)
	h.expectNoReconcile()
	tel.InventoryHash = "ccc"
	h.report("n1", serial, tel)
	h.expectReconcile("n1")

	// Agreement clears the mismatch.
	h.hooks.setHash("n1", "ccc")
	h.report("n1", serial, tel)
	if !mismatch().IsZero() {
		t.Fatal("the mismatch stays after the hashes agree")
	}

	// Not connected to the node: the instances that are get
	// ferun_reconcile, at most once per 5 s from this instance.
	h.links.setConnected("n1", false)
	ch, unsubscribe := h.d.Subscribe(db.ChanReconcile)
	defer unsubscribe()
	h.hooks.setHash("n1", "zzz")
	h.exec("UPDATE node_telemetry SET inventory_mismatch_since = now() - interval '11 seconds' WHERE node = 'n1'")
	h.report("n1", serial, tel)
	expectPayload(t, ch, "n1")
	h.report("n1", serial, tel)
	expectNoPayload(t, ch, "n1", 300*time.Millisecond)
	h.expectNoReconcile()

	// Without a reported hash nothing is compared.
	tel.InventoryHash = ""
	h.exec("UPDATE node_telemetry SET inventory_mismatch_since = NULL WHERE node = 'n1'")
	h.report("n1", serial, tel)
	if !mismatch().IsZero() {
		t.Fatal("a mismatch without a reported hash")
	}
}

func TestTelemetryInstances(t *testing.T) {
	h := newHarness(t)
	serial := h.joined("n1")
	for _, in := range []store.InstanceInfo{
		{ID: "b", NodeAddress: "10.0.0.2:7443"},
		{ID: "a", NodeAddress: "10.0.0.1:7443"},
		{ID: "c"},
		{ID: "d", NodeAddress: "10.0.0.4:7443"},
	} {
		if err := h.st.UpsertInstance(bg, in); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.st.SetInstanceStopping(bg, "b"); err != nil {
		t.Fatal(err)
	}
	h.exec("UPDATE instances SET last_seen = now() - interval '16 seconds' WHERE id = 'd'")
	resp := h.report("n1", serial, &fleetv1.Telemetry{StartId: uuid.NewString()})
	got := resp.GetInstances()
	if len(got) != 2 || got[0].GetId() != "a" || got[0].GetNodeAddress() != "10.0.0.1:7443" ||
		got[1].GetId() != "b" || got[1].GetNodeAddress() != "10.0.0.2:7443" {
		t.Fatalf("instances %v; want a and the stopping b, without c (no address) and d (not seen)", got)
	}
}
