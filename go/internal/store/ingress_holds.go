// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// The ingress's holds (ADR 0021, D11 of its design): before it forwards a
// request to an instance of a service with an idle policy, ferun-ingress
// holds the instance as the port door does, a hold of kind forward
// admitted under the sandbox's row lock, so that an idle pause never
// freezes a request that reached the pod. The holds lapse by their lease
// when the ingress stops renewing them; while an ingress that has not left
// has not been seen for a while, ingressFence keeps public services from
// idling instead.

// The refusals of AdmitIngressHold.
const (
	IngressHoldGone       = "gone"
	IngressHoldNotPublic  = "not public"
	IngressHoldNotRunning = "not running"
	IngressHoldBusy       = "busy"
)

// IngressHolder is the holder of the holds of the ingress instance name:
// "ingress:<name>".
func IngressHolder(name string) string { return "ingress:" + name }

// AdmitIngressHold writes holder's hold of kind forward on the sandbox id
// for lease, renewed by the ferund instance instanceID, and sets
// last_active_at = now(), in a transaction of its own with the sandbox's
// row locked FOR NO KEY UPDATE. It refuses, with the reason and no error,
// a sandbox that does not exist (IngressHoldGone), that is not a service
// instance of a public environment that is not being deleted
// (IngressHoldNotPublic), that does not run (IngressHoldNotRunning), or
// that has an op in flight (IngressHoldBusy).
func (s *Store) AdmitIngressHold(ctx context.Context, id podid.ID, holder, instanceID string, lease time.Duration) (string, error) {
	var refusal string
	err := s.InTx(ctx, func(tx *Store) error {
		refusal = ""
		sb, err := tx.LockSandbox(ctx, id, ForNoKeyUpdate)
		if errors.Is(err, model.ErrNotFound) {
			refusal = IngressHoldGone
			return nil
		}
		if err != nil {
			return err
		}
		var routed bool
		if err := tx.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM env_instances i JOIN environments e ON e.id = i.env_id
			WHERE i.sandbox_id = $1 AND i.kind = 'service' AND e.public AND NOT e.deleting)`, id).Scan(&routed); err != nil {
			return err
		}
		switch {
		case !routed:
			refusal = IngressHoldNotPublic
			return nil
		case sb.ClientState != model.StateRunning:
			refusal = IngressHoldNotRunning
			return nil
		case sb.Op != "":
			refusal = IngressHoldBusy
			return nil
		}
		if err := tx.upsertHold(ctx, Hold{SandboxID: id, Holder: holder, Kind: model.HoldForward, InstanceID: instanceID}, lease); err != nil {
			return err
		}
		return tx.TouchSandbox(ctx, id)
	})
	if err != nil {
		return "", fmt.Errorf("admit the hold of %s on %s: %w", holder, id, err)
	}
	return refusal, nil
}

// ReleaseIngressHold deletes holder's hold on the sandbox id and, when
// there was one, sets last_active_at = now(), in one transaction, so that
// the sandbox's idle interval starts at the release. It reports whether
// there was a hold.
func (s *Store) ReleaseIngressHold(ctx context.Context, id podid.ID, holder string) (bool, error) {
	var released bool
	err := s.InTx(ctx, func(tx *Store) error {
		tag, err := tx.q.Exec(ctx, "DELETE FROM sandbox_holds WHERE sandbox_id = $1 AND holder = $2", id, holder)
		if err != nil {
			return err
		}
		if released = tag.RowsAffected() > 0; !released {
			return nil
		}
		return tx.TouchSandbox(ctx, id)
	})
	if err != nil {
		return false, fmt.Errorf("release the hold of %s on %s: %w", holder, id, err)
	}
	return released, nil
}

// LeaveIngress is the clean stop of the ingress instance name: it deletes
// every hold of IngressHolder(name), sets last_active_at = now() on their
// sandboxes and sets the instance's left_at, which takes it out of
// ingressFence until it is seen again, in one transaction.
func (s *Store) LeaveIngress(ctx context.Context, name string) error {
	err := s.InTx(ctx, func(tx *Store) error {
		if _, err := tx.q.Exec(ctx, `WITH d AS (DELETE FROM sandbox_holds WHERE holder = $1 RETURNING sandbox_id)
			UPDATE sandboxes SET last_active_at = now() WHERE id IN (SELECT sandbox_id FROM d)`, IngressHolder(name)); err != nil {
			return err
		}
		_, err := tx.q.Exec(ctx, "UPDATE ingresses SET left_at = now() WHERE name = $1", name)
		return err
	})
	if err != nil {
		return fmt.Errorf("leave ingress %s: %w", name, err)
	}
	return nil
}

// ingressFence is the condition on a sandbox row s under which the idle
// loop may pause it, beside the others: not while an ingress instance
// that is joined, has a mesh address and has not left was last seen 45 s
// to 15 min ago (it may be cut off from ferund and serve requests whose
// holds it cannot renew) and s is a service instance of a public
// environment. An ingress unseen for longer is taken to be gone.
const ingressFence = `NOT (EXISTS (SELECT 1 FROM ingresses g WHERE g.status = 'joined' AND g.mesh_address <> '' AND g.left_at IS NULL
			AND g.last_seen < now() - interval '45 seconds' AND g.last_seen > now() - interval '15 minutes')
		AND EXISTS (SELECT 1 FROM env_instances i JOIN environments e ON e.id = i.env_id
			WHERE i.sandbox_id = s.id AND i.kind = 'service' AND e.public))`
