// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/apierr"
	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/env/ctl"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// rfc3339 is a time as the answers write it: RFC 3339 in seconds, UTC;
// "" for none.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseUUID parses a UUID in its hyphenated form, in either case.
func parseUUID(s string) (uuid.UUID, bool) {
	if len(s) != 36 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	return id, err == nil
}

// resolveEnv reads the environment of tenant that ref names (its UUID or
// its name) and locks its row with lock; an environment the tenant does
// not have, foreign or absent, is errNoEnvironment.
func resolveEnv(ctx context.Context, st *store.Store, tenant, ref string, lock store.RowLock) (store.Environment, error) {
	var e store.Environment
	var err error
	if id, ok := parseUUID(ref); ok {
		if e, err = st.GetEnvironment(ctx, id); err == nil && e.Owner == tenant && lock != store.NoLock {
			e, err = st.LockEnvironment(ctx, id, lock)
		}
	} else {
		if !spec.IsName(ref) {
			return store.Environment{}, errNoEnvironment()
		}
		e, err = st.LockEnvironmentByName(ctx, tenant, ref, lock)
	}
	if errors.Is(err, store.ErrNoRow) || (err == nil && e.Owner != tenant) {
		return store.Environment{}, errNoEnvironment()
	}
	if err != nil {
		return store.Environment{}, fmt.Errorf("environment %s: %w", ref, err)
	}
	return e, nil
}

// lockTenant takes the tenant's row FOR SHARE and refuses a deleting
// one: what an environment's create, release and secret need.
func lockTenant(ctx context.Context, tx *store.Store, tenant string) error {
	t, err := tx.LockTenant(ctx, tenant, store.ForShare)
	if errors.Is(err, store.ErrNoRow) {
		return apierr.New(http.StatusUnauthorized, "unknown tenant token")
	}
	if err != nil {
		return fmt.Errorf("tenant %s: %w", tenant, err)
	}
	if t.Deleting() {
		return apierr.New(http.StatusConflict, "tenant %s is being deleted", tenant)
	}
	return nil
}

// wake asks the controller to act on the environments: inside the
// writing transaction.
func wake(ctx context.Context, tx *store.Store, hints ...string) error {
	if len(hints) == 0 {
		hints = []string{ctl.HintReconcile}
	}
	for _, h := range hints {
		if err := db.Notify(ctx, tx.Q(), db.ChanLeader, h); err != nil {
			return fmt.Errorf("notify %s: %w", h, err)
		}
	}
	return nil
}

// parseStored reads a stored (normalised) spec.
func parseStored(raw json.RawMessage) (*spec.Spec, error) {
	var sp spec.Spec
	if err := json.Unmarshal(raw, &sp); err != nil {
		return nil, fmt.Errorf("a stored spec: %w", err)
	}
	return &sp, nil
}

// releaseSpec returns release number's parsed spec, for reading only
// (cached: a release never changes); errNoRelease when there is none.
func (s *Service) releaseSpec(ctx context.Context, envID uuid.UUID, number int) (*spec.Spec, error) {
	k := releaseKey{envID, number}
	if sp, ok := s.specs.get(k); ok {
		return sp, nil
	}
	r, err := s.st.GetRelease(ctx, envID, number)
	if errors.Is(err, store.ErrNoRow) {
		return nil, errNoRelease()
	}
	if err != nil {
		return nil, fmt.Errorf("release %d: %w", number, err)
	}
	sp, err := parseStored(r.Spec)
	if err != nil {
		return nil, fmt.Errorf("release %d: %w", number, err)
	}
	s.specs.put(k, sp)
	return sp, nil
}

// releaseInfo is a release as the answers show it.
func releaseInfo(r store.EnvRelease, tags []string) ReleaseInfo {
	if tags == nil {
		tags = []string{}
	}
	ann := r.Annotations
	if ann == nil {
		ann = map[string]string{}
	}
	return ReleaseInfo{Number: r.Number, State: r.State, Origin: r.Origin, OriginRef: r.OriginRef, Author: r.Author,
		Message: r.Message, Annotations: ann, Base: r.Base, Reason: r.Reason, CreatedAt: rfc3339(r.CreatedAt),
		StartedAt: rfc3339(r.StartedAt), FinishedAt: rfc3339(r.FinishedAt), Tags: tags}
}

// tagsByRelease are the environment's tags by the release they name.
func (s *Service) tagsByRelease(ctx context.Context, envID uuid.UUID) (map[int][]string, error) {
	tags, err := s.st.ListTags(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	out := map[int][]string{}
	for _, t := range tags {
		out[t.Number] = append(out[t.Number], t.Tag)
	}
	return out, nil
}

// nonNilDiff is d, or an empty diff.
func nonNilDiff(d []spec.DiffEntry) []spec.DiffEntry {
	if d == nil {
		return []spec.DiffEntry{}
	}
	return d
}
