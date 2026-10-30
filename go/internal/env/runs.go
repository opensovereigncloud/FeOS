// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// CodeRunKeyConflict is a create whose Idempotency-Key names a run made
// with another request (422).
const CodeRunKeyConflict = "run_key_conflict"

// CreateRunRequest is POST /api/v1/runs (ADR 0018, "A tenant's run").
type CreateRunRequest struct {
	// Image is pinned to its digest at the create, asked with
	// ImageCredentials.
	Image string `json:"image"`
	// Command replaces the image's entrypoint and command; empty: they
	// run.
	Command []string `json:"command,omitempty"`
	// Env and Secrets are the container's environment variables; the
	// secrets' values are sealed, never returned and redacted from the
	// log.
	Env     map[string]string `json:"env,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	// ImageCredentials are the login for the image's registry (ADR 0025),
	// sealed; nil: anonymous.
	ImageCredentials *model.RegistryCredentials `json:"imageCredentials,omitempty"`
	Vcpus            int                        `json:"vcpus,omitempty"`
	MemoryMib        int                        `json:"memoryMib,omitempty"`
	// Deadline counts from the create, the queue included; "": 10m, at
	// most 2h.
	Deadline string `json:"deadline,omitempty"`
	// ResultPath is a file of the container that is read once its
	// process exited, its first 4 KiB the run's result; "": none.
	ResultPath string            `json:"resultPath,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// RunStatus is a run as the answers show it.
type RunStatus struct {
	ID string `json:"id"`
	// Image is as asked, ImageRef pinned.
	Image    string `json:"image"`
	ImageRef string `json:"imageRef"`
	// Phase is queued, running, succeeded, failed, deadline_exceeded or
	// cancelled; Reason, of a failed one, exit, image, node_lost or
	// no_exit_status.
	Phase      string            `json:"phase"`
	Reason     string            `json:"reason"`
	Detail     string            `json:"detail"`
	ExitCode   *int              `json:"exitCode"`
	Result     string            `json:"result"`
	ResultPath string            `json:"resultPath"`
	Vcpus      int               `json:"vcpus"`
	MemoryMib  int               `json:"memoryMib"`
	Labels     map[string]string `json:"labels"`
	CreatedAt  string            `json:"createdAt"`
	StartedAt  string            `json:"startedAt"`
	FinishedAt string            `json:"finishedAt"`
	DeadlineAt string            `json:"deadlineAt"`
}

// RunList is GET /api/v1/runs.
type RunList struct {
	Runs []RunStatus `json:"runs"`
}

// RunLogs is GET /api/v1/runs/{id}/logs.
type RunLogs struct {
	Lines []string `json:"lines"`
}

func runStatus(r store.Run) RunStatus {
	return RunStatus{ID: r.ID.String(), Image: r.Image, ImageRef: r.ImageRef, Phase: r.Phase, Reason: r.Reason,
		Detail: r.Detail, ExitCode: r.ExitCode, Result: r.Result, ResultPath: r.ResultPath, Vcpus: r.VCPUs,
		MemoryMib: r.MemoryMiB, Labels: r.Labels, CreatedAt: rfc3339(r.CreatedAt), StartedAt: rfc3339(r.StartedAt),
		FinishedAt: rfc3339(r.FinishedAt), DeadlineAt: rfc3339(r.DeadlineAt)}
}

// CreateRun is POST /api/v1/runs: a run of the tenant, created (true) or,
// for a key a create with the same request made, the run that create
// made (false). A key that names a run of another request is 422
// run_key_conflict; the secrets and image credentials need
// --secret-key-file (503).
func (s *Service) CreateRun(ctx context.Context, tenant, key string, req CreateRunRequest) (RunStatus, bool, error) {
	rs, err := runSpec(req)
	if err != nil {
		return RunStatus{}, false, err
	}
	rs.Key, rs.Hash = key, requestHash(req)
	r, created, err := s.ctl.CreateRun(ctx, tenant, rs)
	if err != nil {
		return RunStatus{}, false, runError(err)
	}
	return runStatus(r), created, nil
}

// requestHash is a create's hash for its key: the request without the
// values of its secrets and image credentials, which a caller mints anew
// when it sends a create again.
func requestHash(req CreateRunRequest) string {
	names := slices.Sorted(maps.Keys(req.Secrets))
	req.Secrets = nil
	hasCreds := req.ImageCredentials != nil
	req.ImageCredentials = nil
	b, _ := json.Marshal(struct {
		CreateRunRequest
		SecretNames []string `json:"secretNames"`
		Credentials bool     `json:"credentials"`
	}{req, names, hasCreds})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// runSpec validates a create (400 for what it gets wrong) and fills in
// the defaults.
func runSpec(req CreateRunRequest) (ctl.RunSpec, error) {
	rs := ctl.RunSpec{Image: strings.TrimSpace(req.Image), Command: req.Command, Env: req.Env, Secrets: req.Secrets,
		ImageCredentials: req.ImageCredentials, ResultPath: req.ResultPath, Labels: req.Labels}
	if rs.Image == "" {
		return rs, errBadRequest("image is required")
	}
	for _, m := range []map[string]string{req.Env, req.Secrets} {
		for k := range m {
			if !spec.ValidEnvName(k) {
				return rs, errBadRequest("%q is not an environment variable's name", k)
			}
		}
	}
	for k := range req.Secrets {
		if _, ok := req.Env[k]; ok {
			return rs, errBadRequest("%s is in env and in secrets", k)
		}
	}
	if c := req.ImageCredentials; c != nil && (c.Username == "" || c.Password == "") {
		return rs, errBadRequest("imageCredentials need a username and a password")
	}
	rs.VCPUs, rs.MemoryMiB = spec.DefaultVCPUs, spec.DefaultMemoryMib
	if req.Vcpus != 0 {
		if req.Vcpus < spec.MinVCPUs || req.Vcpus > spec.MaxVCPUs {
			return rs, errBadRequest("vcpus %d is out of range: use %d..%d", req.Vcpus, spec.MinVCPUs, spec.MaxVCPUs)
		}
		rs.VCPUs = req.Vcpus
	}
	if req.MemoryMib != 0 {
		if req.MemoryMib < spec.MinMemoryMib || req.MemoryMib > spec.MaxMemoryMib {
			return rs, errBadRequest("memoryMib %d is out of range: use %d..%d", req.MemoryMib, spec.MinMemoryMib,
				spec.MaxMemoryMib)
		}
		rs.MemoryMiB = req.MemoryMib
	}
	deadline := req.Deadline
	if deadline == "" {
		deadline = spec.DefaultJobTimeout
	}
	d, err := time.ParseDuration(deadline)
	limit, _ := time.ParseDuration(spec.MaxJobTimeout)
	if err != nil || d <= 0 || d > limit {
		return rs, errBadRequest("deadline %q is not a duration of up to %s (90s, 15m, 1h30m)", req.Deadline, limit)
	}
	rs.Deadline = d
	if p := req.ResultPath; p != "" {
		clean := path.Clean(p)
		if !path.IsAbs(p) || clean == "/" || clean != p || clean == "/dev" || strings.HasPrefix(clean, "/dev/") {
			return rs, errBadRequest("resultPath %q must be an absolute, clean path of a file outside /dev", p)
		}
	}
	for k := range req.Labels {
		if k == "" {
			return rs, errBadRequest("a label's key is empty")
		}
	}
	return rs, nil
}

// runError maps the controller's errors of the runs.
func runError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ctl.ErrNoObject), errors.Is(err, store.ErrNoRow):
		return errNoRun()
	case errors.Is(err, ctl.ErrRunKeyConflict):
		return apierr.WithCode(http.StatusUnprocessableEntity, CodeRunKeyConflict, "%s", err.Error())
	case errors.Is(err, ctl.ErrRunImage):
		return apierr.New(http.StatusUnprocessableEntity, "%s", err.Error())
	case errors.Is(err, ctl.ErrNoContainerLogs):
		return apierr.New(http.StatusNotImplemented, "%s", err.Error())
	case err.Error() == "secrets need --secret-key-file":
		return errNoSecrets()
	case err.Error() == "the tenant is being deleted":
		return apierr.New(http.StatusConflict, "%s", err.Error())
	}
	return err
}

