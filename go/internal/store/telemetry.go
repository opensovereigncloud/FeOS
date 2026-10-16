// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Telemetry is a row of node_telemetry: a node's latest report, the hot
// row that placement reads but never locks.
type Telemetry struct {
	Node                 string
	LastSeen             time.Time // DB clock at receipt: the heartbeat
	Load1                float64
	MemoryAvailableBytes int64
	DiskAvailableBytes   int64
	PodsPending          int
	PodsRunning          int
	PodsSuspended        int
	PodsOther            int
	InventoryHash        string
	// InventoryMismatchSince is when the node's hash began to differ from
	// the registry's; zero while they agree.
	InventoryMismatchSince time.Time
	// ReconciledHash is the node's hash the last reconcile saw.
	ReconciledHash string
}

const telemetryCols = `node, last_seen, load1, memory_available_bytes, disk_available_bytes, pods_pending, pods_running,
	pods_suspended, pods_other, inventory_hash, inventory_mismatch_since, reconciled_hash`

func scanTelemetry(row pgx.Row) (Telemetry, error) {
	var t Telemetry
	var load1 float32
	var since *time.Time
	if err := row.Scan(&t.Node, &t.LastSeen, &load1, &t.MemoryAvailableBytes, &t.DiskAvailableBytes,
		&t.PodsPending, &t.PodsRunning, &t.PodsSuspended, &t.PodsOther, &t.InventoryHash, &since,
		&t.ReconciledHash); err != nil {
		return Telemetry{}, err
	}
	t.Load1 = float64(load1)
	t.InventoryMismatchSince = timeOf(since)
	return t, nil
}

// TelemetryReport is the dynamic part of a node's report.
type TelemetryReport struct {
	Load1                float64
	MemoryAvailableBytes int64
	DiskAvailableBytes   int64
	PodsPending          int
	PodsRunning          int
	PodsSuspended        int
	PodsOther            int
	InventoryHash        string
}

// UpsertTelemetry stores a node's report with last_seen = now(), the
// node's heartbeat, and returns the row.
func (s *Store) UpsertTelemetry(ctx context.Context, node string, r TelemetryReport) (Telemetry, error) {
	return scanTelemetry(s.q.QueryRow(ctx, `INSERT INTO node_telemetry AS t (node, last_seen, load1, memory_available_bytes,
			disk_available_bytes, pods_pending, pods_running, pods_suspended, pods_other, inventory_hash)
		VALUES ($1, now(), $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (node) DO UPDATE SET last_seen = now(), load1 = excluded.load1,
			memory_available_bytes = excluded.memory_available_bytes, disk_available_bytes = excluded.disk_available_bytes,
			pods_pending = excluded.pods_pending, pods_running = excluded.pods_running,
			pods_suspended = excluded.pods_suspended, pods_other = excluded.pods_other,
			inventory_hash = excluded.inventory_hash
		RETURNING `+telemetryCols, node, r.Load1, r.MemoryAvailableBytes, r.DiskAvailableBytes,
		r.PodsPending, r.PodsRunning, r.PodsSuspended, r.PodsOther, r.InventoryHash))
}

// GetTelemetry reads a node's latest report; ErrNoRow before the first.
func (s *Store) GetTelemetry(ctx context.Context, node string) (Telemetry, error) {
	t, err := scanTelemetry(s.q.QueryRow(ctx, "SELECT "+telemetryCols+" FROM node_telemetry WHERE node = $1", node))
	return t, noRow(err, ErrNoRow)
}

// ListTelemetry returns every node's latest report, by node.
func (s *Store) ListTelemetry(ctx context.Context) (map[string]Telemetry, error) {
	rows, err := s.q.Query(ctx, "SELECT "+telemetryCols+" FROM node_telemetry")
	if err != nil {
		return nil, err
	}
	ts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Telemetry, error) { return scanTelemetry(r) })
	if err != nil {
		return nil, err
	}
	out := make(map[string]Telemetry, len(ts))
	for _, t := range ts {
		out[t.Node] = t
	}
	return out, nil
}

// InventoryMismatch is the state of a node's hash comparison after
// SetInventoryMismatch.
type InventoryMismatch struct {
	// Since is when the hashes began to differ; zero when they agree.
	Since time.Time
	// For is how long they have differed, on the database clock.
	For time.Duration
	// ReconciledHash is the node's hash the last reconcile saw.
	ReconciledHash string
}

// SetInventoryMismatch records whether a node's inventory hash differs
// from the registry's: equal clears inventory_mismatch_since; different
// sets it unless set. It returns the persisted state.
func (s *Store) SetInventoryMismatch(ctx context.Context, node string, differs bool) (InventoryMismatch, error) {
	var m InventoryMismatch
	var since *time.Time
	var forUs *int64
	err := s.q.QueryRow(ctx, `UPDATE node_telemetry SET inventory_mismatch_since =
			CASE WHEN $2 THEN coalesce(inventory_mismatch_since, now()) ELSE NULL END
		WHERE node = $1
		RETURNING inventory_mismatch_since, (extract(epoch FROM now() - inventory_mismatch_since) * 1000000)::bigint,
			reconciled_hash`, node, differs).Scan(&since, &forUs, &m.ReconciledHash)
	if err != nil {
		return InventoryMismatch{}, noRow(err, ErrNoRow)
	}
	m.Since = timeOf(since)
	if forUs != nil {
		m.For = time.Duration(*forUs) * time.Microsecond
	}
	return m, nil
}
