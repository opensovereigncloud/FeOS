// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	otlplogsv1 "github.com/gardenlinux/feos/go/gen/otlp/collector/logs/v1"
	"github.com/gardenlinux/feos/go/internal/ingress/sinklog"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// The access log's bounds and timings.
const (
	// accessLogBuffer is the most records the agent keeps unsent: about
	// 1.5 KiB each at the fields' bounds, at most 15 MiB, and some 20 s
	// of a busy instance's requests. Beyond it a record is dropped and
	// counted, as are those of a batch the sink refuses.
	accessLogBuffer = 10000
	// accessLogEvery is how often the pending records are sent; a full
	// batch goes at once.
	accessLogEvery = time.Second
	// sinkRetryMin and sinkRetryMax bound the backoff of a batch the
	// sink failed to take.
	sinkRetryMin = time.Second
	sinkRetryMax = 30 * time.Second
)

// accessLog is the agent's buffer of the access log, which goes to the
// sink (Config.SinkEndpoint, ADR 0026 §8): the records not sent yet, and
// held, the batch on its way there, its records out of pending and
// numbered in their streams; it is sent until the sink takes or refuses
// it.
type accessLog struct {
	kick chan struct{}
	// send serializes the sends of the loop and of FlushAccessLog; the
	// sink's streams, held and retry are the sender's.
	send    sync.Mutex
	streams *sinklog.Streams
	held    *otlplogsv1.ExportLogsServiceRequest
	retry   *backoff

	mu      sync.Mutex
	pending []wire.AccessLogEntry
	// heldN counts the records of held.
	heldN int
	// sent and dropped count records for the metrics: those the sink
	// took, and those dropped.
	sent, dropped uint64
}

// newAccessLog returns the buffer of the instance name, whose streams
// are of this run.
func newAccessLog(name string) *accessLog {
	return &accessLog{kick: make(chan struct{}, 1), streams: sinklog.NewStreams(name, uuid.NewString()),
		retry: newBackoff(sinkRetryMin, sinkRetryMax)}
}

// Record keeps a record of the proxy's for the sink (the proxy's
// AccessLog). It never blocks: without a sink, or when accessLogBuffer
// records wait already, the record is dropped and counted.
func (a *Agent) Record(e wire.AccessLogEntry) {
	l := a.alog
	l.mu.Lock()
	if a.sink == nil || len(l.pending)+l.heldN >= accessLogBuffer {
		l.dropped++
		l.mu.Unlock()
		return
	}
	l.pending = append(l.pending, e)
	full := len(l.pending) >= sinklog.MaxRecords
	l.mu.Unlock()
	if full {
		select {
		case l.kick <- struct{}{}:
		default:
		}
	}
}

// AccessLogCounts returns the records the sink took, those dropped, and
// those waiting.
func (a *Agent) AccessLogCounts() (sent, dropped uint64, pending int) {
	l := a.alog
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sent, l.dropped, len(l.pending) + l.heldN
}

// accessLogLoop sends the pending records to the sink every
// accessLogEvery, and at once when a batch is full.
func (a *Agent) accessLogLoop(ctx context.Context) {
	t := time.NewTicker(accessLogEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.alog.kick:
		}
		if wait := a.sendToSink(ctx); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}

// errAccessLogUnsent ends a FlushAccessLog whose send failed.
var errAccessLogUnsent = errors.New("ingress agent: the access log was not sent")

// FlushAccessLog sends what waits to the sink, a batch after another,
// until nothing does, ctx ends, or a send fails; the program calls it
// while it shuts down. Without a sink nothing waits.
func (a *Agent) FlushAccessLog(ctx context.Context) error {
	for ctx.Err() == nil {
		_, _, pending := a.AccessLogCounts()
		if pending == 0 {
			return nil
		}
		if a.identity() == nil {
			return ErrNotConnected
		}
		if wait := a.sendToSink(ctx); wait > 0 {
			return errAccessLogUnsent
		}
	}
	return ctx.Err()
}

// take removes the first n pending records.
func (l *accessLog) take(n int) {
	l.pending = append(l.pending[:0], l.pending[n:]...)
}

// sendToSink sends batches to the sink until fewer than a batch wait, and
// returns how long to wait before the next send: 0 when it may go on as
// usual. Without a certificate the records wait. A batch the sink failed
// to take is sent again after a backoff, the same records under the same
// offsets, while new records wait in the buffer; one it refuses as
// invalid or over the tenant's quota is dropped and counted.
func (a *Agent) sendToSink(ctx context.Context) time.Duration {
	l := a.alog
	l.send.Lock()
	defer l.send.Unlock()
	for {
		if a.identity() == nil {
			return 0
		}
		if l.held == nil {
			l.mu.Lock()
			if len(l.pending) == 0 {
				l.mu.Unlock()
				return 0
			}
			recs := slices.Clone(l.pending[:min(len(l.pending), sinklog.MaxRecords)])
			l.mu.Unlock()
			req, taken, skipped := l.streams.Batch(recs)
			l.mu.Lock()
			l.take(taken)
			l.dropped += uint64(skipped)
			l.held, l.heldN = req, taken-skipped
			l.mu.Unlock()
			if req == nil {
				continue
			}
		}

		cctx, cancel := context.WithTimeout(ctx, a.tune.callTimeout)
		err := a.sink.Export(cctx, l.held)
		cancel()
		code := status.Code(err)
		drop := code == codes.InvalidArgument || code == codes.ResourceExhausted
		if err != nil && !drop {
			if ctx.Err() == nil {
				a.rl.warn("access-log", "ingress agent: sending the access log to the sink failed; trying again", "err", err)
			}
			return l.retry.delay()
		}
		l.retry.reset()
		l.mu.Lock()
		if err == nil {
			l.sent += uint64(l.heldN)
		} else {
			l.dropped += uint64(l.heldN)
		}
		n := l.heldN
		l.held, l.heldN = nil, 0
		more := len(l.pending) >= sinklog.MaxRecords
		l.mu.Unlock()
		if drop {
			a.rl.warn("access-log", "ingress agent: the sink refused a batch of the access log; dropped", "records", n, "err", err)
		}
		if !more {
			return 0
		}
	}
}
