// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// Run is a tenant's run (runs, ADR 0018): one execution of an image to
// its end. Its pod is the sandbox row of the same ID (tenant_run) while
// it has not ended. Its sealed secrets are read apart (RunSealed).
type Run struct {
	ID    podid.ID
	Owner string
	// Key is the create's Idempotency-Key; "": none.
	Key string
	// RequestHash is the create's body's hash.
	RequestHash string
	// Image is as asked, ImageRef pinned to its digest.
	Image, ImageRef  string
	VCPUs, MemoryMiB int
	ResultPath       string
	Labels           map[string]string
	Phase            string
	Reason, Detail   string
	ExitCode         *int
	Result           string
	LogTail          []string
	CreatedAt        time.Time
	StartedAt        time.Time // zero: NULL
	FinishedAt       time.Time // zero: NULL, the run has not ended
	DeadlineAt       time.Time
}

// Ended reports whether the run has ended.
func (r *Run) Ended() bool { return !r.FinishedAt.IsZero() }

const runCols = `id, owner, coalesce(idempotency_key, ''), request_hash, image, image_ref, vcpus, memory_mib,
	result_path, labels, phase, reason, detail, exit_code, result, log_tail, created_at, started_at, finished_at,
	deadline_at`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	var started, finished *time.Time
	if err := row.Scan(&r.ID, &r.Owner, &r.Key, &r.RequestHash, &r.Image, &r.ImageRef, &r.VCPUs, &r.MemoryMiB,
		&r.ResultPath, &r.Labels, &r.Phase, &r.Reason, &r.Detail, &r.ExitCode, &r.Result, &r.LogTail, &r.CreatedAt,
		&started, &finished, &r.DeadlineAt); err != nil {
		return Run{}, err
	}
	r.StartedAt, r.FinishedAt = timeOf(started), timeOf(finished)
	r.Labels, r.LogTail = stringMap(r.Labels), nonNil(r.LogTail)
	return r, nil
}

func collectRuns(rows pgx.Rows, err error) ([]Run, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
}

// InsertRun inserts a queued run, due deadline after the database's now,
// with its sealed secrets (nil: none). A key another run of the owner
// holds is ErrExists.
func (s *Store) InsertRun(ctx context.Context, r Run, sealed []byte, deadline time.Duration) (Run, error) {
	labels, err := jsonText(stringMap(r.Labels))
	if err != nil {
		return Run{}, err
	}
	out, err := scanRun(s.q.QueryRow(ctx, `INSERT INTO runs (id, owner, idempotency_key, request_hash, image, image_ref,
			vcpus, memory_mib, result_path, labels, sealed, deadline_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, now() + $12::interval) RETURNING `+runCols,
		r.ID, r.Owner, nullString(r.Key), r.RequestHash, r.Image, r.ImageRef, r.VCPUs, r.MemoryMiB, r.ResultPath, labels,
		sealed, interval(deadline)))
	if err != nil {
		return Run{}, insertError("insert run "+r.ID.String(), err)
	}
	return out, nil
}

// GetRun reads a run of owner (any owner for ""); ErrNoRow if there is
// none.
func (s *Store) GetRun(ctx context.Context, owner string, id podid.ID) (Run, error) {
	r, err := scanRun(s.q.QueryRow(ctx, "SELECT "+runCols+" FROM runs WHERE id = $1 AND ($2 = '' OR owner = $2)",
		id, owner))
	return r, noRow(err, ErrNoRow)
}

// RunByKey reads the run of owner that a create with key made; ErrNoRow
// if there is none.
func (s *Store) RunByKey(ctx context.Context, owner, key string) (Run, error) {
	r, err := scanRun(s.q.QueryRow(ctx, "SELECT "+runCols+" FROM runs WHERE owner = $1 AND idempotency_key = $2",
		owner, key))
	return r, noRow(err, ErrNoRow)
}

