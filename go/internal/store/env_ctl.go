// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The environment controller's queries (internal/env/ctl) beyond the
// basic reads and writes of the env_*.go files.

// attachedUsable is the condition under which an attached service a of
// environment e may use the sandbox s it names (§13.6): s is a sandbox of
// e's owner and not an instance row. A sandbox of the owner deleted and
// created again under the same ID (FeCode replaces a lost workspace
// sandbox so) is a new incarnation the service starts in anew; one of
// another tenant under that ID is never used.
const attachedUsable = `s.owner = e.owner AND s.environment_id IS NULL`

// EnvCtlMembers returns the members of an environment's security group:
// the addresses of its instance rows (any state, until the row is gone)
// and of the sandboxes its attached services may use (attachedUsable),
// without the empty ones, distinct and sorted. The caller keeps the IPv6
// ones.
func (s *Store) EnvCtlMembers(ctx context.Context, envID uuid.UUID) ([]string, error) {
	rows, err := s.q.Query(ctx, `SELECT address FROM sandboxes WHERE environment_id = $1 AND address <> ''
		UNION SELECT s.address FROM env_attached a JOIN environments e ON e.id = a.env_id
			JOIN sandboxes s ON s.id = a.sandbox_id
			WHERE a.env_id = $1 AND s.address <> '' AND `+attachedUsable+`
		ORDER BY 1`, envID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// EnvCtlSandboxes returns the instance rows of an environment (rows of
// sandboxes with its environment_id), also those without an
// env_instances row, by id.
func (s *Store) EnvCtlSandboxes(ctx context.Context, envID uuid.UUID) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxCols+" FROM sandboxes WHERE environment_id = $1 ORDER BY id", envID))
}

// EnvCtlCloseIncidents closes the environment's open incidents of one of
// conditions whose release is below below, and returns how many.
func (s *Store) EnvCtlCloseIncidents(ctx context.Context, envID uuid.UUID, below int, conditions ...string) (int64, error) {
	ct, err := s.q.Exec(ctx, `UPDATE env_incidents SET closed_at = now()
		WHERE env_id = $1 AND release < $2 AND condition = ANY($3) AND closed_at IS NULL`, envID, below, nonNil(conditions))
	return ct.RowsAffected(), err
}

// EnvCtlCloseObjectIncidents closes the open incidents of condition of an
// object of the environment ("<kind>/<name>"), of any release, and
// returns how many.
func (s *Store) EnvCtlCloseObjectIncidents(ctx context.Context, envID uuid.UUID, object, condition string) (int64, error) {
	ct, err := s.q.Exec(ctx, `UPDATE env_incidents SET closed_at = now()
		WHERE env_id = $1 AND object = $2 AND condition = $3 AND closed_at IS NULL`, envID, object, condition)
	return ct.RowsAffected(), err
}

// EnvCtlSetPin writes a row's pin_node (not a lifecycle column: its gen
// stays) and reports whether the row was there.
func (s *Store) EnvCtlSetPin(ctx context.Context, id uuid.UUID, node string) (bool, error) {
	ct, err := s.q.Exec(ctx, `UPDATE sandboxes SET pin_node = $2, updated_at = now() WHERE id = $1 AND pin_node <> $2`,
		id, node)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// EnvCtlSetIdle writes a row's idle_after_s (not a lifecycle column) and
// reports whether it changed.
func (s *Store) EnvCtlSetIdle(ctx context.Context, id uuid.UUID, idleAfterS int) (bool, error) {
	ct, err := s.q.Exec(ctx, `UPDATE sandboxes SET idle_after_s = $2, updated_at = now()
		WHERE id = $1 AND idle_after_s IS DISTINCT FROM $2`, id, idleAfterS)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// EnvCtlMarkVolumesDeleting marks every volume of the environment
// deleting and returns how many it marked.
func (s *Store) EnvCtlMarkVolumesDeleting(ctx context.Context, envID uuid.UUID) (int64, error) {
	ct, err := s.q.Exec(ctx, `UPDATE env_volumes SET deleting = true, updated_at = now() WHERE env_id = $1 AND NOT deleting`,
		envID)
	return ct.RowsAffected(), err
}

// EnvCtlVolumesByID returns the volumes of the ids that have a row, by
// volume ID.
func (s *Store) EnvCtlVolumesByID(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]EnvVolume, error) {
	out := map[uuid.UUID]EnvVolume{}
	if len(ids) == 0 {
		return out, nil
	}
	vols, err := collectEnvVolumes(s.q.Query(ctx, "SELECT "+envVolumeCols+" FROM env_volumes WHERE volume_id = ANY($1)", ids))
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		out[v.VolumeID] = v
	}
	return out, nil
}

