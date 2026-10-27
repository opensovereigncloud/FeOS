// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// InstanceTTL is how long an instance counts as alive after its last
// heartbeat.
const InstanceTTL = 15 * time.Second

// instanceFresh is the condition under which the instances row a was
// written within the TTL: a process runs under its ID, stopping or not.
func instanceFresh(a string) string {
	return a + ".last_seen > now() - interval '15 seconds'"
}

// instanceLive is the condition under which the instances row a is live:
// seen within the TTL and not stopping. A stopping or dead instance's ops
// may be taken over.
func instanceLive(a string) string {
	return instanceFresh(a) + " AND " + a + ".stopping IS NULL"
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
	// Fresh: seen within the TTL, stopping or not. A stopping instance
	// that is fresh still serves its requests and ops.
	Fresh bool
	// Live: fresh and not stopping.
	Live bool
}

// InstanceInfo is what an instance writes about itself.
type InstanceInfo struct {
	ID, Address, NodeAddress, Version string
}

// InstanceInUseError is ClaimInstance's refusal: the row of the ID was
// written within the TTL.
type InstanceInUseError struct {
	ID       string
	LastSeen time.Time     // the row's last write
	Age      time.Duration // its age, on the database clock
	Stopping bool          // the row says stopping
}

func (e *InstanceInUseError) Error() string {
	state := "running"
	if e.Stopping {
		state = "stopping"
	}
	return fmt.Sprintf("instance id %s is in use: a %s instance wrote its row %v ago", e.ID, state,
		e.Age.Round(time.Millisecond))
}

// ClaimInstance begins a run of the instance in.ID, in one transaction: it
// writes the row anew (started_at = last_seen = now(), not stopping) and
// releases the ops the ID still owns, which no process runs any more: they
// are an earlier run's that ended without its stop's release. The release
// is ReleaseOpsOf's, with the description "op <op> of instance <id> was
// released: the instance started again"; the released sandboxes are
// notified and returned. A row written within the TTL refuses the claim
// with *InstanceInUseError: a process still runs under the ID, or stops
// (its last steps release the ops the ID owns and delete the row), or one
// ended without deleting it less than the TTL ago.
func (s *Store) ClaimInstance(ctx context.Context, in InstanceInfo) ([]podid.ID, error) {
	var released []podid.ID
	err := s.InTx(ctx, func(tx *Store) error {
		released = nil
		var claimed bool
		err := tx.q.QueryRow(ctx, `INSERT INTO instances (id, address, node_address, version) VALUES ($1, $2, $3, $4)
			ON CONFLICT (id) DO UPDATE SET address = excluded.address, node_address = excluded.node_address,
				version = excluded.version, started_at = now(), last_seen = now(), stopping = NULL
			WHERE NOT (`+instanceFresh("instances")+`)
			RETURNING true`, in.ID, in.Address, in.NodeAddress, in.Version).Scan(&claimed)
		if errors.Is(err, pgx.ErrNoRows) {
			e := &InstanceInUseError{ID: in.ID}
			var ageUs int64
			if err := tx.q.QueryRow(ctx, `SELECT last_seen, (extract(epoch FROM now() - last_seen) * 1000000)::bigint,
				stopping IS NOT NULL FROM instances WHERE id = $1`, in.ID).Scan(&e.LastSeen, &ageUs, &e.Stopping); err != nil {
				return err
			}
			e.Age = time.Duration(ageUs) * time.Microsecond
			return e
		}
		if err != nil {
			return err
		}
		// The release of a stop's step 4 (ReleaseOpsOf), with the reason
		// that holds here: no stop released these ops.
		rows, err := tx.q.Query(ctx, `UPDATE sandboxes SET op = '', op_id = NULL, op_owner = '', op_deadline = NULL,
				op_target = '', op_from_state = '', client_state = 'unknown',
				status_description = 'op ' || op || ' of instance ' || $1 || ' was released: the instance started again',
				gen = gen + 1, updated_at = now()
			WHERE op_owner = $1 AND op <> '' RETURNING id`, in.ID)
		released, err = tx.notifyReturned(ctx, rows, err)
		return err
	})
	return released, err
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

var instanceCols = "i.id, i.address, i.node_address, i.version, i.started_at, i.last_seen, i.stopping, " +
	"(" + instanceFresh("i") + "), (" + instanceLive("i") + ")"

func scanInstance(row pgx.Row) (Instance, error) {
	var in Instance
	var stopping *time.Time
	if err := row.Scan(&in.ID, &in.Address, &in.NodeAddress, &in.Version, &in.StartedAt, &in.LastSeen, &stopping,
		&in.Fresh, &in.Live); err != nil {
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
	rows, err := s.q.Query(ctx, `SELECT i.id, i.node_address FROM instances i
		WHERE i.node_address <> '' AND `+instanceFresh("i")+` ORDER BY i.id`)
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
