// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/apierr"
	"github.com/ironcore-dev/feos/go/internal/auth"
	"github.com/ironcore-dev/feos/go/internal/config"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// The label of the token a create returns.
const firstTokenLabel = "default"

// mintSandboxToken returns a new sandbox token and its hash: 32 random
// bytes, base64url without padding (43 characters). Only the hash is
// stored; the token is shown once.
func mintSandboxToken() (token, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b[:])
	return token, store.HashToken(token), nil
}

// tenantOf is the tenant the request was authenticated as.
func tenantOf(r *http.Request) string {
	t, _ := auth.TenantFrom(r.Context())
	return t.Name
}

// errUnknownSandbox is the answer to a sandbox the tenant does not own,
// the same whether it is foreign, absent or an environment's instance
// (ADR 0018: the environments' API shows those).
func errUnknownSandbox(id string) error {
	return apierr.New(http.StatusNotFound, "unknown sandbox %s", id)
}

// owned reads the sandbox of the path's {id} that the request's tenant
// owns; an environment's instance row is none.
func (s *server) owned(r *http.Request) (store.Sandbox, error) {
	id, err := pathSandboxID(r)
	if err != nil {
		return store.Sandbox{}, err
	}
	sb, err := s.st.GetSandbox(r.Context(), id)
	if errors.Is(err, model.ErrNotFound) || (err == nil && (sb.Owner != tenantOf(r) || sb.IsInstance())) {
		return store.Sandbox{}, errUnknownSandbox(id.String())
	}
	if err != nil {
		return store.Sandbox{}, fmt.Errorf("get %s: %w", id, err)
	}
	return sb, nil
}

// createSandboxRequest is POST /api/v1/sandboxes.
type createSandboxRequest struct {
	// SandboxID is chosen by the caller (a UUID) or minted when empty.
	SandboxID string `json:"sandboxId"`
	// Image empty: the default image.
	Image string `json:"image"`
	// The size, fixed for the sandbox's life; 0: the default.
	Vcpus     int `json:"vcpus"`
	MemoryMib int `json:"memoryMib"`
	// Attachable gives the sandbox's pod a security group of its own,
	// sbx-<id>, which an environment's attached services in it reach the
	// environment's addons through (ADR 0018). Fixed for its life.
	Attachable bool `json:"attachable"`
}

// sandboxView is a sandbox as the sandbox routes show it: its status and
// whether it is attachable.
type sandboxView struct {
	sandboxStatus
	Attachable bool `json:"attachable"`
}

// viewOf renders a row for the sandbox routes.
func viewOf(sb store.Sandbox) sandboxView {
	return sandboxView{sandboxStatus: statusOf(sb), Attachable: sb.PodTemplate != nil && !sb.IsInstance()}
}

// attachableTemplate is the pod template of an attachable sandbox (ADR
// 0018 §2.3): the command of every sandbox's pod, no env, the default
// labels, the sandbox's own security group, which the environments it
// serves let reach their addons, and its own name table, which names its
// attached services and the addons and image services of their
// environments.
func attachableTemplate(id uuid.UUID) *model.PodTemplate {
	return &model.PodTemplate{Command: []string{"sleep", "infinity"}, SecurityGroups: []string{model.SandboxGroupID(id)},
		NameTables: []string{model.SandboxTableID(id)}}
}

type createSandboxResponse struct {
	Status sandboxView `json:"status"`
	// Token is the first token, only when this call created the sandbox.
	Token   string `json:"token,omitempty"`
	Created bool   `json:"created"`
}

// size validates a create's size; zeros take the defaults.
func (s *server) size(req createSandboxRequest) (vcpus, memoryMiB int, err error) {
	if req.Vcpus < 0 || req.Vcpus > config.MaxVCPUs {
		return 0, 0, apierr.New(http.StatusBadRequest, "vcpus must be between %d and %d (0 = default %d)",
			config.MinVCPUs, config.MaxVCPUs, s.cfg.DefaultVCPUs)
	}
	if req.MemoryMib != 0 && (req.MemoryMib < config.MinMemoryMiB || req.MemoryMib > config.MaxMemoryMiB) {
		return 0, 0, apierr.New(http.StatusBadRequest, "memoryMib must be between %d and %d (0 = default %d)",
			config.MinMemoryMiB, config.MaxMemoryMiB, s.cfg.DefaultMemoryMiB)
	}
	vcpus, memoryMiB = req.Vcpus, req.MemoryMib
	if vcpus == 0 {
		vcpus = s.cfg.DefaultVCPUs
	}
	if memoryMiB == 0 {
		memoryMiB = s.cfg.DefaultMemoryMiB
	}
	return vcpus, memoryMiB, nil
}

