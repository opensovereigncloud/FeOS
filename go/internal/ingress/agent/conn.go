// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// TCP and gRPC settings of the control connection: keepalive and
// TCP_USER_TIMEOUT notice a dead instance within seconds, gRPC's
// keepalive one whose connection hangs.
const (
	tcpKeepAlive    = 15 * time.Second
	tcpUserTimeout  = 20 * time.Second
	grpcKeepAlive   = 15 * time.Second
	grpcKeepTimeout = 5 * time.Second
	maxRecvMsgSize  = wire.MaxMessageBytes
	errConnGoneText = "the control connection is gone"
)

// session is one connection to one control-plane instance, on which Hello
// succeeded.
type session struct {
	addr   string
	conn   net.Conn
	cc     *grpc.ClientConn
	client *wire.Client
	id     *identity
	hello  *wire.HelloResponse
	// synced: the Sync stream delivered its whole table.
	synced atomic.Bool
	once   sync.Once
}

func (s *session) close() {
	s.once.Do(func() {
		_ = s.cc.Close()
		_ = s.conn.Close()
	})
}

// connect opens a session to one of the control plane's addresses, tried
// in random order: TLS with the current identity, then Hello. A refusal
// or a refused mesh key ends the round at once: every instance answers the
// same.
func (a *Agent) connect(ctx context.Context) (*session, error) {
	id := a.identity()
	if id == nil {
		return nil, errors.New("no identity")
	}
	var errs []error
	for _, addr := range a.order(a.cfg.ControlPlane) {
		s, err := a.open(ctx, addr, id)
		if err == nil {
			return s, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if _, refused := refusalText(err); refused || isKeyFixed(err) || isNewKeyNeeded(err) {
			return nil, err
		}
		errs = append(errs, fmt.Errorf("%s: %w", addr, err))
	}
	return nil, errors.Join(errs...)
}

// open dials addr with id and says Hello.
func (a *Agent) open(ctx context.Context, addr string, id *identity) (*session, error) {
	conn, err := a.dialTLS(ctx, addr, a.clientTLS(id))
	if err != nil {
		return nil, err
	}
	cc, err := onceClient(a.cfg.ControlPlaneName, conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	s := &session{addr: addr, conn: conn, cc: cc, client: wire.NewClient(cc), id: id}
	if err := a.hello(ctx, s); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// clientTLS is the instance's TLS configuration toward the control
// plane: TLS 1.3 without resumption, so that every connection shows the
// current certificate; the server verified against the trust bundle for
// the configured name, whatever address is dialled, and for the control
// plane's URI, which only its certificates carry.
func (a *Agent) clientTLS(id *identity) *tls.Config {
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		ServerName:             a.cfg.ControlPlaneName,
		RootCAs:                id.bundle,
		Certificates:           []tls.Certificate{id.cert},
		NextProtos:             []string{wire.ALPN},
		SessionTicketsDisabled: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return pki.VerifyControlPlane(id.bundle, a.cfg.ControlPlaneName, a.cfg.TrustDomain, rawCerts(cs))
		},
	}
}

// dialTLS dials addr and runs the TLS handshake of cfg; the server must
// negotiate ALPN ferun-ingress/1.
func (a *Agent) dialTLS(ctx context.Context, addr string, cfg *tls.Config) (*tls.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, a.tune.dialTimeout)
	raw, err := a.dialRaw(dctx, addr)
	cancel()
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, cfg)
	hctx, cancel := context.WithTimeout(ctx, a.tune.handshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		raw.Close()
		return nil, err
	}
	if got := conn.ConnectionState().NegotiatedProtocol; got != wire.ALPN {
		raw.Close()
		return nil, fmt.Errorf("the control plane at %s negotiated ALPN %q, not %q", addr, got, wire.ALPN)
	}
	return conn, nil
}

// dialRaw opens the TCP connection to addr.
func (a *Agent) dialRaw(ctx context.Context, addr string) (net.Conn, error) {
	if a.deps.DialControlPlane != nil {
		return a.deps.DialControlPlane(ctx, addr)
	}
	d := &net.Dialer{
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: tcpKeepAlive, Interval: tcpKeepAlive, Count: 3},
		Control: func(_, _ string, raw syscall.RawConn) error {
			setUserTimeout(raw, tcpUserTimeout)
			return nil
		},
	}
	return d.DialContext(ctx, "tcp", addr)
}

// onceClient makes a gRPC client over one connection that is already
// handshaken: its dialer gives the connection out once, so a broken
// connection fails the calls instead of being dialled again behind the
// agent's back. Every call forces the JSON codec.
func onceClient(serverName string, conn net.Conn) (*grpc.ClientConn, error) {
	var used atomic.Bool
	return grpc.NewClient("passthrough:///"+serverName,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			if used.Swap(true) {
				return nil, errors.New(errConnGoneText)
			}
			return conn, nil
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDisableServiceConfig(),
		// An idle channel would close its connection, which the dialer
		// cannot give again.
		grpc.WithIdleTimeout(0),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: grpcKeepAlive, Timeout: grpcKeepTimeout, PermitWithoutStream: true}),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(wire.Codec{}), grpc.MaxCallRecvMsgSize(maxRecvMsgSize)),
	)
}

// rawCerts are the DER certificates a peer presented.
func rawCerts(cs tls.ConnectionState) [][]byte {
	raw := make([][]byte, len(cs.PeerCertificates))
	for i, c := range cs.PeerCertificates {
		raw[i] = c.Raw
	}
	return raw
}

// shuffled returns the addresses in a random order.
func shuffled(addrs []string) []string {
	out := make([]string, len(addrs))
	for i, j := range rand.Perm(len(addrs)) {
		out[i] = addrs[j]
	}
	return out
}

// grpcStatus finds the gRPC status in err, also when it is wrapped.
func grpcStatus(err error) (*status.Status, bool) {
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		if st := gs.GRPCStatus(); st != nil {
			return st, true
		}
	}
	return nil, false
}

// statusCode is the code of err's gRPC status; codes.Unknown without one.
func statusCode(err error) codes.Code {
	if st, ok := grpcStatus(err); ok {
		return st.Code()
	}
	return codes.Unknown
}

// statusMessage is the message of err's gRPC status, or err's text.
func statusMessage(err error) string {
	if st, ok := grpcStatus(err); ok {
		return st.Message()
	}
	return err.Error()
}

// refusalText returns the text of a refusal: a PERMISSION_DENIED.
func refusalText(err error) (string, bool) {
	st, ok := grpcStatus(err)
	if !ok || st.Code() != codes.PermissionDenied {
		return "", false
	}
	return st.Message(), true
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

// delay is the next delay, ±20 %, so that instances that lost the control
// plane at once do not redial in step.
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
