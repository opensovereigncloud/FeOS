// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	volumev1 "github.com/ironcore-dev/feos/go/gen/feos/volume/v1"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// volumesBatch is how many deleting volumes one env-volumes pass takes.
const volumesBatch = 64

// volumesLoop is env-volumes: the volumes marked deleting go from their
// nodes once no pod holds them (FAILED_PRECONDITION: later; NOT_FOUND:
// done), then their rows. A volume never created on a node, or of a node
// that is gone or removed, only loses its row. A volume that has to wait
// goes to the end of the queue, so that those that wait long never hold
// the batch.
func (c *Controller) volumesLoop(ctx context.Context) error {
	vols, err := c.st.DeletingEnvVolumes(ctx, volumesBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, vol := range vols {
		gone, err := c.deleteVolume(ctx, vol)
		if err != nil && ctx.Err() == nil {
			errs = append(errs, fmt.Errorf("volume %s of %s: %w", vol.VolumeID, vol.Object, err))
		}
		if !gone && ctx.Err() == nil {
			if err := c.fenced(ctx, func(tx *store.Store) error {
				return tx.EnvCtlTouchVolume(ctx, vol.EnvID, vol.Object)
			}); err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
				errs = append(errs, fmt.Errorf("volume %s of %s: %w", vol.VolumeID, vol.Object, err))
			}
		}
	}
	return errors.Join(errs...)
}

// nodeGone reports whether a node will never serve its disks again: its
// row is gone, or it was removed (a removed node is never deleted and
// never connects again, and its disks are gone with it, as the engine's
// DropRemovedNodeTombstones assumes).
func (c *Controller) nodeGone(ctx context.Context, node string) (bool, error) {
	n, err := c.st.GetNode(ctx, node)
	switch {
	case errors.Is(err, store.ErrNoRow):
		return true, nil
	case err != nil:
		return false, err
	}
	return n.Status == model.NodeRemoved, nil
}

// deleteVolume deletes one volume marked deleting, and reports whether its
// row went.
func (c *Controller) deleteVolume(ctx context.Context, vol store.EnvVolume) (bool, error) {
	if vol.Provisioned && vol.Node != "" {
		gone, err := c.nodeGone(ctx, vol.Node)
		switch {
		case err != nil:
			return false, err
		case gone:
			c.log.Info("a volume's node is gone: its row goes without a node call", "environment", vol.EnvID,
				"object", vol.Object, "volume", vol.VolumeID, "node", vol.Node)
		case !c.connected(vol.Node):
			return false, nil
		default:
			cctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
			cc, err := c.conn(cctx, vol.Node)
			if err == nil {
				_, err = volumev1.NewVolumeServiceClient(cc).DeleteVolume(cctx, &volumev1.DeleteVolumeRequest{VolumeId: vol.VolumeID.String()})
			}
			cancel()
			switch status.Code(err) {
			case codes.OK, codes.NotFound:
			case codes.FailedPrecondition:
				return false, nil // a pod still holds it
			default:
				return false, err
			}
		}
	}
	err := c.fenced(ctx, func(tx *store.Store) error {
		_, err := tx.DeleteEnvVolume(ctx, vol.EnvID, vol.Object)
		return err
	})
	if err != nil {
		return false, err
	}
	c.log.Info("volume deleted", "environment", vol.EnvID, "object", vol.Object, "volume", vol.VolumeID, "node", vol.Node)
	return true, nil
}

// deleteBatch is how many deleting environments one env-delete pass
// takes.
const deleteBatch = 16