// EnvCtlTouchVolume sets a volume's updated_at to now(), so that one that
// waits goes to the end of DeletingEnvVolumes's order; ErrNoRow when it is
// gone.
func (s *Store) EnvCtlTouchVolume(ctx context.Context, envID uuid.UUID, object string) error {
	ct, err := s.q.Exec(ctx, `UPDATE env_volumes SET updated_at = now() WHERE env_id = $1 AND object = $2`, envID, object)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNoRow
	}
	return nil
}

// EnvCtlStarved returns the instance rows of the environment that have
// been pending without capacity (a description beginning "no capacity")
// for at least after, by the database's clock: their ids and
// descriptions.
func (s *Store) EnvCtlStarved(ctx context.Context, envID uuid.UUID, after time.Duration) (map[uuid.UUID]string, error) {
	rows, err := s.q.Query(ctx, `SELECT id, status_description FROM sandboxes
		WHERE environment_id = $1 AND client_state = 'pending' AND op = '' AND status_description LIKE 'no capacity%'
			AND coalesce(pending_since, created_at) <= now() - make_interval(secs => $2)`, envID, after.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var desc string
		if err := rows.Scan(&id, &desc); err != nil {
			return nil, err
		}
		out[id] = desc
	}
	return out, rows.Err()
}

// EnvCtlRunningJobRuns returns the environment's runs that are running,
// oldest first.
func (s *Store) EnvCtlRunningJobRuns(ctx context.Context, envID uuid.UUID) ([]EnvJobRun, error) {
	return collectJobRuns(s.q.Query(ctx, "SELECT "+jobRunCols+` FROM env_job_runs
		WHERE env_id = $1 AND phase = 'running' ORDER BY id`, envID))
}

// EnvCtlWithAttached returns the IDs of the environments whose attached
// services run in the sandbox and may use it (attachedUsable), sorted.
func (s *Store) EnvCtlWithAttached(ctx context.Context, sandboxID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, `SELECT DISTINCT a.env_id FROM env_attached a JOIN environments e ON e.id = a.env_id
			JOIN sandboxes s ON s.id = a.sandbox_id
		WHERE a.sandbox_id = $1 AND `+attachedUsable+` ORDER BY a.env_id`, sandboxID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// EnvCtlTemplatedOnNode returns the rows with a pod template whose pod is
// on node, or is being brought there by an op: instance rows and
// attachable sandboxes, by id.
func (s *Store) EnvCtlTemplatedOnNode(ctx context.Context, node string) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxCols+` FROM sandboxes
		WHERE pod_template IS NOT NULL AND (node = $1 OR op_target = $1 OR lost_from = $1) ORDER BY id`, node))
}

// EnvCtlExisting returns which of the ids are environments (exists) and
// which are sandboxes (sandboxes).
func (s *Store) EnvCtlExisting(ctx context.Context, envs, sandboxes []uuid.UUID) (map[uuid.UUID]bool, map[uuid.UUID]bool, error) {
	collect := func(sql string, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
		out := map[uuid.UUID]bool{}
		if len(ids) == 0 {
			return out, nil
		}
		rows, err := s.q.Query(ctx, sql, ids)
		if err != nil {
			return nil, err
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return nil, err
		}
		for _, id := range found {
			out[id] = true
		}
		return out, nil
	}
	e, err := collect("SELECT id FROM environments WHERE id = ANY($1)", envs)
	if err != nil {
		return nil, nil, err
	}
	sb, err := collect("SELECT id FROM sandboxes WHERE id = ANY($1)", sandboxes)
	if err != nil {
		return nil, nil, err
	}
	return e, sb, nil
}