func errNoRun() error { return apierr.New(http.StatusNotFound, "no such run") }

// runID parses a run's ID; one that is none is no run (404).
func runID(s string) (podid.ID, error) {
	id, err := podid.Parse(s)
	if err != nil || len(id) != podid.Len {
		return "", errNoRun()
	}
	return id, nil
}

// GetRun is GET /api/v1/runs/{id}.
func (s *Service) GetRun(ctx context.Context, tenant, id string) (RunStatus, error) {
	rid, err := runID(id)
	if err != nil {
		return RunStatus{}, err
	}
	r, err := s.st.GetRun(ctx, tenant, rid)
	if err != nil {
		return RunStatus{}, runError(err)
	}
	return runStatus(r), nil
}

// ListRuns is GET /api/v1/runs?label=k=v…: the tenant's kept runs that
// carry every label, newest first.
func (s *Service) ListRuns(ctx context.Context, tenant string, labels map[string]string) (RunList, error) {
	rows, err := s.st.ListRuns(ctx, tenant, labels)
	if err != nil {
		return RunList{}, err
	}
	out := RunList{Runs: make([]RunStatus, 0, len(rows))}
	for _, r := range rows {
		out.Runs = append(out.Runs, runStatus(r))
	}
	return out, nil
}

// RunLogs is GET /api/v1/runs/{id}/logs?lines=N (100 when not above 0, at
// most 10000): from the run's pod while it runs, its kept tail once it
// has ended.
func (s *Service) RunLogs(ctx context.Context, tenant, id string, lines int) (RunLogs, error) {
	rid, err := runID(id)
	if err != nil {
		return RunLogs{}, err
	}
	if lines <= 0 {
		lines = defaultLogLines
	}
	out, err := s.ctl.RunLog(ctx, tenant, rid, min(lines, maxLogLines))
	if err != nil {
		return RunLogs{}, runError(err)
	}
	if out == nil {
		out = []string{}
	}
	return RunLogs{Lines: out}, nil
}

// CancelRun is POST /api/v1/runs/{id}/cancel: the run as it is after the
// cancel, which changes nothing of a run that has ended.
func (s *Service) CancelRun(ctx context.Context, tenant, id string) (RunStatus, error) {
	rid, err := runID(id)
	if err != nil {
		return RunStatus{}, err
	}
	r, err := s.ctl.CancelRun(ctx, tenant, rid)
	if err != nil {
		return RunStatus{}, runError(err)
	}
	return runStatus(r), nil
}

// DeleteRun is DELETE /api/v1/runs/{id}: a run that has not ended is
// cancelled first; its record goes.
func (s *Service) DeleteRun(ctx context.Context, tenant, id string) error {
	rid, err := runID(id)
	if err != nil {
		return err
	}
	return runError(s.ctl.DeleteRun(ctx, tenant, rid))
}
