// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// Run sweeps the trace usage of old days every SweepInterval, until ctx
// ends: the spans themselves go to their topic at ingest, and the trace
// index's Kafka source takes them from there (ADR 0026 §11).
func (t *Traces) Run(ctx context.Context) error {
	for {
		if err := t.sweepUsage(ctx); err != nil && ctx.Err() == nil {
			t.log.Error("trace usage sweep", "err", err)
		}
		tm := time.NewTimer(t.cfg.SweepInterval)
		select {
		case <-ctx.Done():
			tm.Stop()
			return nil
		case <-tm.C:
		}
	}
}

// sweepUsage deletes the trace usage of the days before yesterday.
func (t *Traces) sweepUsage(ctx context.Context) error {
	if _, err := t.db.Pool.Exec(ctx, `DELETE FROM trace_usage WHERE day < (now() AT TIME ZONE 'utc')::date - 1`); err != nil {
		return fmt.Errorf("usage: %w", err)
	}
	return nil
}

// ensure makes the trace index, its topic and its source, and returns the
// topic's id and partitions.
func (t *Traces) ensure(ctx context.Context) (string, int32, error) {
	id, days := t.indexID()
	n, err := t.pipes.ensure(ctx, id, func() []byte {
		return quickwit.TraceIndexConfig(id, quickwit.IndexSettings{RetentionDays: days,
			CommitSecs: traceCommitSecs, Partitions: t.cfg.IndexPartitions})
	})
	return id, n, err
}

// traceCommitSecs is the trace index's commit interval: a span is found
// by a search at most this long after it is stored.
const traceCommitSecs = 5

// spanRecord is a span as a record of the trace topic, keyed by its trace
// ID: a trace's spans lie in one partition of n.
func spanRecord(traceID string, raw []byte, n int32) kafka.Record {
	return kafka.Record{Partition: kafka.Partition(traceID, n), Key: []byte(traceID), Value: raw}
}

// tailSpans reads the spans of q not yet in the index: a trace's, by its
// ID, from its partition of the topic, from the source's checkpoint on;
// a search of spans reads the index alone (traces.md, Reading).
func (t *Traces) tailSpans(ctx context.Context, q quickwit.SpanQuery) ([]quickwit.SpanDoc, error) {
	if q.TraceID == "" {
		return nil, nil
	}
	id, n, err := t.ensure(ctx)
	if err != nil {
		return nil, err
	}
	recs, err := t.pipes.tail(ctx, id, []int32{kafka.Partition(q.TraceID, n)})
	if err != nil {
		return nil, err
	}
	var out []quickwit.SpanDoc
	for _, r := range recs {
		var d quickwit.SpanDoc
		if json.Unmarshal(r.Value, &d) == nil && q.Match(&d) {
			out = append(out, d)
		}
	}
	return out, nil
}