// ListRuns returns the runs of owner that carry every label of labels,
// newest first.
func (s *Store) ListRuns(ctx context.Context, owner string, labels map[string]string) ([]Run, error) {
	want, err := jsonText(stringMap(labels))
	if err != nil {
		return nil, err
	}
	return collectRuns(s.q.Query(ctx, "SELECT "+runCols+` FROM runs WHERE owner = $1 AND labels @> $2::jsonb
		ORDER BY created_at DESC, id DESC`, owner, want))
}

// OpenRuns returns every run that has not ended, oldest first.
func (s *Store) OpenRuns(ctx context.Context) ([]Run, error) {
	return collectRuns(s.q.Query(ctx, "SELECT "+runCols+" FROM runs WHERE finished_at IS NULL ORDER BY created_at, id"))
}

// RunSealed reads a run's sealed secrets: nil once it has ended or when
// it has none; ErrNoRow if there is no run.
func (s *Store) RunSealed(ctx context.Context, id podid.ID) ([]byte, error) {
	var sealed []byte
	err := s.q.QueryRow(ctx, "SELECT sealed FROM runs WHERE id = $1", id).Scan(&sealed)
	return sealed, noRow(err, ErrNoRow)
}

// StartRun marks a queued run running, started now; it reports whether
// it did.
func (s *Store) StartRun(ctx context.Context, id podid.ID) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE runs SET phase = $2, started_at = now()
		WHERE id = $1 AND phase = $3`, id, model.RunRunning, model.RunQueued)
	return tag.RowsAffected() > 0, err
}

// RunEnd is how a run ended.
type RunEnd struct {
	Phase, Reason, Detail string
	ExitCode              *int
	Result                string
	LogTail               []string
	// Exited: its process ran and exited; a run not seen running before
	// gets its start now.
	Exited bool
}

// EndRun ends a run that has not ended, now, and drops its sealed
// secrets. It reports whether it did: false when the run has ended
// already, or is gone.
func (s *Store) EndRun(ctx context.Context, id podid.ID, e RunEnd) (bool, error) {
	switch e.Phase {
	case model.RunSucceeded, model.RunFailed, model.RunDeadlineExceeded, model.RunCancelled:
	default:
		return false, fmt.Errorf("end run %s: %q is no end", id, e.Phase)
	}
	tail, err := jsonText(nonNil(e.LogTail))
	if err != nil {
		return false, err
	}
	tag, err := s.q.Exec(ctx, `UPDATE runs SET phase = $2, reason = $3, detail = $4, exit_code = $5, result = $6,
			log_tail = $7::jsonb, finished_at = now(), sealed = NULL,
			started_at = CASE WHEN $8 THEN coalesce(started_at, now()) ELSE started_at END
		WHERE id = $1 AND finished_at IS NULL`, id, e.Phase, e.Reason, e.Detail, nullInt(e.ExitCode), e.Result, tail,
		e.Exited)
	return tag.RowsAffected() > 0, err
}

// DeleteRun removes a run of owner; it reports whether there was one.
func (s *Store) DeleteRun(ctx context.Context, owner string, id podid.ID) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM runs WHERE id = $1 AND owner = $2", id, owner)
	return tag.RowsAffected() > 0, err
}

// SweepRuns removes the runs that ended more than retention ago, by the
// database's clock, and returns how many.
func (s *Store) SweepRuns(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM runs WHERE finished_at < now() - $1::interval", interval(retention))
	return tag.RowsAffected(), err
}

// LeftRunPods returns the runs' pods (tenant_run) whose run has ended or
// is gone: a run's end deletes its pod, and these are the deletes that
// did not happen.
func (s *Store) LeftRunPods(ctx context.Context) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxColumns("s")+` FROM sandboxes s
		LEFT JOIN runs r ON r.id = s.id WHERE s.tenant_run AND (r.id IS NULL OR r.finished_at IS NOT NULL)
		ORDER BY s.id`))
}
