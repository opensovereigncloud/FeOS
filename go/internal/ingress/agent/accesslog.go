// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// The access log's bounds and timings.
const (
	// accessLogBuffer is the most records the agent keeps unsent: about
	// 1.5 KiB each at the fields' bounds, at most 15 MiB, and some 20 s
	// of a busy instance's requests. Beyond it a record is dropped and
	// counted, as are those of a batch the control plane refuses.
	accessLogBuffer = 10000
	// accessLogEvery is how often the pending records are sent; a full
	// batch goes at once.
	accessLogEvery = time.Second
	// accessLogUnsupported is how long the agent waits after a control
	// plane that does not take access logs (UNIMPLEMENTED) before it
	// sends again, dropping what it holds.
	accessLogUnsupported = time.Minute
)

// accessLog is the agent's buffer of the access log: the records not
// sent yet, numbered from seq in the run start, and the count of those
// dropped since the last batch the control plane took.
type accessLog struct {
	start string
	kick  chan struct{}
	// send serializes the sends of the loop and of FlushAccessLog.
	send sync.Mutex

	mu      sync.Mutex
	pending []wire.AccessLogEntry
	seq     int64
	missed  int64
	// sent and dropped count records for the metrics: those the control
	// plane took, and those dropped.
	sent, dropped uint64
}

func newAccessLog() *accessLog {
	return &accessLog{start: uuid.NewString(), kick: make(chan struct{}, 1)}
}

// Record keeps a record of the proxy's for the control plane (the
// proxy's AccessLog). It never blocks: when accessLogBuffer records wait
// already, the record is dropped and counted.
func (a *Agent) Record(e wire.AccessLogEntry) {
	l := a.alog
	l.mu.Lock()
	if len(l.pending) >= accessLogBuffer {
		l.missed++
		l.dropped++
		l.mu.Unlock()
		return
	}
	l.pending = append(l.pending, e)
	full := len(l.pending) >= wire.MaxAccessLogRecords
	l.mu.Unlock()
	if full {
		select {
		case l.kick <- struct{}{}:
		default:
		}
	}
}

// AccessLogCounts returns the records the control plane took, those
// dropped, and those waiting.
func (a *Agent) AccessLogCounts() (sent, dropped uint64, pending int) {
	l := a.alog
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sent, l.dropped, len(l.pending)
}

// accessLogLoop sends the pending records every accessLogEvery, and at
// once when a batch is full, while the agent is connected.
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
		if wait := a.sendAccessLog(ctx); wait > 0 {
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

// FlushAccessLog sends what waits, a batch after another, until nothing
// does, ctx ends, or a send fails; the program calls it while it shuts
// down.
func (a *Agent) FlushAccessLog(ctx context.Context) error {
	for ctx.Err() == nil {
		_, _, pending := a.AccessLogCounts()
		if pending == 0 {
			return nil
		}
		if a.current() == nil {
			return ErrNotConnected
		}
		if wait := a.sendAccessLog(ctx); wait > 0 {
			return errAccessLogUnsent
		}
	}
	return ctx.Err()
}

// sendAccessLog sends batches of the pending records until fewer than a
// batch wait, and returns how long to wait before the next send: 0 when
// it may go on as usual. Without a connection the records wait. A batch
// the control plane refuses as invalid is dropped and counted; one it
// failed to store waits for the next send; a control plane without the
// method has every pending record dropped.
func (a *Agent) sendAccessLog(ctx context.Context) time.Duration {
	l := a.alog
	l.send.Lock()
	defer l.send.Unlock()
	for {
		s := a.current()
		if s == nil {
			return 0
		}
		l.mu.Lock()
		if len(l.pending) == 0 {
			l.mu.Unlock()
			return 0
		}
		n := min(len(l.pending), wire.MaxAccessLogRecords)
		batch := &wire.AccessLogBatch{Start: l.start, Seq: l.seq, Missed: l.missed,
			Records: append([]wire.AccessLogEntry(nil), l.pending[:n]...)}
		l.mu.Unlock()

		cctx, cancel := context.WithTimeout(ctx, a.tune.callTimeout)
		_, err := s.client.PublishAccessLogs(cctx, batch)
		cancel()

		l.mu.Lock()
		code := status.Code(err)
		switch {
		case err == nil:
			l.take(n)
			l.missed -= batch.Missed
			l.sent += uint64(n)
		case code == codes.InvalidArgument:
			l.take(n)
			l.missed += int64(n)
			l.dropped += uint64(n)
		case code == codes.Unimplemented:
			m := len(l.pending)
			l.take(m)
			l.missed += int64(m)
			l.dropped += uint64(m)
		}
		more := len(l.pending) >= wire.MaxAccessLogRecords
		l.mu.Unlock()
		switch {
		case err == nil:
		case code == codes.Unimplemented:
			a.rl.warn("access-log", "ingress agent: the control plane takes no access log; dropping it", "err", err)
			return accessLogUnsupported
		case code == codes.InvalidArgument:
			a.rl.warn("access-log", "ingress agent: the control plane refused a batch of the access log; dropped", "records", n, "err", err)
		default:
			a.rl.warn("access-log", "ingress agent: sending the access log failed; trying again", "err", err)
			return accessLogEvery
		}
		if !more {
			return 0
		}
	}
}

// take removes the first n pending records, which advances seq: the next
// batch's first record is the next number.
func (l *accessLog) take(n int) {
	l.pending = append(l.pending[:0], l.pending[n:]...)
	l.seq += int64(n)
}
