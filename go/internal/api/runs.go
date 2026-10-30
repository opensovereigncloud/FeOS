// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/gardenlinux/feos/go/internal/apierr"
	envs "github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// Runs is the tenants' runs (*env.Service, ADR 0018 "A tenant's run"):
// one method per run route, tenant class. nil: no run route is
// registered.
type Runs interface {
	CreateRun(ctx context.Context, tenant, key string, req envs.CreateRunRequest) (envs.RunStatus, bool, error)
	ListRuns(ctx context.Context, tenant string, labels map[string]string) (envs.RunList, error)
	GetRun(ctx context.Context, tenant, id string) (envs.RunStatus, error)
	RunLogs(ctx context.Context, tenant, id string, lines int) (envs.RunLogs, error)
	CancelRun(ctx context.Context, tenant, id string) (envs.RunStatus, error)
	DeleteRun(ctx context.Context, tenant, id string) error
}

// runRoutes registers the runs' routes, tenant class, each a thin adapter
// of a request to one method of Deps.Runs; none without the service.
func (s *server) runRoutes(tenant func(pattern string, h apiHandler)) {
	if s.deps.Runs == nil {
		return
	}
	tenant("POST /api/v1/runs", s.createRun)
	tenant("GET /api/v1/runs", s.listRuns)
	tenant("GET /api/v1/runs/{id}", s.getRun)
	tenant("DELETE /api/v1/runs/{id}", s.deleteRun)
	tenant("GET /api/v1/runs/{id}/logs", s.runLogs)
	tenant("POST /api/v1/runs/{id}/cancel", s.cancelRun)
}

// createRun is POST /api/v1/runs, with an Idempotency-Key as the tool
// calls take one: 201 for a new run, 200 with the run a create of the
// same request and key made.
func (s *server) createRun(w http.ResponseWriter, r *http.Request) error {
	key, err := tools.IdempotencyKey(r)
	if err != nil {
		return err
	}
	var req envs.CreateRunRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	run, created, err := s.deps.Runs.CreateRun(r.Context(), tenantOf(r), key, req)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return writeJSON(w, status, run)
}

// listRuns is GET /api/v1/runs?label=k=v…: every label must match.
func (s *server) listRuns(w http.ResponseWriter, r *http.Request) error {
	labels := map[string]string{}
	for _, l := range r.URL.Query()["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return apierr.New(http.StatusBadRequest, "label must be key=value, got %q", l)
		}
		labels[k] = v
	}
	list, err := s.deps.Runs.ListRuns(r.Context(), tenantOf(r), labels)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request) error {
	run, err := s.deps.Runs.GetRun(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, run)
}

func (s *server) deleteRun(w http.ResponseWriter, r *http.Request) error {
	if err := s.deps.Runs.DeleteRun(r.Context(), tenantOf(r), r.PathValue("id")); err != nil {
		return err
	}
	return noContent(w, http.StatusNoContent)
}

// runLogs is GET /api/v1/runs/{id}/logs?lines=N.
func (s *server) runLogs(w http.ResponseWriter, r *http.Request) error {
	lines, err := queryInt(r, "lines")
	if err != nil {
		return err
	}
	out, err := s.deps.Runs.RunLogs(r.Context(), tenantOf(r), r.PathValue("id"), lines)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *server) cancelRun(w http.ResponseWriter, r *http.Request) error {
	run, err := s.deps.Runs.CancelRun(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, run)
}
