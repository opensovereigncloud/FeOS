// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"fmt"
	"time"

	"github.com/gardenlinux/feos/go/internal/store"
)

// The access log's page sizes.
const (
	accessLogDefaultLimit = 200
	accessLogMaxLimit     = 1000
)

// AccessLogQuery is a page of an environment's access log: at most Limit
// records (default 200, at most 1000) before BeforeTsNs (0: the newest),
// of the trace TraceID when it is set.
type AccessLogQuery struct {
	Limit      int
	BeforeTsNs int64
	TraceID    string
}

// AccessLogList is GET /api/v1/environments/{env}/access-logs: a page of
// the requests ferun-ingress answered for the environment, ascending by
// time. The next older page is the one before the first record's tsNs.
type AccessLogList struct {
	Records []AccessLogRecord `json:"records"`
}

// AccessLogRecord is one request and its server span at the ingress
// (ADR 0021). TsNs is its start in Unix nanoseconds, as a string; Path
// the escaped path without the query; Status 101 for an upgrade, 0 when
// none was written; Outcome the status class, "upgraded" or "aborted";
// Instance the instance of the last attempt, "" for none; HeldNs how long
// it waited for an instance. TraceID and SpanID name the span in W3C
// Trace Context, which the instance got in traceparent with SpanID as its
// parent; ParentSpanID is the client's span, "" when the request brought
// none.
type AccessLogRecord struct {
	TsNs         int64     `json:"tsNs,string"`
	Time         time.Time `json:"time"`
	DurationNs   int64     `json:"durationNs"`
	Ingress      string    `json:"ingress"`
	Service      string    `json:"service"`
	Instance     string    `json:"instance"`
	Host         string    `json:"host"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Proto        string    `json:"proto"`
	Status       int       `json:"status"`
	Outcome      string    `json:"outcome"`
	Attempts     int       `json:"attempts"`
	HeldNs       int64     `json:"heldNs"`
	BytesIn      int64     `json:"bytesIn"`
	BytesOut     int64     `json:"bytesOut"`
	ClientIP     string    `json:"clientIp"`
	UserAgent    string    `json:"userAgent"`
	TraceID      string    `json:"traceId"`
	SpanID       string    `json:"spanId"`
	ParentSpanID string    `json:"parentSpanId"`
}

// AccessLogs is GET /api/v1/environments/{env}/access-logs.
func (s *Service) AccessLogs(ctx context.Context, tenant, env string, q AccessLogQuery) (AccessLogList, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return AccessLogList{}, err
	}
	if q.Limit <= 0 {
		q.Limit = accessLogDefaultLimit
	}
	q.Limit = min(q.Limit, accessLogMaxLimit)
	rows, err := s.st.ListEnvAccessLogs(ctx, e.ID, store.AccessLogQuery{Limit: q.Limit, BeforeTsNs: q.BeforeTsNs, TraceID: q.TraceID})
	if err != nil {
		return AccessLogList{}, fmt.Errorf("access log: %w", err)
	}
	out := AccessLogList{Records: make([]AccessLogRecord, 0, len(rows))}
	for _, r := range rows {
		out.Records = append(out.Records, AccessLogRecord{
			TsNs: r.TsNs, Time: time.Unix(0, r.TsNs).UTC(), DurationNs: r.DurationNs, Ingress: r.Ingress,
			Service: r.Service, Instance: r.Instance, Host: r.Host, Method: r.Method, Path: r.Path, Proto: r.Proto,
			Status: r.Status, Outcome: r.Outcome, Attempts: r.Attempts, HeldNs: r.HeldNs,
			BytesIn: r.BytesIn, BytesOut: r.BytesOut, ClientIP: r.ClientIP, UserAgent: r.UserAgent,
			TraceID: r.TraceID, SpanID: r.SpanID, ParentSpanID: r.ParentSpanID,
		})
	}
	return out, nil
}
