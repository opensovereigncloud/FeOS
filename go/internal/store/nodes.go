// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Node is a row of nodes. A node is never deleted: removal is a status.
type Node struct {
	Name              string
	Status            string // model.Node*
	StatusDescription string
	DownSince         time.Time
	RemovedAt         time.Time
	Draining          bool
	DrainStartedAt    time.Time

	Slots            int
	MemHeadroomBytes int64
	CPUOvercommit    float64 // floor(CPUCores × this) = 0: CPU not gated

	Hostname         string
	Arch             string
	CPUModel         string
	CPUCores         int
	MemoryTotalBytes int64
	DiskTotalBytes   int64
	Versions         json.RawMessage
	ObjectStore      bool
	ObjectStoreID    string
	PodPrefix        string

	StartID          uuid.UUID // feosd's current start; uuid.Nil before the first report
	StoppingUntil    time.Time // the rollout grace after a NodeEvent "stopping"
	CurrentSerial    string
	IdentityForkedAt time.Time

	JoinedAt             time.Time
	ReconciledAt         time.Time
	ReconcileRequestedAt time.Time
}

// Forked reports whether the node's identity forked.
func (n *Node) Forked() bool { return !n.IdentityForkedAt.IsZero() }

// VCPUCapacity is placement's CPU capacity of the node, floor(cores ×
// overcommit); 0 means CPU is not gated.
func (n *Node) VCPUCapacity() int64 {
	return int64(math.Floor(float64(n.CPUCores) * n.CPUOvercommit))
}

// MemoryCapacityMiB is the memory placement books against: the total less
// the headroom, never below 0.
func (n *Node) MemoryCapacityMiB() int64 {
	return max(n.MemoryTotalBytes-n.MemHeadroomBytes, 0) / (1 << 20)
}

func nodeColumns(p string) string {
	cols := []string{"name", "status", "status_description", "down_since", "removed_at", "draining", "drain_started_at",
		"slots", "mem_headroom_bytes", "cpu_overcommit", "hostname", "arch", "cpu_model", "cpu_cores",
		"memory_total_bytes", "disk_total_bytes", "versions", "object_store", "object_store_id", "pod_prefix",
		"start_id", "stopping_until", "current_serial", "identity_forked_at", "joined_at", "reconciled_at",
		"reconcile_requested_at"}
	if p != "" {
		for i, c := range cols {
			cols[i] = p + "." + c
		}
	}
	return strings.Join(cols, ", ")
}

var nodeCols = nodeColumns("")

// nodeScanTargets returns the scan destinations of nodeColumns for n and a
// function that moves the nullable ones into n after the scan.
func nodeScanTargets(n *Node) ([]any, func()) {
	var downSince, removedAt, drainStarted, stoppingUntil, forkedAt, reconciledAt, requestedAt *time.Time
	var startID *uuid.UUID
	var overcommit float32
	targets := []any{&n.Name, &n.Status, &n.StatusDescription, &downSince, &removedAt, &n.Draining, &drainStarted,
		&n.Slots, &n.MemHeadroomBytes, &overcommit, &n.Hostname, &n.Arch, &n.CPUModel, &n.CPUCores,
		&n.MemoryTotalBytes, &n.DiskTotalBytes, &n.Versions, &n.ObjectStore, &n.ObjectStoreID, &n.PodPrefix,
		&startID, &stoppingUntil, &n.CurrentSerial, &forkedAt, &n.JoinedAt, &reconciledAt, &requestedAt}
	return targets, func() {
		n.DownSince, n.RemovedAt, n.DrainStartedAt = timeOf(downSince), timeOf(removedAt), timeOf(drainStarted)
		n.StoppingUntil, n.IdentityForkedAt = timeOf(stoppingUntil), timeOf(forkedAt)
		n.ReconciledAt, n.ReconcileRequestedAt = timeOf(reconciledAt), timeOf(requestedAt)
		n.StartID = uuidOf(startID)
		n.CPUOvercommit = float64(overcommit)
	}
}

func scanNode(row pgx.Row) (Node, error) {
	var n Node
	targets, finish := nodeScanTargets(&n)
	if err := row.Scan(targets...); err != nil {
		return Node{}, err
	}
	finish()
	return n, nil
}

// GetNode reads a node; ErrNoRow if there is none.
func (s *Store) GetNode(ctx context.Context, name string) (Node, error) {
	return s.LockNode(ctx, name, NoLock)
}

// LockNode reads a node and locks its row; ErrNoRow if there is none.
func (s *Store) LockNode(ctx context.Context, name string, lock RowLock) (Node, error) {
	n, err := scanNode(s.q.QueryRow(ctx, "SELECT "+nodeCols+" FROM nodes WHERE name = $1 "+string(lock), name))
	return n, noRow(err, ErrNoRow)
}

// ListNodes returns every node, removed ones included, ordered by name.
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.q.Query(ctx, "SELECT "+nodeCols+" FROM nodes ORDER BY name")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Node, error) { return scanNode(r) })
}

