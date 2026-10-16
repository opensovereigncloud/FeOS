// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/ironcore-dev/feos/go/gen/feos/fleet/v1"
	"github.com/ironcore-dev/feos/go/internal/activity"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// reportTelemetry is ReportTelemetry (§8), a node's heartbeat: the report
// is stored with last_seen on the database clock, the node's static facts
// when they changed, a joined or down node becomes ready, a new start of
// feosd is recorded, and the node's inventory hash is compared with the
// registry's. The answer names the interval and the instances to keep a
// node connection to.
func (s *Service) reportTelemetry(ctx context.Context, t *fleetv1.Telemetry) (*fleetv1.TelemetryResponse, error) {
	p, err := nodePeer(ctx)
	if err != nil {
		return nil, err
	}
	startID, err := uuid.Parse(t.GetStartId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "start_id %q is not a UUID", activity.Clean(t.GetStartId()))
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	node := p.Node
	report := store.TelemetryReport{
		Load1:                cleanFloat(t.GetLoad1()),
		MemoryAvailableBytes: clampInt64(t.GetMemoryAvailableBytes()),
		DiskAvailableBytes:   clampInt64(t.GetDiskAvailableBytes()),
		PodsPending:          clampInt32(uint64(t.GetPodsPending())),
		PodsRunning:          clampInt32(uint64(t.GetPodsRunning())),
		PodsSuspended:        clampInt32(uint64(t.GetPodsSuspended())),
		PodsOther:            clampInt32(uint64(t.GetPodsOther())),
		InventoryHash:        activity.Clean(t.GetInventoryHash()),
	}
	static := store.NodeStatic{
		Hostname:         activity.Clean(t.GetHostname()),
		Arch:             activity.Clean(t.GetArch()),
		CPUModel:         activity.Clean(t.GetCpuModel()),
		CPUCores:         clampInt32(uint64(t.GetCpuCores())),
		MemoryTotalBytes: clampInt64(t.GetMemoryTotalBytes()),
		DiskTotalBytes:   clampInt64(t.GetDiskTotalBytes()),
		Versions:         versionsJSON(t.GetVersions()),
		ObjectStore:      t.GetObjectStore(),
		ObjectStoreID:    activity.Clean(t.GetObjectStoreId()),
		PodPrefix:        activity.Clean(t.GetPodPrefix()),
	}

	// The registry's hash is read before the transaction: it is the
	// engine's query, and a comparison a moment off is settled by the
	// persistence rule below.
	registryHash, haveHash := "", false
	if report.InventoryHash != "" && s.hooks != nil {
		h, err := s.hooks.RegistryHash(ctx, node)
		if err != nil {
			s.rl.warn("registry-hash", "cannot compute the registry's inventory hash", "node", node, "err", err)
		} else {
			registryHash, haveHash = h, true
		}
	}

	var resp *fleetv1.TelemetryResponse
	var newStart, reconcile bool
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		resp, newStart, reconcile = nil, false, false
		// The node's row before its telemetry row: the order in which a
		// finished reconcile writes them (store.SetReconciled), so that
		// the two do not deadlock.
		if _, err := tx.UpdateNodeStatic(ctx, node, static); err != nil {
			return err
		}
		if _, err := tx.MarkNodeReady(ctx, node); err != nil {
			return err
		}
		var err error
		if newStart, err = tx.SetNodeStart(ctx, node, startID); err != nil {
			return err
		}
		if newStart {
			row := s.act.Row(activity.Event{
				Node: node, Kind: activity.KindNodeStart,
				Name:   fmt.Sprintf("feosd started (start %s)", startID),
				Detail: map[string]any{"start_id": startID.String()},
			})
			row.StartID = startID
			if err := tx.InsertActivity(ctx, row); err != nil {
				return err
			}
		}
		if _, err := tx.UpsertTelemetry(ctx, node, report); err != nil {
			return err
		}
		if haveHash {
			differs := report.InventoryHash != registryHash
			m, err := tx.SetInventoryMismatch(ctx, node, differs)
			if err != nil {
				return err
			}
			// A mismatch that persisted over two reports, and that the
			// last reconcile did not already see: a node's hash covers
			// pods the registry never holds, so a mismatch a reconcile
			// could not remove waits for the node's hash to change.
			reconcile = differs && m.For >= mismatchSettle && report.InventoryHash != m.ReconciledHash
		}
		resp = &fleetv1.TelemetryResponse{IntervalSeconds: telemetryInterval}
		return s.listInstances(ctx, tx, &resp.Instances)
	})
	if err != nil {
		s.rl.warn("telemetry", "cannot store a node's telemetry", "node", node, "err", err)
		return nil, rpcError(err, "store telemetry")
	}
	if newStart {
		s.log.Info("new start of feosd", "node", node, "start_id", startID)
	}
	if newStart || reconcile {
		s.requestReconcile(node)
	}
	return resp, nil
}

// versionsJSON renders what a node has installed as the JSON object of
// nodes.versions, its strings sanitised.
func versionsJSON(v *fleetv1.Versions) json.RawMessage {
	if v == nil {
		return json.RawMessage("{}")
	}
	clean := func(ss []string) []string {
		out := make([]string, len(ss))
		for i, s := range ss {
			out[i] = activity.Clean(s)
		}
		return out
	}
	b, err := json.Marshal(struct {
		Feosd       string   `json:"feosd"`
		Kernel      string   `json:"kernel"`
		Firecracker []string `json:"firecracker"`
		Guest       []string `json:"guest"`
		Diskd       string   `json:"diskd"`
	}{
		Feosd: activity.Clean(v.GetFeosd()), Kernel: activity.Clean(v.GetKernel()),
		Firecracker: clean(v.GetFirecracker()), Guest: clean(v.GetGuest()), Diskd: activity.Clean(v.GetDiskd()),
	})
	if err != nil {
		return json.RawMessage("{}")
	}
	return activity.CleanJSON(b)
}

// clampInt64 is v, or math.MaxInt64 when v exceeds it: a bigint column.
func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// clampInt32 is v, or math.MaxInt32 when v exceeds it: an integer column.
func clampInt32(v uint64) int {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(v)
}

// cleanFloat makes a load average storable as real: not a number, an
// infinity or a negative value becomes 0, a value beyond float32 is cut.
func cleanFloat(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return min(f, math.MaxFloat32)
}
