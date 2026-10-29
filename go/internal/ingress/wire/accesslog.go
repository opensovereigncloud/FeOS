// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import "time"

// The bounds of PublishAccessLogs. The control plane refuses a batch
// beyond them with INVALID_ARGUMENT; the ingress cuts fields to them
// before it records a request.
const (
	// MaxAccessLogRecords is the most records of one batch.
	MaxAccessLogRecords = 500
	// MaxAccessLogPath bounds Path, MaxAccessLogUserAgent UserAgent and
	// MaxAccessLogText every other text field, in bytes.
	MaxAccessLogPath      = 1024
	MaxAccessLogUserAgent = 256
	MaxAccessLogText      = 256
)

// AccessLogBatch is a part of an ingress's access log (ADR 0021): the
// records numbered Seq, Seq+1, … of the run Start (an ID the ingress
// draws when it starts), oldest first. Missed counts the records the
// ingress dropped since the last batch it sent, because its buffer was
// full or the control plane refused them. A batch sent again is the same
// batch: the control plane stores each (ingress, Start, Seq) once.
type AccessLogBatch struct {
	Start   string           `json:"start"`
	Seq     int64            `json:"seq"`
	Missed  int64            `json:"missed"`
	Records []AccessLogEntry `json:"records"`
}

// AccessLogEntry is one request the ingress answered for an environment,
// which is also the request's server span (W3C Trace Context): TraceID
// and SpanID name it, ParentSpanID the client's span when the request
// brought a traceparent. Path is the escaped path without the query.
// Outcome is how it ended: the status class ("2xx" … "5xx"),
// "upgraded", or "aborted". Instance is the instance the last attempt
// went to, "" when none; Attempts counts the attempts, HeldNs how long
// the request waited for an instance.
type AccessLogEntry struct {
	Time         time.Time `json:"time"`
	DurationNs   int64     `json:"durationNs"`
	Env          string    `json:"env"`
	Service      string    `json:"service"`
	Instance     string    `json:"instance"`
	Host         string    `json:"host"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Proto        string    `json:"proto"`
	Status       int       `json:"status"`
	Outcome      string    `json:"outcome"`
	BytesIn      int64     `json:"bytesIn"`
	BytesOut     int64     `json:"bytesOut"`
	ClientIP     string    `json:"clientIp"`
	UserAgent    string    `json:"userAgent"`
	Attempts     int       `json:"attempts"`
	HeldNs       int64     `json:"heldNs"`
	TraceID      string    `json:"traceId"`
	SpanID       string    `json:"spanId"`
	ParentSpanID string    `json:"parentSpanId"`
}

// AccessLogAck says the batch is stored.
type AccessLogAck struct{}
