// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/store"
)

// accessLogTimeout bounds the insert of one batch of the access log.
const accessLogTimeout = 10 * time.Second

// PublishAccessLogs stores a batch of the access log of the ingress of
// the connection's certificate (ADR 0021): each record of an environment
// that exists, attributed to its owner, once per (ingress, start, seq).
// A record whose environment is not an ID is skipped; text fields are cut
// to the wire's bounds. More than wire.MaxAccessLogRecords records, or a
// start that is not a UUID, is INVALID_ARGUMENT; a database error is
// UNAVAILABLE, and the ingress sends the batch again. While the instance
// stops it answers UNAVAILABLE.
func (s *Service) PublishAccessLogs(ctx context.Context, b *wire.AccessLogBatch) (*wire.AccessLogAck, error) {
	p, err := ingressPeer(ctx)
	if err != nil {
		return nil, err
	}
	if s.stopping.Load() {
		return nil, errStopping()
	}
	if n := len(b.Records); n > wire.MaxAccessLogRecords {
		return nil, status.Errorf(codes.InvalidArgument, "the batch holds %d records, more than %d", n, wire.MaxAccessLogRecords)
	}
	start, err := uuid.Parse(b.Start)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "start %q is not a UUID", b.Start)
	}
	if b.Seq < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "seq %d is negative", b.Seq)
	}
	if b.Missed > 0 {
		s.rl.warn("access-log-missed", "ingress: an ingress dropped records of its access log",
			"ingress", p.Name, "missed", b.Missed)
	}
	rows := make([]store.AccessLogRow, 0, len(b.Records))
	for i, e := range b.Records {
		env, err := uuid.Parse(e.Env)
		if err != nil {
			continue
		}
		rows = append(rows, store.AccessLogRow{
			TsNs: e.Time.UnixNano(), Ingress: p.Name, StartID: start, Seq: b.Seq + int64(i), EnvID: env,
			Service: field(e.Service), Instance: field(e.Instance), Host: field(e.Host),
			Method: field(e.Method), Path: bound(e.Path, wire.MaxAccessLogPath), Proto: field(e.Proto),
			Status: e.Status, Outcome: field(e.Outcome),
			DurationNs: max(e.DurationNs, 0), HeldNs: max(e.HeldNs, 0), Attempts: max(e.Attempts, 0),
			BytesIn: max(e.BytesIn, 0), BytesOut: max(e.BytesOut, 0),
			ClientIP: field(e.ClientIP), UserAgent: bound(e.UserAgent, wire.MaxAccessLogUserAgent),
			TraceID: field(e.TraceID), SpanID: field(e.SpanID), ParentSpanID: field(e.ParentSpanID),
		})
	}
	ictx, cancel := context.WithTimeout(ctx, accessLogTimeout)
	defer cancel()
	if _, err := s.st.InsertAccessLogs(ictx, rows...); err != nil {
		return nil, dbError(ctx, err, "store the access log")
	}
	return &wire.AccessLogAck{}, nil
}

// field bounds a text field of a record to wire.MaxAccessLogText.
func field(s string) string { return bound(s, wire.MaxAccessLogText) }

// bound cuts s to n bytes at a rune's start and replaces what is not
// UTF-8, which PostgreSQL's text refuses; NUL, which it refuses too, is
// dropped.
func bound(s string, n int) string {
	if len(s) > n {
		i := n
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		s = s[:i]
	}
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}
