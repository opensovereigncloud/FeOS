// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// InstanceTTL is how long an instance counts as alive after its last
// heartbeat.
const InstanceTTL = 15 * time.Second

// instanceLive is the condition under which the instances row a is live:
// seen within the TTL and not stopping. A stopping or dead instance's ops
// may be taken over.
func instanceLive(a string) string {
	return a + ".last_seen > now() - interval '15 seconds' AND " + a + ".stopping IS NULL"
}

// Instance is a row of instances.
type Instance struct {
	ID          string
	Address     string // the front-door listen address, informational
	NodeAddress string // --node-advertise-address; "" = not listed to nodes
	Version     string
	StartedAt   time.Time
	LastSeen    time.Time
	Stopping    time.Time // zero unless the instance is stopping
	// Live: seen within the TTL and not stopping.
	Live bool
}

// InstanceInfo is what an instance writes about itself.
type InstanceInfo struct {
	ID, Address, NodeAddress, Version string
}

// UpsertInstance writes the instance's heartbeat: its row with last_seen =
// now() (started_at and stopping are kept).
func (s *Store) UpsertInstance(ctx context.Context, in InstanceInfo) error {
	_, err := s.q.Exec(ctx, `INSERT INTO instances (id, address, node_address, version) VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET address = excluded.address, node_address = excluded.node_address,
			version = excluded.version, last_seen = now()`, in.ID, in.Address, in.NodeAddress, in.Version)
	return err
}

// SetInstanceStopping marks the instance stopping from now on (SIGTERM).
func (s *Store) SetInstanceStopping(ctx context.Context, id string) error {
	_, err := s.q.Exec(ctx, "UPDATE instances SET stopping = coalesce(stopping, now()) WHERE id = $1", id)
	return err
}

// DeleteInstance removes the instance's row (the last step of its
// shutdown).
func (s *Store) DeleteInstance(ctx context.Context, id string) error {
	_, err := s.q.Exec(ctx, "DELETE FROM instances WHERE id = $1", id)
	return err
}

// InstanceLive reports whether the instance is live: seen within the TTL
// and not stopping.
func (s *Store) InstanceLive(ctx context.Context, id string) (bool, error) {
	var live bool
	err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM instances i WHERE i.id = $1 AND "+instanceLive("i")+")", id).Scan(&live)
	return live, err
}

// GetInstance reads an instance; ErrNoRow if there is none.
func (s *Store) GetInstance(ctx context.Context, id string) (Instance, error) {
	in, err := scanInstance(s.q.QueryRow(ctx, "SELECT "+instanceCols+" FROM instances i WHERE i.id = $1", id))
	return in, noRow(err, ErrNoRow)
}

const instanceCols = "i.id, i.address, i.node_address, i.version, i.started_at, i.last_seen, i.stopping, " +
	"(i.last_seen > now() - interval '15 seconds' AND i.stopping IS NULL)"

func scanInstance(row pgx.Row) (Instance, error) {
	var in Instance
	var stopping *time.Time
	if err := row.Scan(&in.ID, &in.Address, &in.NodeAddress, &in.Version, &in.StartedAt, &in.LastSeen, &stopping, &in.Live); err != nil {
		return Instance{}, err
	}
	in.Stopping = timeOf(stopping)
	return in, nil
}

// ListInstances returns every instance row, ordered by id.
func (s *Store) ListInstances(ctx context.Context) ([]Instance, error) {
	rows, err := s.q.Query(ctx, "SELECT "+instanceCols+" FROM instances i ORDER BY i.id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Instance, error) { return scanInstance(r) })
}

// InstanceAddress is an instance as nodes see it.
type InstanceAddress struct {
	ID, NodeAddress string
}

// InstancesForNodes lists the instances nodes keep a node connection to:
// a node address and seen within the TTL, stopping ones included (they
// serve until they delete their row), ordered by id.
func (s *Store) InstancesForNodes(ctx context.Context) ([]InstanceAddress, error) {
	rows, err := s.q.Query(ctx, `SELECT id, node_address FROM instances
		WHERE node_address <> '' AND last_seen > now() - interval '15 seconds' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (InstanceAddress, error) {
		var a InstanceAddress
		err := r.Scan(&a.ID, &a.NodeAddress)
		return a, err
	})
}

// DeleteStaleInstances removes the rows of instances not seen for longer
// than olderThan (the leader: an hour). It returns how many.
func (s *Store) DeleteStaleInstances(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM instances WHERE last_seen < now() - $1::interval", interval(olderThan))
	return tag.RowsAffected(), err
}