// createSandbox is POST /api/v1/sandboxes: in one transaction, the tenant
// (refused while it is being deleted), the row unless the ID exists, and
// the first token. Nothing reaches a node: the sandbox materialises at its
// first tool call. A repeat for an existing sandbox of the caller is a 200
// without a token (an attachable one stays as it was created); an ID of
// another tenant's sandbox, or of an environment's instance, is a 409.
func (s *server) createSandbox(w http.ResponseWriter, r *http.Request) error {
	var req createSandboxRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	vcpus, memoryMiB, err := s.size(req)
	if err != nil {
		return err
	}
	id := uuid.New()
	if req.SandboxID != "" {
		if id, err = parseSandboxID(req.SandboxID); err != nil {
			return err
		}
	}
	image := req.Image
	if image == "" {
		image = s.cfg.DefaultImage
	}
	tenant := tenantOf(r)
	token, hash, err := mintSandboxToken()
	if err != nil {
		return fmt.Errorf("create %s: %w", id, err)
	}
	var row store.Sandbox
	var created bool
	err = s.st.InTx(r.Context(), func(tx *store.Store) error {
		t, err := tx.LockTenant(r.Context(), tenant, store.ForShare)
		if isNoRow(err) {
			return apierr.New(http.StatusUnauthorized, "unknown tenant token")
		}
		if err != nil {
			return err
		}
		if t.Deleting() {
			return apierr.New(http.StatusConflict, "tenant %s is being deleted", tenant)
		}
		n := store.NewSandbox{ID: id, Owner: tenant, Image: image, VCPUs: vcpus, MemoryMiB: memoryMiB}
		if req.Attachable {
			n.PodTemplate = attachableTemplate(id)
		}
		row, created, err = tx.CreateSandbox(r.Context(), n)
		if err != nil {
			return err
		}
		if !created {
			if row.Owner != tenant || row.IsInstance() {
				return apierr.New(http.StatusConflict, "sandbox id %s already exists", id)
			}
			return nil
		}
		_, err = tx.CreateSandboxToken(r.Context(), hash, id, firstTokenLabel)
		return err
	})
	if err != nil {
		return passOr(err, "create "+id.String())
	}
	if !created {
		return writeJSON(w, http.StatusOK, createSandboxResponse{Status: viewOf(row)})
	}
	return writeJSON(w, http.StatusCreated, createSandboxResponse{Status: viewOf(row), Token: token, Created: true})
}

// listSandboxes is GET /api/v1/sandboxes: the tenant's, oldest first,
// without its environments' instances.
func (s *server) listSandboxes(w http.ResponseWriter, r *http.Request) error {
	rows, err := s.st.ListSandboxes(r.Context(), tenantOf(r))
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	out := struct {
		Sandboxes []sandboxView `json:"sandboxes"`
	}{Sandboxes: make([]sandboxView, 0, len(rows))}
	for _, sb := range rows {
		out.Sandboxes = append(out.Sandboxes, viewOf(sb))
	}
	return writeJSON(w, http.StatusOK, out)
}

// getSandbox is GET /api/v1/sandboxes/{id}: a read that never wakes it.
func (s *server) getSandbox(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, viewOf(sb))
}

// deleteSandbox is DELETE /api/v1/sandboxes/{id}: the row goes in one
// transaction (§9 "Delete"), its pod through tombstones. The answer is the
// status read, desired state deleted, and client state absent when a pod
// is being deleted. An environment's instance is not deleted here: its
// ID is fixed to it for its row's life, so the read before is enough.
func (s *server) deleteSandbox(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	id := sb.ID
	row, tombstoned, err := s.deps.Engine.Delete(r.Context(), tenantOf(r), id)
	if errors.Is(err, model.ErrNotFound) {
		return errUnknownSandbox(id.String())
	}
	if err != nil {
		return internalf("delete: %v", err)
	}
	st := viewOf(row)
	st.DesiredState = desiredDeleted
	if tombstoned {
		st.ClientState, st.State = stateAbsent, stateAbsent
	}
	return writeJSON(w, http.StatusOK, st)
}

// pauseSandbox is POST /api/v1/sandboxes/{id}/pause: a running sandbox is
// paused (waiting up to a minute; the pause goes on after); a paused,
// hibernated or pending one has nothing to do. The answer is the status
// as read before.
func (s *server) pauseSandbox(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	err = s.deps.Engine.Pause(r.Context(), sb.ID)
	switch {
	case err == nil, errors.Is(err, model.ErrPrecondition):
		// A recheck that found the row changed: someone else moved it.
	case errors.Is(err, model.ErrNotFound):
		return errUnknownSandbox(sb.ID.String())
	default:
		return internalf("pause: %v", err)
	}
	return writeJSON(w, http.StatusOK, viewOf(sb))
}

// resumeSandbox is POST /api/v1/sandboxes/{id}/resume: Ensure (within the
// preparation budget) and activity. The answer is the status as read
// before.
func (s *server) resumeSandbox(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	err = s.deps.Engine.Resume(r.Context(), sb.ID)
	switch {
	case err == nil:
	case errors.Is(err, model.ErrNotFound):
		return errUnknownSandbox(sb.ID.String())
	default:
		return internalf("resume: %v", err)
	}
	return writeJSON(w, http.StatusOK, viewOf(sb))
}

