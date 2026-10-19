// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// OperatorTenant is the tenant the bootstrap token belongs to.
const OperatorTenant = "operator"

// TenantNamePattern is the rule for tenant names (tenants.id).
var TenantNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,62}$`)

// Tenant is a row of tenants.
type Tenant struct {
	ID         string // the tenant's name
	CreatedAt  time.Time
	DeletingAt time.Time // zero unless a DELETE is under way
}

// Deleting reports whether the tenant is being deleted.
func (t *Tenant) Deleting() bool { return !t.DeletingAt.IsZero() }

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	var deleting *time.Time
	if err := row.Scan(&t.ID, &t.CreatedAt, &deleting); err != nil {
		return Tenant{}, err
	}
	t.DeletingAt = timeOf(deleting)
	return t, nil
}

// CreateTenant inserts a tenant. It returns the new row and true, or the
// existing row and false when the name is taken.
func (s *Store) CreateTenant(ctx context.Context, id string) (Tenant, bool, error) {
	t, err := scanTenant(s.q.QueryRow(ctx, `INSERT INTO tenants (id) VALUES ($1) ON CONFLICT (id) DO NOTHING
		RETURNING id, created_at, deleting_at`, id))
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, false, err
	}
	t, err = s.GetTenant(ctx, id)
	return t, false, err
}

// GetTenant reads a tenant; ErrNoRow if there is none.
func (s *Store) GetTenant(ctx context.Context, id string) (Tenant, error) {
	return s.LockTenant(ctx, id, NoLock)
}

// LockTenant reads a tenant and locks its row; ErrNoRow if there is none.
func (s *Store) LockTenant(ctx context.Context, id string, lock RowLock) (Tenant, error) {
	t, err := scanTenant(s.q.QueryRow(ctx, "SELECT id, created_at, deleting_at FROM tenants WHERE id = $1 "+string(lock), id))
	return t, noRow(err, ErrNoRow)
}

// TenantInfo is a tenant with what the admin list shows of it.
type TenantInfo struct {
	Tenant
	SandboxCount int64
	// EnvironmentCount is its environments that are not being deleted:
	// a tenant's deletion takes them, and their addons' data, along.
	EnvironmentCount int64
	Tokens           []TenantToken
}

// ListTenants returns every tenant ordered by name, with its number of
// sandboxes (an environment's instance rows are none), of environments
// (those being deleted are none) and its tokens (oldest first).
func (s *Store) ListTenants(ctx context.Context) ([]TenantInfo, error) {
	rows, err := s.q.Query(ctx, `SELECT t.id, t.created_at, t.deleting_at,
			(SELECT count(*) FROM sandboxes s WHERE s.owner = t.id AND s.environment_id IS NULL),
			(SELECT count(*) FROM environments e WHERE e.owner = t.id AND NOT e.deleting)
		FROM tenants t ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	tenants, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TenantInfo, error) {
		var ti TenantInfo
		var deleting *time.Time
		err := r.Scan(&ti.ID, &ti.CreatedAt, &deleting, &ti.SandboxCount, &ti.EnvironmentCount)
		ti.DeletingAt = timeOf(deleting)
		return ti, err
	})
	if err != nil {
		return nil, err
	}
	tokens, err := s.listTenantTokens(ctx, "")
	if err != nil {
		return nil, err
	}
	byTenant := map[string][]TenantToken{}
	for _, tk := range tokens {
		byTenant[tk.TenantID] = append(byTenant[tk.TenantID], tk)
	}
	for i := range tenants {
		tenants[i].Tokens = nonNil(byTenant[tenants[i].ID])
	}
	return tenants, nil
}

// MarkTenantDeleting starts a tenant's deletion: deleting_at = now()
// unless set, and every tenant token deleted, so that its requests and its
// sandbox tokens stop resolving at once. The caller holds the tenant's row
// FOR UPDATE. It reports whether the tenant exists.
func (s *Store) MarkTenantDeleting(ctx context.Context, id string) (bool, error) {
	tag, err := s.q.Exec(ctx, "UPDATE tenants SET deleting_at = coalesce(deleting_at, now()) WHERE id = $1", id)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	_, err = s.q.Exec(ctx, "DELETE FROM tenant_tokens WHERE tenant_id = $1", id)
	return true, err
}

// DeleteTenantIfEmpty removes the tenant when it has no sandboxes and no
// environments left (the environments' controller deletes those of a
// deleting tenant). It reports whether it did.
func (s *Store) DeleteTenantIfEmpty(ctx context.Context, id string) (bool, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM tenants WHERE id = $1
		AND NOT EXISTS (SELECT 1 FROM sandboxes WHERE owner = $1)
		AND NOT EXISTS (SELECT 1 FROM environments WHERE owner = $1)`, id)
	return tag.RowsAffected() > 0, err
}

// DeletingTenants returns the tenants whose deletion has begun, oldest
// first.
func (s *Store) DeletingTenants(ctx context.Context) ([]string, error) {
	rows, err := s.q.Query(ctx, "SELECT id FROM tenants WHERE deleting_at IS NOT NULL ORDER BY deleting_at, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// BootstrapOperator creates the tenant "operator" with a token of hash
// tokenHash (label "bootstrap"), but only when no tenant exists at all. It
// reports whether it created them.
func (s *Store) BootstrapOperator(ctx context.Context, tokenHash string) (bool, error) {
	var id string
	err := s.q.QueryRow(ctx, `INSERT INTO tenants (id) SELECT $1
		WHERE NOT EXISTS (SELECT 1 FROM tenants) ON CONFLICT DO NOTHING RETURNING id`, OperatorTenant).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = s.q.Exec(ctx, `INSERT INTO tenant_tokens (hash, tenant_id, label) VALUES ($1, $2, 'bootstrap')
		ON CONFLICT DO NOTHING`, tokenHash, OperatorTenant)
	return err == nil, err
}
