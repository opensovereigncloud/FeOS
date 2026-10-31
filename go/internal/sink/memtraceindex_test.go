// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// memTraceIndex is the trace index in memory: what Quickwit answers for
// the queries the sink makes (SpanQuery.Match).
type memTraceIndex struct {
	mu         sync.Mutex
	docs       map[string][]quickwit.SpanDoc
	ingests    int
	failIngest error
}

func newMemTraceIndex() *memTraceIndex { return &memTraceIndex{docs: map[string][]quickwit.SpanDoc{}} }

func (m *memTraceIndex) EnsureTraceIndex(_ context.Context, id string, _ quickwit.IndexSettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.docs[id]; !ok {
		m.docs[id] = nil
	}
	return nil
}

func (m *memTraceIndex) IngestSpans(_ context.Context, id string, docs []json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failIngest != nil {
		return m.failIngest
	}
	if _, ok := m.docs[id]; !ok {
		return &quickwit.Error{Status: 404, Message: "index " + id + " not found"}
	}
	for _, raw := range docs {
		var d quickwit.SpanDoc
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		m.docs[id] = append(m.docs[id], d)
	}
	m.ingests++
	return nil
}

func (m *memTraceIndex) match(pattern string, q *quickwit.SpanQuery) []quickwit.SpanDoc {
	prefix := strings.TrimSuffix(pattern, "*")
	var out []quickwit.SpanDoc
	for id, docs := range m.docs {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		for i := range docs {
			if q.Match(&docs[i]) {
				out = append(out, docs[i])
			}
		}
	}
	return out
}

func (m *memTraceIndex) SearchSpans(_ context.Context, pattern string, q quickwit.SpanQuery) ([]quickwit.SpanDoc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.match(pattern, &q)
	slices.SortStableFunc(out, func(a, b quickwit.SpanDoc) int { return cmp.Compare(a.TS, b.TS) })
	if len(out) > q.Max {
		out = out[:q.Max]
	}
	return out, nil
}

func (m *memTraceIndex) SpanTerms(_ context.Context, pattern string, q quickwit.SpanQuery, field string, size int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, d := range m.match(pattern, &q) {
		switch field {
		case "service_name":
			out = append(out, d.ServiceName)
		case "span_name":
			out = append(out, d.SpanName)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	return out[:min(len(out), size)], nil
}

func (m *memTraceIndex) NewestTraces(_ context.Context, pattern string, q quickwit.SpanQuery, n int) ([]quickwit.TraceHit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	newest := map[string]int64{}
	for _, d := range m.match(pattern, &q) {
		if x, ok := newest[d.TraceID]; !ok || d.StartNs > x {
			newest[d.TraceID] = d.StartNs
		}
	}
	var out []quickwit.TraceHit
	for id, x := range newest {
		out = append(out, quickwit.TraceHit{TraceID: id, NewestNs: x})
	}
	slices.SortFunc(out, func(a, b quickwit.TraceHit) int { return cmp.Compare(b.NewestNs, a.NewestNs) })
	return out[:min(len(out), n)], nil
}

// count is how many spans the indexes hold.
func (m *memTraceIndex) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, docs := range m.docs {
		n += len(docs)
	}
	return n
}
