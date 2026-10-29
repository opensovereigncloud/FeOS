// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/domains"
)

// Domains is the domains service (*domains.Service, ADR 0021): one method
// per domain route, tenant class. nil: no domain route is registered.
type Domains interface {
	CreateDomain(ctx context.Context, tenant string, req domains.CreateRequest) (domains.Domain, error)
	ListDomains(ctx context.Context, tenant, env string, labels map[string]string) (domains.List, error)
	GetDomain(ctx context.Context, tenant, id string) (domains.Domain, error)
	BindDomain(ctx context.Context, tenant, id, env string) (domains.Domain, error)
	UnbindDomain(ctx context.Context, tenant, id string) (domains.Domain, error)
	CheckDomain(ctx context.Context, tenant, id string) (domains.Domain, error)
	DeleteDomain(ctx context.Context, tenant, id string) error
}

// domainRoutes registers the domains' routes, tenant class, each a thin
// adapter of a request to one method of Deps.Domains; none without the
// service.
func (s *server) domainRoutes(tenant func(pattern string, h apiHandler)) {
	if s.deps.Domains == nil {
		return
	}
	tenant("POST /api/v1/domains", s.createDomain)
	tenant("GET /api/v1/domains", s.listDomains)
	tenant("GET /api/v1/domains/{id}", s.getDomain)
	tenant("DELETE /api/v1/domains/{id}", s.deleteDomain)
	tenant("PUT /api/v1/domains/{id}/environment", s.bindDomain)
	tenant("DELETE /api/v1/domains/{id}/environment", s.unbindDomain)
	tenant("POST /api/v1/domains/{id}/check", s.checkDomain)
}

func (s *server) createDomain(w http.ResponseWriter, r *http.Request) error {
	var req domains.CreateRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	d, err := s.deps.Domains.CreateDomain(r.Context(), tenantOf(r), req)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, d)
}

// listDomains is GET /api/v1/domains?environment=<env>&label=k=v…: every
// label must match.
func (s *server) listDomains(w http.ResponseWriter, r *http.Request) error {
	labels := map[string]string{}
	for _, l := range r.URL.Query()["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return apierr.New(http.StatusBadRequest, "label must be key=value, got %q", l)
		}
		labels[k] = v
	}
	list, err := s.deps.Domains.ListDomains(r.Context(), tenantOf(r), r.URL.Query().Get("environment"), labels)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *server) getDomain(w http.ResponseWriter, r *http.Request) error {
	d, err := s.deps.Domains.GetDomain(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}

func (s *server) deleteDomain(w http.ResponseWriter, r *http.Request) error {
	if err := s.deps.Domains.DeleteDomain(r.Context(), tenantOf(r), r.PathValue("id")); err != nil {
		return err
	}
	return noContent(w, http.StatusNoContent)
}

// bindDomain is PUT …/environment {"environment": "<id or name>"}.
func (s *server) bindDomain(w http.ResponseWriter, r *http.Request) error {
	var req domains.BindRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	d, err := s.deps.Domains.BindDomain(r.Context(), tenantOf(r), r.PathValue("id"), req.Environment)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}

func (s *server) unbindDomain(w http.ResponseWriter, r *http.Request) error {
	d, err := s.deps.Domains.UnbindDomain(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}

func (s *server) checkDomain(w http.ResponseWriter, r *http.Request) error {
	d, err := s.deps.Domains.CheckDomain(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}
