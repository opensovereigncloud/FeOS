// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AccessLogRow is a row of access_logs: a request ferun-ingress answered
// for an environment, and its server span (ADR 0021).
type AccessLogRow struct {
	Day                                          time.Time // set by the database
	ID                                           int64     // set by the database
	TsNs                                         int64     // the request's start
	Ingress                                      string
	StartID                                      uuid.UUID
	Seq                                          int64
	EnvID                                        uuid.UUID
	Owner                                        string // set by InsertAccessLogs from the environment
	Service, Instance, Host, Method, Path, Proto string
	Status                                       int
	Outcome                                      string
	DurationNs, HeldNs                           int64
	Attempts                                     int
	BytesIn, BytesOut                            int64
	ClientIP, UserAgent                          string
	TraceID, SpanID, ParentSpanID                string
}

const accessLogInsertCols = "ts_ns, ingress, start_id, seq, env_id, owner, service, instance, host, method, path, " +
	"proto, status, outcome, duration_ns, held_ns, attempts, bytes_in, bytes_out, client_ip, user_agent, " +
	"trace_id, span_id, parent_span_id"

const accessLogCols = "day, id, " + accessLogInsertCols

// insertAccessLogsSQL inserts the records given column by column, those
// of environments that exist, each with its environment's owner. Its
// text is the same for every batch size.
const insertAccessLogsSQL = `INSERT INTO access_logs (` + accessLogInsertCols + `)
	SELECT r.ts_ns, r.ingress, r.start_id, r.seq, e.id, e.owner, r.service, r.instance, r.host, r.method, r.path,
		r.proto, r.status, r.outcome, r.duration_ns, r.held_ns, r.attempts, r.bytes_in, r.bytes_out, r.client_ip,
		r.user_agent, r.trace_id, r.span_id, r.parent_span_id
	FROM unnest($1::bigint[], $2::text[], $3::uuid[], $4::bigint[], $5::uuid[], $6::text[], $7::text[], $8::text[],
		$9::text[], $10::text[], $11::text[], $12::integer[], $13::text[], $14::bigint[], $15::bigint[],
		$16::integer[], $17::bigint[], $18::bigint[], $19::text[], $20::text[], $21::text[], $22::text[], $23::text[])
		AS r(ts_ns, ingress, start_id, seq, env_id, service, instance, host, method, path, proto, status, outcome,
			duration_ns, held_ns, attempts, bytes_in, bytes_out, client_ip, user_agent, trace_id, span_id,
			parent_span_id)
	JOIN environments e ON e.id = r.env_id
	ON CONFLICT DO NOTHING`

// InsertAccessLogs stores the records, in statements of up to 500 rows,
// attributed to their environments' owners, and returns how many it
// stored. A record of an environment that does not exist, or one stored
// already (same ingress, start, seq and day), is skipped.
func (s *Store) InsertAccessLogs(ctx context.Context, rows ...AccessLogRow) (int, error) {
	stored := 0
	for chunk := range slices.Chunk(rows, activityBatch) {
		n := len(chunk)
		var (
			tsNs, seq, dur, held, in, out = make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
			startID, envID                = make([]uuid.UUID, n), make([]uuid.UUID, n)
			status, attempts              = make([]int32, n), make([]int32, n)
			ingress, service, instance    = make([]string, n), make([]string, n), make([]string, n)
			host, method, path, proto     = make([]string, n), make([]string, n), make([]string, n), make([]string, n)
			outcome, ip, ua               = make([]string, n), make([]string, n), make([]string, n)
			trace, span, parent           = make([]string, n), make([]string, n), make([]string, n)
		)
		for i, r := range chunk {
			tsNs[i], seq[i], dur[i], held[i], in[i], out[i] = r.TsNs, r.Seq, r.DurationNs, r.HeldNs, r.BytesIn, r.BytesOut
			startID[i], envID[i] = r.StartID, r.EnvID
			status[i], attempts[i] = int32(r.Status), int32(r.Attempts)
			ingress[i], service[i], instance[i] = r.Ingress, r.Service, r.Instance
			host[i], method[i], path[i], proto[i] = r.Host, r.Method, r.Path, r.Proto
			outcome[i], ip[i], ua[i] = r.Outcome, r.ClientIP, r.UserAgent
			trace[i], span[i], parent[i] = r.TraceID, r.SpanID, r.ParentSpanID
		}
		tag, err := s.q.Exec(ctx, insertAccessLogsSQL, tsNs, ingress, startID, seq, envID, service, instance, host,
			method, path, proto, status, outcome, dur, held, attempts, in, out, ip, ua, trace, span, parent)
		if err != nil {
			return stored, fmt.Errorf("insert access log records: %w", err)
		}
		stored += int(tag.RowsAffected())
	}
	return stored, nil
}

// AccessLogQuery is a page of an environment's access log: at most Limit
// records before BeforeTsNs (0: the newest), of the trace TraceID when
// it is set.
type AccessLogQuery struct {
	Limit      int
	BeforeTsNs int64
	TraceID    string
}

// ListEnvAccessLogs returns a page of an environment's access log, the
// newest records of q, ascending by time.
func (s *Store) ListEnvAccessLogs(ctx context.Context, envID uuid.UUID, q AccessLogQuery) ([]AccessLogRow, error) {
	where, args := "env_id = $1", []any{envID}
	if q.BeforeTsNs != 0 {
		args = append(args, q.BeforeTsNs)
		where += fmt.Sprintf(" AND ts_ns < $%d", len(args))
	}
	if q.TraceID != "" {
		args = append(args, q.TraceID)
		where += fmt.Sprintf(" AND trace_id = $%d", len(args))
	}
	args = append(args, q.Limit)
	rows, err := s.q.Query(ctx, "SELECT "+accessLogCols+" FROM access_logs WHERE "+where+
		fmt.Sprintf(" ORDER BY ts_ns DESC, id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("list the access log: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AccessLogRow, error) {
		var r AccessLogRow
		var status, attempts int32
		err := row.Scan(&r.Day, &r.ID, &r.TsNs, &r.Ingress, &r.StartID, &r.Seq, &r.EnvID, &r.Owner, &r.Service,
			&r.Instance, &r.Host, &r.Method, &r.Path, &r.Proto, &status, &r.Outcome, &r.DurationNs, &r.HeldNs,
			&attempts, &r.BytesIn, &r.BytesOut, &r.ClientIP, &r.UserAgent, &r.TraceID, &r.SpanID, &r.ParentSpanID)
		r.Status, r.Attempts = int(status), int(attempts)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("list the access log: %w", err)
	}
	slices.Reverse(out)
	return out, nil
}