// InsertNode adds a node named name with status joined unless the name
// exists. It reports whether it did: an unpinned join token never takes
// over an existing name, whatever the row's status.
func (s *Store) InsertNode(ctx context.Context, name string) (bool, error) {
	tag, err := s.q.Exec(ctx, "INSERT INTO nodes (name, status) VALUES ($1, 'joined') ON CONFLICT DO NOTHING", name)
	return tag.RowsAffected() > 0, err
}

// RejoinNode resets a node for a join with a token pinned to its name: the
// row (inserted if missing, then locked) becomes joined, not removed, not
// forked, without a current serial or description (draining stays); every
// earlier certificate of the node is revoked ("rejoined"); ferun_node is
// sent, so that the old connections close everywhere.
func (s *Store) RejoinNode(ctx context.Context, name string) error {
	if _, err := s.InsertNode(ctx, name); err != nil {
		return err
	}
	if _, err := s.LockNode(ctx, name, ForUpdate); err != nil {
		return err
	}
	if _, err := s.q.Exec(ctx, `UPDATE nodes SET status = 'joined', removed_at = NULL, identity_forked_at = NULL,
		current_serial = '', status_description = '' WHERE name = $1`, name); err != nil {
		return err
	}
	if _, err := s.RevokeNodeCerts(ctx, name, RevokedRejoined, ""); err != nil {
		return err
	}
	return s.Notify(ctx, chanNode, name)
}

// NodeStatic is what a node reports about itself that rarely changes.
type NodeStatic struct {
	Hostname         string
	Arch             string
	CPUModel         string
	CPUCores         int
	MemoryTotalBytes int64
	DiskTotalBytes   int64
	Versions         json.RawMessage // a JSON object
	ObjectStore      bool
	ObjectStoreID    string
	PodPrefix        string
}

// UpdateNodeStatic writes a node's static columns, only when one differs,
// so that the hot telemetry path leaves the node row alone. It reports
// whether it wrote.
func (s *Store) UpdateNodeStatic(ctx context.Context, name string, st NodeStatic) (bool, error) {
	versions := st.Versions
	if len(versions) == 0 {
		versions = json.RawMessage("{}")
	}
	tag, err := s.q.Exec(ctx, `UPDATE nodes SET hostname = $2, arch = $3, cpu_model = $4, cpu_cores = $5,
			memory_total_bytes = $6, disk_total_bytes = $7, versions = $8, object_store = $9,
			object_store_id = $10, pod_prefix = $11
		WHERE name = $1 AND (hostname, arch, cpu_model, cpu_cores, memory_total_bytes, disk_total_bytes, versions,
			object_store, object_store_id, pod_prefix)
			IS DISTINCT FROM ($2, $3, $4, $5::integer, $6::bigint, $7::bigint, $8::jsonb, $9::boolean, $10, $11)`,
		name, st.Hostname, st.Arch, st.CPUModel, st.CPUCores, st.MemoryTotalBytes, st.DiskTotalBytes,
		string(versions), st.ObjectStore, st.ObjectStoreID, st.PodPrefix)
	return tag.RowsAffected() > 0, err
}

// MarkNodeReady moves a joined or down node to ready, clears down_since,
// and clears the status description unless the node is draining. It
// reports whether the status changed.
func (s *Store) MarkNodeReady(ctx context.Context, name string) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE nodes SET status = 'ready', down_since = NULL,
		status_description = CASE WHEN draining THEN status_description ELSE '' END
		WHERE name = $1 AND status IN ('joined', 'down')`, name)
	return tag.RowsAffected() > 0, err
}

// SetNodeStart records a node's current feosd start when it is new, and
// clears stopping_until (the rollout it announced is over); ferun_node is
// sent then. It reports whether the start was new.
func (s *Store) SetNodeStart(ctx context.Context, name string, startID uuid.UUID) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE nodes SET start_id = $2, stopping_until = NULL
		WHERE name = $1 AND start_id IS DISTINCT FROM $2`, name, startID)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	return true, s.Notify(ctx, chanNode, name)
}

// SetNodeStopping sets stopping_until = now() + grace after a NodeEvent
// "stopping", sends ferun_node, and returns the deadline.
func (s *Store) SetNodeStopping(ctx context.Context, name string, grace time.Duration) (time.Time, error) {
	var until time.Time
	err := s.q.QueryRow(ctx, `UPDATE nodes SET stopping_until = now() + $2::interval WHERE name = $1
		RETURNING stopping_until`, name, interval(grace)).Scan(&until)
	if err != nil {
		return time.Time{}, noRow(err, ErrNoRow)
	}
	return until, s.Notify(ctx, chanNode, name)
}

// StoppingUntil returns every node's stopping_until that lies ahead.
func (s *Store) StoppingUntil(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.q.Query(ctx, "SELECT name, stopping_until FROM nodes WHERE stopping_until > now()")
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	var name string
	var until time.Time
	_, err = pgx.ForEachRow(rows, []any{&name, &until}, func() error {
		out[name] = until
		return nil
	})
	return out, err
}

