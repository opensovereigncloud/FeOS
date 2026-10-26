// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package auth carries who a request is from through its context. The REST
// router authenticates a request by the credential its route takes (a
// sandbox's token, a tenant's token, or an operator's password or sign-in
// session) and puts the result here; the handlers of the other packages
// read it and never look at the credential themselves.
package auth

import (
	"context"

	"github.com/ironcore-dev/feos/go/internal/podid"
)

// Sandbox is a request made with a sandbox's token: the token is the
// sandbox.
type Sandbox struct {
	ID podid.ID
	// The tenant that owns the sandbox.
	Owner string
	// The SHA-256 of the token, hex: what identifies the caller across
	// requests, for example an MCP call to cancel.
	TokenHash string
}

// Tenant is a request made with a tenant's token.
type Tenant struct {
	Name string
}

// Admin is a request made by an operator: with a password of
// --admin-users, or with the session of a person who signed in through
// OIDC and is in the admin group.
type Admin struct {
	// User is who acted, as the admin views record it (join tokens'
	// created_by, tenant creation, node removal): the Basic user, or the
	// signed-in person's username, else their email.
	User string
	// Sub is the OIDC subject of a signed-in person; "" for Basic.
	Sub string
	// Method is how the operator authenticated: AdminBasic or AdminOIDC.
	Method string
}

// The methods an operator authenticates with.
const (
	AdminBasic = "basic"
	AdminOIDC  = "oidc"
)

type sandboxKey struct{}
type tenantKey struct{}
type adminKey struct{}

func WithSandbox(ctx context.Context, s Sandbox) context.Context {
	return context.WithValue(ctx, sandboxKey{}, s)
}

// SandboxFrom returns the sandbox a request was made as, if it was.
func SandboxFrom(ctx context.Context) (Sandbox, bool) {
	s, ok := ctx.Value(sandboxKey{}).(Sandbox)
	return s, ok
}

func WithTenant(ctx context.Context, t Tenant) context.Context {
	return context.WithValue(ctx, tenantKey{}, t)
}

// TenantFrom returns the tenant a request was made as, if it was.
func TenantFrom(ctx context.Context) (Tenant, bool) {
	t, ok := ctx.Value(tenantKey{}).(Tenant)
	return t, ok
}

func WithAdmin(ctx context.Context, a Admin) context.Context {
	return context.WithValue(ctx, adminKey{}, a)
}

// AdminFrom returns the operator a request was made as, if it was.
func AdminFrom(ctx context.Context) (Admin, bool) {
	a, ok := ctx.Value(adminKey{}).(Admin)
	return a, ok
}
