// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// tcpKeepAlive is the TCP keepalive of accepted connections, and
// tcpUserTimeout how long unacknowledged data may wait before the kernel
// drops the connection: a dead peer is noticed within seconds, not after
// the kernel's default of many minutes.
const (
	tcpKeepAlive   = 15 * time.Second
	tcpUserTimeout = 20 * time.Second
)

// tuneTCP sets keepalive and TCP_USER_TIMEOUT on an accepted connection.
func tuneTCP(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: tcpKeepAlive, Interval: tcpKeepAlive, Count: 3})
	if raw, err := tc.SyscallConn(); err == nil {
		setUserTimeout(raw, tcpUserTimeout)
	}
}

// chanListener hands connections that the nodes' port has already
// accepted and handshaken to a grpc.Server.
type chanListener struct {
	addr   net.Addr
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{addr: addr, ch: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

// hand passes c to the server, or closes it when the listener is closed.
func (l *chanListener) hand(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.closed:
		c.Close()
	}
}

// srvConn is a handshaken TLS connection handed to one of the port's gRPC
// servers. It carries the AuthInfo the server's credentials return, and
// runs onClose once when it is closed, by the server or by nodelink.
type srvConn struct {
	*tls.Conn
	info    *authInfo
	onClose func()
	once    sync.Once
	done    chan struct{}
}

func newSrvConn(c *tls.Conn, info *authInfo, onClose func()) *srvConn {
	return &srvConn{Conn: c, info: info, onClose: onClose, done: make(chan struct{})}
}

func (c *srvConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		close(c.done)
		if c.onClose != nil {
			c.onClose()
		}
	})
	return err
}

// rateLog logs at most one line per second per kind, counting what it
// suppressed: for lines a remote party can trigger.
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
	now := time.Now()
	r.mu.Lock()
	k := r.kinds[kind]
	if k == nil {
		k = &rateKind{}
		r.kinds[kind] = k
	} else if now.Sub(k.last) < time.Second {
		k.suppressed++
		r.mu.Unlock()
		return
	}
	n := k.suppressed
	k.last, k.suppressed = now, 0
	r.mu.Unlock()
	if n > 0 {
		args = append(args, "suppressed", n)
	}
	r.log.Warn(msg, args...)
}

// helloConn is an accepted connection during its TLS handshake; hello is
// set once its ClientHello has arrived.
type helloConn struct {
	net.Conn
	hello atomic.Bool
}

// noteClientHello is the port's GetConfigForClient: it records that the
// ClientHello arrived and keeps the port's TLS config.
func noteClientHello(chi *tls.ClientHelloInfo) (*tls.Config, error) {
	if hc, ok := chi.Conn.(*helloConn); ok {
		hc.hello.Store(true)
	}
	return nil, nil
}

// isProbe reports whether a handshake failed because the peer closed
// before its ClientHello: what a balancer's TCP health check does. A peer
// that closes after its ClientHello fails a real handshake, which is
// logged.
func isProbe(err error, hello bool) bool {
	return !hello && errors.Is(err, io.EOF)
}
