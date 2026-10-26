// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/ironcore-dev/feos/go/internal/podid"
)

// The audit records of a pod's feos-authn that the access route reads
// back, by activity kind.
const (
	KindKubeRequest  = "kube_request"  // one Kubernetes request through a binding
	KindSSHSignature = "ssh_signature" // one SSH signature by a binding's key
	KindKubeDeny     = "kube_deny"     // a Kubernetes request refused
	KindSSHDeny      = "ssh_deny"      // an SSH signature refused
)

// BindingUse sums one binding's use from the sandbox's attributed authn
// rows: kube_request rows for a Kubernetes binding, ssh_signature rows for
// an SSH binding, grouped by detail->>'binding'.
type BindingUse struct {
	Kind    string // KindKubeRequest or KindSSHSignature
	Binding string
	Uses    int64
	// Errors counts the kube_request rows whose detail->>'status' lies
	// outside 200–399 (or is missing); 0 for SSH.
	Errors int64
	// LastTsNs and LastName are the newest row's time and name.
	LastTsNs int64
	LastName string
}

// BindingDeny is one kube_deny or ssh_deny row, its fields from the detail
// JSON.
type BindingDeny struct {
	Kind    string // KindKubeDeny or KindSSHDeny
	Binding string
	Reason  string
	Detail  string
	TsNs    int64
	Name    string
}

// BindingUsage returns the use of every binding the sandbox's attributed
// authn rows name (bindings never used do not appear) and the newest
// denyLimit denies, newest first. Foreign records carry no sandbox and
// never count.
func (s *Store) BindingUsage(ctx context.Context, sandboxID podid.ID, denyLimit int) ([]BindingUse, []BindingDeny, error) {
	rows, err := s.q.Query(ctx, `SELECT DISTINCT ON (kind, binding) kind, binding, uses, errors, ts_ns, name FROM (
			SELECT kind, coalesce(detail->>'binding', '') AS binding, ts_ns, name, id,
				count(*) OVER w AS uses,
				count(*) FILTER (WHERE kind = 'kube_request' AND
					CASE WHEN detail->>'status' ~ '^[0-9]+(\.[0-9]+)?$' THEN (detail->>'status')::numeric ELSE 0 END
					NOT BETWEEN 200 AND 399) OVER w AS errors
			FROM activity
			WHERE sandbox_id = $1 AND source = 'authn' AND kind IN ('kube_request', 'ssh_signature')
			WINDOW w AS (PARTITION BY kind, coalesce(detail->>'binding', ''))) u
		ORDER BY kind, binding, ts_ns DESC, id DESC`, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	uses, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BindingUse, error) {
		var u BindingUse
		err := r.Scan(&u.Kind, &u.Binding, &u.Uses, &u.Errors, &u.LastTsNs, &u.LastName)
		return u, err
	})
	if err != nil {
		return nil, nil, err
	}
	if denyLimit <= 0 {
		denyLimit = 20
	}
	rows, err = s.q.Query(ctx, `SELECT kind, coalesce(detail->>'binding', ''), coalesce(detail->>'reason', ''),
			coalesce(detail->>'detail', ''), ts_ns, name
		FROM activity
		WHERE sandbox_id = $1 AND source = 'authn' AND kind IN ('kube_deny', 'ssh_deny')
		ORDER BY ts_ns DESC, id DESC LIMIT $2`, sandboxID, denyLimit)
	if err != nil {
		return nil, nil, err
	}
	denies, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BindingDeny, error) {
		var d BindingDeny
		err := r.Scan(&d.Kind, &d.Binding, &d.Reason, &d.Detail, &d.TsNs, &d.Name)
		return d, err
	})
	if err != nil {
		return nil, nil, err
	}
	return uses, denies, nil
}
