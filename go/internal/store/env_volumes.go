// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// EnvVolume is a row of env_volumes: an addon's volume, created once on a
// node and never resized.
type EnvVolume struct {
	EnvID    uuid.UUID
	Object   string // "addon/<name>"
	VolumeID uuid.UUID
	SizeMiB  int
	Node     string // the node that has it; "": none yet
	// Provisioned: created on Node; never CreateVolume again.
	Provisioned bool
	// Holder is the instance row that last held it; "": NULL.
	Holder podid.ID
	// AddonType is the addon type whose data the volume holds; fixed. A
	// target addon of another type never mounts it.
	AddonType string
	// DataVersion is the newest blueprint version that ran ready on the
	// volume: what its data was written by, the prev of the blueprint's
	// Validate for whatever mounts it next.
	DataVersion string
	Deleting    bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const envVolumeCols = "env_id, object, volume_id, size_mib, node, provisioned, holder, addon_type, data_version, deleting, created_at, updated_at"

func scanEnvVolume(row pgx.Row) (EnvVolume, error) {
	var v EnvVolume
	var holder *podid.ID
	if err := row.Scan(&v.EnvID, &v.Object, &v.VolumeID, &v.SizeMiB, &v.Node, &v.Provisioned, &holder, &v.AddonType, &v.DataVersion, &v.Deleting,
		&v.CreatedAt, &v.UpdatedAt); err != nil {
		return EnvVolume{}, err
	}
	v.Holder = idOf(holder)
	return v, nil
}

func collectEnvVolumes(rows pgx.Rows, err error) ([]EnvVolume, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvVolume, error) { return scanEnvVolume(r) })
}

// InsertEnvVolume inserts an addon's volume; an (environment, object) or a
// volume ID that exists is ErrExists, an unknown environment ErrNoRow.
func (s *Store) InsertEnvVolume(ctx context.Context, v EnvVolume) (EnvVolume, error) {
	out, err := scanEnvVolume(s.q.QueryRow(ctx, `INSERT INTO env_volumes (env_id, object, volume_id, size_mib, node,
			provisioned, holder, addon_type, data_version, deleting)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+envVolumeCols,
		v.EnvID, v.Object, v.VolumeID, v.SizeMiB, v.Node, v.Provisioned, nullID(v.Holder), v.AddonType, v.DataVersion,
		v.Deleting))
	if err != nil {
		return EnvVolume{}, insertError("insert volume of "+v.Object, err)
	}
	return out, nil
}

// GetEnvVolume reads an object's volume; ErrNoRow if there is none.
func (s *Store) GetEnvVolume(ctx context.Context, envID uuid.UUID, object string) (EnvVolume, error) {
	return s.LockEnvVolume(ctx, envID, object, NoLock)
}

// LockEnvVolume reads an object's volume and locks it with lock; ErrNoRow
// if there is none.
func (s *Store) LockEnvVolume(ctx context.Context, envID uuid.UUID, object string, lock RowLock) (EnvVolume, error) {
	v, err := scanEnvVolume(s.q.QueryRow(ctx, "SELECT "+envVolumeCols+` FROM env_volumes
		WHERE env_id = $1 AND object = $2 `+string(lock), envID, object))
	if err != nil {
		return EnvVolume{}, noRow(err, ErrNoRow)
	}
	return v, nil
}

// LockEnvVolumeByID reads the volume of a volume ID and locks it with
// lock; ErrNoRow if there is none.
func (s *Store) LockEnvVolumeByID(ctx context.Context, volumeID uuid.UUID, lock RowLock) (EnvVolume, error) {
	v, err := scanEnvVolume(s.q.QueryRow(ctx, "SELECT "+envVolumeCols+" FROM env_volumes WHERE volume_id = $1 "+
		string(lock), volumeID))
	if err != nil {
		return EnvVolume{}, noRow(err, ErrNoRow)
	}
	return v, nil
}

// ListEnvVolumes returns the environment's volumes by object.
func (s *Store) ListEnvVolumes(ctx context.Context, envID uuid.UUID) ([]EnvVolume, error) {
	return collectEnvVolumes(s.q.Query(ctx, "SELECT "+envVolumeCols+" FROM env_volumes WHERE env_id = $1 ORDER BY object", envID))
}

// DeletingEnvVolumes returns up to limit volumes marked deleting, of every
// environment, the least recently updated first.
func (s *Store) DeletingEnvVolumes(ctx context.Context, limit int) ([]EnvVolume, error) {
	return collectEnvVolumes(s.q.Query(ctx, "SELECT "+envVolumeCols+` FROM env_volumes
		WHERE deleting ORDER BY updated_at, volume_id LIMIT $1`, limit))
}

// UpdateEnvVolume writes the node, provisioned, holder, data version and
// deleting in
// which after differs from before (updated_at = now()); the other columns
// are refused. When nothing differs, before is returned unwritten.
func (s *Store) UpdateEnvVolume(ctx context.Context, before, after EnvVolume) (EnvVolume, error) {
	what := fmt.Sprintf("volume of %s of environment %s", before.Object, before.EnvID)
	if err := fixed(what, map[string]bool{"env_id": before.EnvID != after.EnvID, "object": before.Object != after.Object,
		"volume_id": before.VolumeID != after.VolumeID, "size_mib": before.SizeMiB != after.SizeMiB,
		"addon_type": before.AddonType != after.AddonType, "created_at": !before.CreatedAt.Equal(after.CreatedAt)}); err != nil {
		return EnvVolume{}, err
	}
	u := newUpdates(before.EnvID, before.Object)
	u.set("node", before.Node != after.Node, after.Node)
	u.set("provisioned", before.Provisioned != after.Provisioned, after.Provisioned)
	u.set("holder", before.Holder != after.Holder, nullID(after.Holder))
	u.set("data_version", before.DataVersion != after.DataVersion, after.DataVersion)
	u.set("deleting", before.Deleting != after.Deleting, after.Deleting)
	if u.empty() {
		return before, nil
	}
	v, err := scanEnvVolume(s.q.QueryRow(ctx, "UPDATE env_volumes SET "+u.clause(true)+
		" WHERE env_id = $1 AND object = $2 RETURNING "+envVolumeCols, u.args...))
	if err != nil {
		return EnvVolume{}, fmt.Errorf("update %s: %w", what, noRow(err, ErrNoRow))
	}
	return v, nil
}

// DeleteEnvVolume removes an object's volume row (once it is gone from its
// node) and reports whether it was there.
func (s *Store) DeleteEnvVolume(ctx context.Context, envID uuid.UUID, object string) (bool, error) {
	ct, err := s.q.Exec(ctx, "DELETE FROM env_volumes WHERE env_id = $1 AND object = $2", envID, object)
	return ct.RowsAffected() > 0, err
}