// tokenInfo is a sandbox token as listed: its hash, never the token.
type tokenInfo struct {
	Hash       string `json:"hash"`
	HashPrefix string `json:"hashPrefix"`
	SandboxID  string `json:"sandboxId"`
	Label      string `json:"label"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
}

// listSandboxTokens is GET /api/v1/sandboxes/{id}/tokens.
func (s *server) listSandboxTokens(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	toks, err := s.st.ListSandboxTokens(r.Context(), sb.ID)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	out := struct {
		Tokens []tokenInfo `json:"tokens"`
	}{Tokens: make([]tokenInfo, 0, len(toks))}
	for _, t := range toks {
		out.Tokens = append(out.Tokens, tokenInfo{
			Hash: t.Hash, HashPrefix: t.Hash[:8], SandboxID: t.SandboxID.String(), Label: t.Label,
			CreatedAt: rfc3339(t.CreatedAt), LastUsedAt: rfc3339(t.LastUsedAt),
		})
	}
	return writeJSON(w, http.StatusOK, out)
}

// mintedToken is the answer of a token mint: the token, shown once.
type mintedToken struct {
	Token      string `json:"token"`
	HashPrefix string `json:"hashPrefix"`
	CreatedAt  string `json:"createdAt"`
}

// createSandboxToken is POST /api/v1/sandboxes/{id}/tokens {label?}, in
// one transaction with the row FOR SHARE, so that a delete cannot slip in
// between the ownership check and the insert. An environment's instance
// gets no token.
func (s *server) createSandboxToken(w http.ResponseWriter, r *http.Request) error {
	id, err := pathSandboxID(r)
	if err != nil {
		return err
	}
	var req struct {
		Label string `json:"label"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	token, hash, err := mintSandboxToken()
	if err != nil {
		return fmt.Errorf("create token: %w", err)
	}
	var tok store.SandboxToken
	err = s.st.InTx(r.Context(), func(tx *store.Store) error {
		sb, err := tx.LockSandbox(r.Context(), id, store.ForShare)
		if errors.Is(err, model.ErrNotFound) || (err == nil && (sb.Owner != tenantOf(r) || sb.IsInstance())) {
			return errUnknownSandbox(id.String())
		}
		if err != nil {
			return err
		}
		tok, err = tx.CreateSandboxToken(r.Context(), hash, id, req.Label)
		if errors.Is(err, model.ErrNotFound) {
			return errUnknownSandbox(id.String())
		}
		return err
	})
	if err != nil {
		return passOr(err, "create token")
	}
	return writeJSON(w, http.StatusCreated, mintedToken{Token: token, HashPrefix: hash[:8], CreatedAt: rfc3339(tok.CreatedAt)})
}

// revokeSandboxToken is DELETE /api/v1/sandboxes/{id}/tokens/{hash}. The
// hash names the token; a hash of another sandbox's token is not revoked
// through this path (404 unknown token).
func (s *server) revokeSandboxToken(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	hash := r.PathValue("hash")
	tok, err := s.st.GetSandboxToken(r.Context(), hash)
	switch {
	case err == nil && tok.SandboxID != sb.ID:
		return apierr.New(http.StatusNotFound, "unknown token")
	case err != nil && !isNoRow(err):
		return fmt.Errorf("revoke token: %w", err)
	}
	ok, err := s.st.DeleteSandboxToken(r.Context(), sb.ID, hash)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return writeJSON(w, http.StatusOK, struct {
		Revoked bool `json:"revoked"`
	}{ok})
}

// emptyAccess is the access route's answer while the bindings are off.
var emptyAccess = json.RawMessage(`{"kube":null,"ssh":null,"denies":[]}`)

// access is GET /api/v1/sandboxes/{id}/access: the binding sets of the
// tenant's sandbox with their use; the empty object while bindings are
// off.
func (s *server) access(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.owned(r); err != nil {
		return err
	}
	if s.deps.Bindings == nil {
		return writeJSON(w, http.StatusOK, emptyAccess)
	}
	s.deps.Bindings.Access(w, r)
	return nil
}

// listTools is GET /api/v1/tools: the tool baseline, which MCP serves as
// tools/list, and the preparation budget, after which a client that waits
// for a tool call's sandbox gives up.
func (s *server) listTools(w http.ResponseWriter, _ *http.Request) error {
	type toolInfo struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	out := struct {
		Tools             []toolInfo `json:"tools"`
		PreparationWaitMs int64      `json:"preparationWaitMs"`
	}{Tools: []toolInfo{}, PreparationWaitMs: s.cfg.PreparationWaitMs}
	if s.deps.Specs != nil {
		for _, t := range s.deps.Specs() {
			out.Tools = append(out.Tools, toolInfo{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

// passOr returns an *apierr.Error of err as it is, and anything else as a
// 500 "<what>: <err>".
func passOr(err error, what string) error {
	var e *apierr.Error
	if errors.As(err, &e) {
		return e
	}
	return internalf("%s: %v", what, err)
}