// deleteLoop is env-delete: the environments of deleting tenants are
// marked deleting; each deleting environment loses its instances
// (Engine.Delete), its attached services (their processes stop and their
// hosts blocks go), its volumes (marked deleting) and its group on every
// connected node; its row goes once no instance row and no volume is
// left (its releases, secrets, runs, incidents and route tokens cascade).
func (c *Controller) deleteLoop(ctx context.Context) error {
	tenants, err := c.st.DeletingTenants(ctx)
	if err != nil {
		return err
	}
	for _, t := range tenants {
		var marked []string
		err := c.fenced(ctx, func(tx *store.Store) error {
			ids, err := tx.MarkTenantEnvironmentsDeleting(ctx, t)
			for _, id := range ids {
				marked = append(marked, id.String())
			}
			return err
		})
		if err != nil {
			return err
		}
		if len(marked) > 0 {
			c.log.Info("a deleting tenant's environments are deleted", "tenant", t, "environments", marked)
		}
	}
	envs, err := c.st.DeletingEnvironments(ctx, deleteBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range envs {
		if err := c.deleteEnvironment(ctx, e); err != nil && ctx.Err() == nil {
			errs = append(errs, fmt.Errorf("environment %s: %w", e.ID, err))
		}
	}
	return errors.Join(errs...)
}

// deleteEnvironment is one pass of env-delete over an environment.
func (c *Controller) deleteEnvironment(ctx context.Context, e store.Environment) error {
	rows, err := c.st.EnvCtlSandboxes(ctx, e.ID)
	if err != nil {
		return err
	}
	for _, sb := range rows {
		if err := c.deleteInstance(ctx, sb); err != nil {
			return err
		}
	}
	attached, err := c.st.ListAttached(ctx, e.ID)
	if err != nil {
		return err
	}
	v := &view{env: e, target: emptySpec(), latestSpec: emptySpec()}
	for _, a := range attached {
		c.stopAttached(ctx, v, a)
		if err := c.fenced(ctx, func(tx *store.Store) error {
			_, err := tx.DeleteAttached(ctx, e.ID, a.Service)
			return err
		}); err != nil {
			return err
		}
		c.forget("a/" + attachedKey(e.ID, a.Service))
	}
	if len(attached) > 0 {
		c.notify(ctx, HintGroups)
	}
	if err := c.fenced(ctx, func(tx *store.Store) error {
		_, err := tx.EnvCtlMarkVolumesDeleting(ctx, e.ID)
		return err
	}); err != nil {
		return err
	}
	// The env group goes from every node this instance reaches, once no
	// pod there names it any more (FAILED_PRECONDITION: a later pass); a
	// node out of reach keeps it until env-groups finds it of an
	// environment that is gone.
	groupsGone := true
	if c.d.Conns != nil {
		for _, node := range c.d.Conns.Nodes() {
			gone, err := c.deleteGroup(ctx, node, model.EnvGroupID(e.ID))
			if err != nil {
				return err
			}
			groupsGone = groupsGone && gone
		}
	}
	left, err := c.st.EnvCtlSandboxes(ctx, e.ID)
	if err != nil {
		return err
	}
	vols, err := c.st.ListEnvVolumes(ctx, e.ID)
	if err != nil {
		return err
	}
	if len(left) == 0 {
		// The pods are going: their volumes may go too (env-volumes
		// would, within its interval).
		for _, vol := range vols {
			if _, err := c.deleteVolume(ctx, vol); err != nil {
				return err
			}
		}
		if vols, err = c.st.ListEnvVolumes(ctx, e.ID); err != nil {
			return err
		}
	}
	if len(left) > 0 || len(vols) > 0 || !groupsGone {
		return nil
	}
	return c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockEnvironment(ctx, e.ID, store.ForUpdate)
		if errors.Is(err, store.ErrNoRow) {
			return nil
		}
		if err != nil || !cur.Deleting {
			return err
		}
		if _, err := tx.DeleteEnvironment(ctx, e.ID); err != nil {
			return err
		}
		c.log.Info("environment deleted", "environment", e.ID, "name", e.Name, "owner", e.Owner)
		return nil
	})
}

// emptySpec is the spec of nothing.
func emptySpec() *spec.Spec { return &spec.Spec{} }
