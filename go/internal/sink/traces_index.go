// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// traceClaimSpans bounds the spans one round of the trace indexer
// claims; indexBatchBytes the bytes of their documents (at least one).
const traceClaimSpans = 5000

// Run runs indexWorkers loops that move spans into the trace index, each
// again at once after a round that moved some, else after ChunkInterval,
// and the sweep of old usage every SweepInterval, until ctx ends.
func (t *Traces) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	loop := func(name string, round func(context.Context) (int, error), wait time.Duration) {
		defer wg.Done()
		for {
			n, err := round(ctx)
			if err != nil && ctx.Err() == nil {
				t.log.Error(name, "err", err)
			}
			d := wait
			if n > 0 && err == nil {
				d = 0
			}
			tm := time.NewTimer(d)
			select {
			case <-ctx.Done():
				tm.Stop()
				return
			case <-tm.C:
			}
		}
	}
	for range indexWorkers {
		wg.Add(1)
		go loop("trace indexer", t.indexRound, t.cfg.ChunkInterval)
	}
	wg.Add(1)
	go loop("trace usage sweep", t.sweepUsage, t.cfg.SweepInterval)
	wg.Wait()
	return nil
}

// indexRound claims spans whose claim is free or expired (FOR UPDATE SKIP
// LOCKED, a lease of claimLease of the database's clock), at most
// traceClaimSpans or indexBatchBytes of documents, ingests them into the
// trace index with commit=wait_for and then deletes them, those whose
// claim is still its own. A round that failed leaves its rows to the next
// claim after the lease; spans indexed twice are dropped by the reads. It
// returns how many spans it moved.
func (t *Traces) indexRound(ctx context.Context) (int, error) {
	rows, err := t.db.Pool.Query(ctx, `WITH c AS (SELECT id, bytes FROM trace_spans
			WHERE claim_lease IS NULL OR claim_lease < now() ORDER BY id LIMIT $3 FOR UPDATE SKIP LOCKED),
		w AS (SELECT id, sum(bytes) OVER (ORDER BY id) AS cum, row_number() OVER (ORDER BY id) AS n FROM c)
		UPDATE trace_spans s SET claim_owner = $1, claim_lease = now() + make_interval(secs => $2)
		FROM w WHERE s.id = w.id AND (w.cum <= $4 OR w.n = 1)
		RETURNING s.id, s.doc::text`, t.owner, claimLease.Seconds(), traceClaimSpans, indexBatchBytes)
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	var ids []int64
	var docs []json.RawMessage
	for rows.Next() {
		var id int64
		var doc string
		if err := rows.Scan(&id, &doc); err != nil {
			rows.Close()
			return 0, fmt.Errorf("claim: %w", err)
		}
		ids, docs = append(ids, id), append(docs, json.RawMessage(doc))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	id, days := t.indexID()
	if !t.ensured.Load() {
		if err := t.index.EnsureTraceIndex(ctx, id, days, int(t.cfg.CommitTimeout/time.Second)); err != nil {
			return 0, fmt.Errorf("index %s: %w", id, err)
		}
		t.ensured.Store(true)
	}
	ictx, cancel := context.WithTimeout(ctx, claimLease-30*time.Second)
	err = t.index.IngestSpans(ictx, id, docs)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("index %d spans into %s: %w", len(docs), id, err)
	}
	tag, err := t.db.Pool.Exec(ctx, `DELETE FROM trace_spans WHERE id = ANY($1) AND claim_owner = $2`, ids, t.owner)
	if err != nil {
		return 0, fmt.Errorf("delete indexed spans: %w", err)
	}
	if n := tag.RowsAffected(); n < int64(len(ids)) {
		t.log.Warn("lost spans' claims to another instance", "spans", int64(len(ids))-n)
	}
	return len(ids), nil
}

// sweepUsage deletes the trace usage of the days before yesterday.
func (t *Traces) sweepUsage(ctx context.Context) (int, error) {
	if _, err := t.db.Pool.Exec(ctx, `DELETE FROM trace_usage WHERE day < (now() AT TIME ZONE 'utc')::date - 1`); err != nil {
		return 0, fmt.Errorf("usage: %w", err)
	}
	return 0, nil
}

// hotSpans reads the spans of q not yet in the index: a trace's by
// (tenant, trace ID), else the tenant's since q.SinceNs, newest first; at
// most limit rows, those that pass q.
func (t *Traces) hotSpans(ctx context.Context, q quickwit.SpanQuery, limit int) ([]quickwit.SpanDoc, error) {
	var rows pgx.Rows
	var err error
	if q.TraceID != "" {
		rows, err = t.db.Pool.Query(ctx, `SELECT doc::text FROM trace_spans WHERE tenant = $1 AND trace_id = $2 LIMIT $3`,
			q.Tenant, q.TraceID, limit)
	} else {
		rows, err = t.db.Pool.Query(ctx, `SELECT doc::text FROM trace_spans WHERE tenant = $1 AND start_ns >= $2
			ORDER BY start_ns DESC LIMIT $3`, q.Tenant, q.SinceNs, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("hot spans: %w", err)
	}
	defer rows.Close()
	var out []quickwit.SpanDoc
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, fmt.Errorf("hot spans: %w", err)
		}
		var d quickwit.SpanDoc
		if err := json.Unmarshal([]byte(doc), &d); err != nil {
			return nil, fmt.Errorf("hot span: %w", err)
		}
		if q.Match(&d) {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}