// ExpireNodes marks down every joined or ready node whose last telemetry
// (or, without any, its join) is older than ttl: status down, down_since =
// now(), "heartbeat expired". It returns their names.
func (s *Store) ExpireNodes(ctx context.Context, ttl time.Duration) ([]string, error) {
	rows, err := s.q.Query(ctx, `UPDATE nodes n SET status = 'down', down_since = now(),
			status_description = 'heartbeat expired'
		WHERE n.status IN ('joined', 'ready')
			AND coalesce((SELECT t.last_seen FROM node_telemetry t WHERE t.node = n.name), n.joined_at) < now() - $1::interval
		RETURNING n.name`, interval(ttl))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ClaimReconcile claims a node's inventory reconcile for the caller: only
// for a joined or ready node, and only when no claim was made within every
// (30 s). It reports whether the caller got it.
func (s *Store) ClaimReconcile(ctx context.Context, name string, every time.Duration) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE nodes SET reconcile_requested_at = now() WHERE name = $1
		AND status IN ('joined', 'ready')
		AND (reconcile_requested_at IS NULL OR reconcile_requested_at < now() - $2::interval)`, name, interval(every))
	return tag.RowsAffected() > 0, err
}

// SetReconciled records a finished reconcile: nodes.reconciled_at = now()
// and node_telemetry.reconciled_hash = the node's hash the reconcile
// started from.
func (s *Store) SetReconciled(ctx context.Context, name, nodeHash string) error {
	if _, err := s.q.Exec(ctx, "UPDATE nodes SET reconciled_at = now() WHERE name = $1", name); err != nil {
		return err
	}
	_, err := s.q.Exec(ctx, "UPDATE node_telemetry SET reconciled_hash = $2 WHERE node = $1", name, nodeHash)
	return err
}

// SetNodeDraining sets or clears a node's draining flag and
// drain_started_at (set at the first drain, cleared by an undrain); the
// caller holds the node row FOR UPDATE. A change sends ferun_node. It
// returns the node as written.
func (s *Store) SetNodeDraining(ctx context.Context, name string, draining bool) (Node, error) {
	n, err := scanNode(s.q.QueryRow(ctx, `UPDATE nodes SET draining = $2,
			drain_started_at = CASE WHEN $2 THEN coalesce(drain_started_at, now()) ELSE NULL END
		WHERE name = $1 RETURNING `+nodeCols, name, draining))
	if err != nil {
		return Node{}, noRow(err, ErrNoRow)
	}
	return n, s.Notify(ctx, chanNode, name)
}

// NodeSettings is an operator's change of a node's capacity settings; nil
// fields stay.
type NodeSettings struct {
	Slots            *int
	MemHeadroomBytes *int64
	CPUOvercommit    *float64
}

// UpdateNodeSettings writes the given settings and returns the node.
func (s *Store) UpdateNodeSettings(ctx context.Context, name string, ns NodeSettings) (Node, error) {
	n, err := scanNode(s.q.QueryRow(ctx, `UPDATE nodes SET slots = coalesce($2, slots),
			mem_headroom_bytes = coalesce($3, mem_headroom_bytes), cpu_overcommit = coalesce($4::real, cpu_overcommit)
		WHERE name = $1 RETURNING `+nodeCols, name, ns.Slots, ns.MemHeadroomBytes, ns.CPUOvercommit))
	return n, noRow(err, ErrNoRow)
}

// SetNodeDescription writes a node's status description.
func (s *Store) SetNodeDescription(ctx context.Context, name, description string) error {
	_, err := s.q.Exec(ctx, "UPDATE nodes SET status_description = $2 WHERE name = $1 AND status_description <> $2", name, description)
	return err
}

// RemoveNode marks a node removed (status, removed_at), revokes every
// certificate of it ("removed") and sends ferun_node. The caller holds the
// row FOR UPDATE and has checked the node's sandboxes.
func (s *Store) RemoveNode(ctx context.Context, name string) (Node, error) {
	n, err := scanNode(s.q.QueryRow(ctx, `UPDATE nodes SET status = 'removed', removed_at = coalesce(removed_at, now())
		WHERE name = $1 RETURNING `+nodeCols, name))
	if err != nil {
		return Node{}, noRow(err, ErrNoRow)
	}
	if _, err := s.RevokeNodeCerts(ctx, name, RevokedRemoved, ""); err != nil {
		return Node{}, err
	}
	return n, s.Notify(ctx, chanNode, name)
}

// ForkNode records that a node's identity forked: identity_forked_at =
// now(), the description, every certificate revoked ("forked"), and
// ferun_node sent. The caller holds the row FOR UPDATE.
func (s *Store) ForkNode(ctx context.Context, name, description string) error {
	tag, err := s.q.Exec(ctx, `UPDATE nodes SET identity_forked_at = coalesce(identity_forked_at, now()),
		status_description = $2 WHERE name = $1`, name, description)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoRow
	}
	if _, err := s.RevokeNodeCerts(ctx, name, RevokedForked, ""); err != nil {
		return err
	}
	return s.Notify(ctx, chanNode, name)
}
