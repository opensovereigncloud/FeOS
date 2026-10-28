// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// maxIdlePerEndpoint is how many idle connections the pool keeps per
// endpoint address and port.
const maxIdlePerEndpoint = 32

// errBodyTooLarge ends the read of a request body past the limit.
var errBodyTooLarge = errors.New("the request body is too large")

// errNotRouted fails a dial to an address that the table does not route
// to (any more): a request that picked its endpoint just before the
// table changed routes again by the new table, so that no connection
// reaches what was unpublished.
var errNotRouted = errors.New("the table does not route to this address")

// dialError is a failed dial to an upstream: nothing of the request was
// sent.
type dialError struct {
	addr string
	err  error
}

func (e *dialError) Error() string { return "dial " + e.addr + ": " + e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// upstreams is the pool of connections to the instances: one HTTP/1.1
// transport whose connections are tracked, so that shutdown can close
// those that are not idle (upgraded ones, long polls) as well, and a
// change of the table those to instances it no longer routes to.
type upstreams struct {
	tr      *http.Transport
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	timeout time.Duration
	// route tells what a dial address reaches in the table; a dial to an
	// address it does not know fails. Nil: every address.
	route func(addr string) (dialTarget, bool)
	dials atomic.Uint64
	mu    sync.Mutex
	conns map[*trackedConn]struct{}
}

func newUpstreams(cfg Config, dial func(ctx context.Context, network, addr string) (net.Conn, error),
	route func(addr string) (dialTarget, bool)) *upstreams {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	u := &upstreams{dial: dial, timeout: cfg.DialTimeout, route: route, conns: map[*trackedConn]struct{}{}}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	u.tr = &http.Transport{
		DialContext:           u.dialContext,
		Protocols:             &protocols,
		MaxIdleConnsPerHost:   maxIdlePerEndpoint,
		MaxConnsPerHost:       cfg.MaxConnsPerEndpoint,
		IdleConnTimeout:       cfg.IdleTimeout,
		ExpectContinueTimeout: time.Second,
		// The client's Accept-Encoding goes to the upstream as it is,
		// and so does the upstream's encoding back.
		DisableCompression: true,
	}
	return u
}

// dialContext dials an upstream within the dial timeout, if the table
// routes to it, before the dial and again once it is connected. A
// failure is a dialError.
//
// The second check closes the race with a change of the table during
// the dial: Proxy.SetState stores the new table before it cuts, under
// the pool's lock, the connections to what the table dropped, and the
// check runs under that lock as the connection joins the pool, so
// either the cut sees the connection or the check sees the new table.
func (u *upstreams) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	u.dials.Add(1)
	if u.route != nil {
		if _, ok := u.route(addr); !ok {
			return nil, &dialError{addr: addr, err: errNotRouted}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, u.timeout)
	defer cancel()
	c, err := u.dial(ctx, network, addr)
	if err != nil {
		return nil, &dialError{addr: addr, err: err}
	}
	tc := &trackedConn{Conn: c, u: u, addr: addr}
	u.mu.Lock()
	routed := true
	if u.route != nil {
		tc.to, routed = u.route(addr)
	}
	if routed {
		u.conns[tc] = struct{}{}
	}
	u.mu.Unlock()
	if !routed {
		_ = c.Close()
		return nil, &dialError{addr: addr, err: errNotRouted}
	}
	return tc, nil
}

// closeIdle closes the pool's idle connections.
func (u *upstreams) closeIdle() {
	u.tr.CloseIdleConnections()
}

// closeAll closes every connection of the pool, idle or not.
func (u *upstreams) closeAll() {
	u.tr.CloseIdleConnections()
	u.closeIf(func(*trackedConn) bool { return true })
}

// closeIf closes every connection of the pool, idle or not, for which
// cut reports true, and returns how many. cut runs under the pool's
// lock, which guards trackedConn.leftAt.
func (u *upstreams) closeIf(cut func(c *trackedConn) bool) int {
	u.mu.Lock()
	var conns []*trackedConn
	for c := range u.conns {
		if cut(c) {
			conns = append(conns, c)
		}
	}
	u.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return len(conns)
}

// open is how many connections the pool holds.
func (u *upstreams) open() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.conns)
}

// trackedConn is a connection of the pool, which leaves the pool's set
// when it is closed.
type trackedConn struct {
	net.Conn
	u *upstreams
	// addr is the address it was dialled at, to what it reached then.
	addr string
	to   dialTarget
	// leftAt is when the proxy first saw its instance gone from a
	// service that is still routed (Proxy.cutConns); zero while the
	// table routes to addr.
	leftAt time.Time
	once   sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.u.mu.Lock()
		delete(c.u.conns, c)
		c.u.mu.Unlock()
	})
	return c.Conn.Close()
}

// body is a request's body on its way to the upstream: it counts what
// was read, so that a request whose body was not touched can be sent
// again, and ends the read past the limit (0: none).
type body struct {
	rc    io.ReadCloser
	limit int64
	n     atomic.Int64
	over  atomic.Bool
}

func (b *body) Read(p []byte) (int, error) {
	if b.over.Load() {
		return 0, errBodyTooLarge
	}
	if b.limit > 0 {
		// One byte past the limit tells a body of exactly the limit
		// from a larger one.
		if left := b.limit - b.n.Load() + 1; int64(len(p)) > left {
			p = p[:left]
		}
	}
	n, err := b.rc.Read(p)
	if total := b.n.Add(int64(n)); b.limit > 0 && total > b.limit {
		b.over.Store(true)
		return 0, errBodyTooLarge
	}
	return n, err
}

func (b *body) Close() error {
	return b.rc.Close()
}

// untouched reports whether nothing of the body was read; a nil body is
// untouched.
func (b *body) untouched() bool {
	return b == nil || b.n.Load() == 0
}

// tooLarge reports whether the body was read past the limit.
func (b *body) tooLarge() bool {
	return b != nil && b.over.Load()
}
