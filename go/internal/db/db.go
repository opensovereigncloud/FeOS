// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package db is ferund's access to PostgreSQL: the connection pool with the
// session settings every statement relies on, transactions that retry on
// serialization failures, deadlocks and lock timeouts, the embedded schema
// and its migration, the daily partitions of the activity tables, and the
// notification hub, through which every instance learns of another's writes
// (LISTEN/NOTIFY).
//
// Transactions run at READ COMMITTED with explicit row locks: every
// statement sees its own snapshot, so a recheck that must see what another
// transaction committed while this one waited for a lock is a separate,
// later statement. Nothing slow (a node call) ever runs inside a
// transaction.
package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier runs statements: a pgx.Tx, a *pgxpool.Pool or a *pgx.Conn.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// The notification channels. Every instance listens to all of them.
const (
	// ChanSandbox carries a sandbox id whose state, op, node, checkpoint or
	// existence changed, or "*": re-read every sandbox you wait on.
	ChanSandbox = "ferun_sandbox"
	// ChanNode carries a node name whose certificates, removal, identity,
	// stopping_until or draining changed.
	ChanNode = "ferun_node"
	// ChanLeader carries a hint that wakes the leader's loops.
	ChanLeader = "ferun_leader"
	// ChanWatch carries a watch id whose path set, owner or result changed.
	ChanWatch = "ferun_watch"
	// ChanCancel carries a JSON cancellation of an exec or an MCP call.
	ChanCancel = "ferun_cancel"
	// ChanReconcile carries a node name an instance asks another to
	// reconcile.
	ChanReconcile = "ferun_reconcile"
)

// Channels lists every channel the hub listens to.
var Channels = []string{ChanSandbox, ChanNode, ChanLeader, ChanWatch, ChanCancel, ChanReconcile}

// Session settings of every connection: a lock waited on for longer than
// lock_timeout fails the statement with 55P03 (Tx retries it), a statement
// running longer than statement_timeout is cancelled, and times are UTC.
const (
	sessionLockTimeout      = "5s"
	sessionStatementTimeout = "30s"
)

// defaultMaxConns is the pool size unless the URL names one
// (pool_max_conns).
const defaultMaxConns = 32

// DB is an open database: the pool, the notification hub and the record of
// the last connection failure.
type DB struct {
	Pool *pgxpool.Pool

	log *slog.Logger
	// listenConfig is the configuration of the hub's own connection.
	listenConfig *pgx.ConnConfig
	hub          hub
	// lastFailure is the UnixNano of the last connection failure, 0 if
	// none.
	lastFailure atomic.Int64

	listenMu  sync.Mutex
	listening chan struct{} // closed while the listener listens
	listenOK  bool
}

// Open connects the pool. Every connection gets the session settings
// lock_timeout = 5s, statement_timeout = 30s and TimeZone = UTC. Open does
// not migrate; see Migrate.
func Open(ctx context.Context, url string, log *slog.Logger) (*DB, error) {
	if log == nil {
		log = slog.Default()
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if !strings.Contains(url, "pool_max_conns") {
		cfg.MaxConns = defaultMaxConns
	}
	rp := cfg.ConnConfig.RuntimeParams
	rp["lock_timeout"] = sessionLockTimeout
	rp["statement_timeout"] = sessionStatementTimeout
	rp["timezone"] = "UTC"
	if rp["application_name"] == "" {
		rp["application_name"] = "ferund"
	}
	d := &DB{log: log, listening: make(chan struct{})}
	d.hub.subs = map[string]map[*subscriber]struct{}{}

	lc := cfg.ConnConfig.Copy()
	lc.RuntimeParams["application_name"] = "ferund-listener"
	lc.Tracer = nil
	d.listenConfig = lc

	cfg.ConnConfig.Tracer = failureTracer{d}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	d.Pool = pool
	return d, nil
}

// Close closes the pool. The listener ends with its own context.
func (d *DB) Close() {
	d.Pool.Close()
}

// LastFailure returns when the database last failed for a connection
// reason: a statement that failed on its connection, a failed acquire or
// connect, a lost listener connection. Zero if it never did.
func (d *DB) LastFailure() time.Time {
	n := d.lastFailure.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (d *DB) noteFailure() {
	d.lastFailure.Store(time.Now().UnixNano())
}

// maxTxRetries is how often Tx runs a function again after a retryable
// failure.
const maxTxRetries = 5

// Tx runs fn in a transaction and commits it. When the transaction fails
// with a serialization failure (40001), a deadlock (40P01) or a lock
// timeout (55P03), the whole function runs again, after a jittered backoff,
// up to 5 times. fn must not call a node or wait for anything slow, and
// must be safe to run more than once.
func (d *DB) Tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	backoff := 10 * time.Millisecond
	for attempt := 0; ; attempt++ {
		err := d.tx(ctx, fn)
		if err == nil || !Retryable(err) || attempt >= maxTxRetries {
			return err
		}
		// Full jitter: a random wait up to the backoff, so that the
		// transactions that collided do not collide again.
		wait := time.Duration(rand.Int64N(int64(backoff))) + time.Millisecond
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		backoff = min(backoff*2, 500*time.Millisecond)
	}
}

func (d *DB) tx(ctx context.Context, fn func(tx pgx.Tx) error) (err error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// Roll back on a context of its own, so that a cancelled
			// caller does not cost the connection.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = tx.Rollback(rctx)
			cancel()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Retryable reports whether err is a serialization failure, a deadlock or
// a lock timeout: a transaction failing with one of them may succeed when
// run again.
func Retryable(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Code {
	case "40001", "40P01", "55P03":
		return true
	}
	return false
}

// Notify sends payload on channel through q. Inside a transaction it is
// delivered when the transaction commits, and not at all if it rolls back.
func Notify(ctx context.Context, q Querier, channel, payload string) error {
	if _, err := q.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload); err != nil {
		return fmt.Errorf("notify %s: %w", channel, err)
	}
	return nil
}

// connectionFailure reports whether err, from a statement, an acquire or a
// connect made with ctx, failed for a reason of the connection or the
// server rather than of the statement or the caller.
func connectionFailure(ctx context.Context, err error) bool {
	if err == nil || (ctx != nil && ctx.Err() != nil) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		// connection_exception, admin_shutdown, crash_shutdown,
		// cannot_connect_now, too_many_connections.
		return strings.HasPrefix(pe.Code, "08") || pe.Code == "57P01" || pe.Code == "57P02" ||
			pe.Code == "57P03" || pe.Code == "53300"
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return strings.Contains(err.Error(), "conn closed")
}

// failureTracer records connection failures of the pool's statements,
// acquires and connects in DB.lastFailure.
type failureTracer struct{ d *DB }

func (t failureTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (t failureTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if connectionFailure(ctx, data.Err) {
		t.d.noteFailure()
	}
}

func (t failureTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return ctx
}

func (t failureTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if connectionFailure(ctx, data.Err) {
		t.d.noteFailure()
	}
}

func (t failureTracer) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	return ctx
}

func (t failureTracer) TraceConnectEnd(ctx context.Context, data pgx.TraceConnectEndData) {
	if connectionFailure(ctx, data.Err) {
		t.d.noteFailure()
	}
}
