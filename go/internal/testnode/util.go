// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// rateLog logs a kind of line at most once per second, with the number of
// lines of that kind it suppressed since: dial failures to an instance
// that is down, or a control plane that refuses, would flood the log.
type rateLog struct {
	log   *slog.Logger
	mu    sync.Mutex
	kinds map[string]*rateKind
}

type rateKind struct {
	last       time.Time
	suppressed int
}

func newRateLog(log *slog.Logger) *rateLog {
	return &rateLog{log: log, kinds: map[string]*rateKind{}}
}

func (r *rateLog) warn(kind, msg string, args ...any) {
	r.mu.Lock()
	k := r.kinds[kind]
	if k == nil {
		k = &rateKind{}
		r.kinds[kind] = k
	}
	now := time.Now()
	if now.Sub(k.last) < time.Second {
		k.suppressed++
		r.mu.Unlock()
		return
	}
	suppressed := k.suppressed
	k.last, k.suppressed = now, 0
	r.mu.Unlock()
	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	r.log.Warn(msg, args...)
}

// backoff is an exponential backoff with jitter, between min and max.
type backoff struct {
	min, max, next time.Duration
}

func newBackoff(min, max time.Duration) *backoff {
	return &backoff{min: min, max: max, next: min}
}

// wait sleeps for the next delay, or until ctx ends; it reports whether
// the delay passed.
func (b *backoff) wait(ctx context.Context) bool { return sleep(ctx, b.delay()) }

// delay is the next delay, ±20 %, so that nodes that lost an instance at
// once do not redial in step.
func (b *backoff) delay() time.Duration {
	d := b.next
	b.next = min(2*b.next, b.max)
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func (b *backoff) reset() { b.next = b.min }

// sleep waits for d or until ctx ends; it reports whether d passed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// watchedConn reports when it is closed, by either side: the gRPC server
// of a node connection closes it when the HTTP/2 connection ends.
type watchedConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func newWatchedConn(c net.Conn) *watchedConn {
	return &watchedConn{Conn: c, closed: make(chan struct{})}
}

func (w *watchedConn) Close() error {
	err := w.Conn.Close()
	w.once.Do(func() { close(w.closed) })
	return err
}

// oneConnListener hands one connection to a grpc.Server, which serves it
// as an HTTP/2 server; Accept blocks after it until Close.
type oneConnListener struct {
	c      chan net.Conn
	closed chan struct{}
	once   sync.Once
	addr   net.Addr
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{c: make(chan net.Conn, 1), closed: make(chan struct{}), addr: c.LocalAddr()}
	l.c <- c
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.c:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.addr }
