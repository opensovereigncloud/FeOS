// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ports

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	portforwardv1 "github.com/ironcore-dev/feos/go/gen/feos/portforward/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
)

// tkey names a pool: a sandbox's pod and one of its ports.
type tkey struct {
	id   uuid.UUID
	port int
}

// entry is the pool of one (pod, port), bound to the node and the
// incarnation it dials. Its fields but key and tr are guarded by
// Handler.mu.
type entry struct {
	key         tkey
	node        string
	incarnation uuid.UUID
	tr          *http.Transport
	// active counts the requests using tr; lastUsed is when the last one
	// began or ended.
	active   int
	lastUsed time.Time
	// retired: no new request uses tr; its connections close as they go
	// idle.
	retired bool
}

// transport returns the pool for the lease's pod and port, and the
// function that ends the request's use of it. A pool bound to another
// node or incarnation is retired and replaced: its connections reach a
// pod that is not the lease's.
func (h *Handler) transport(lease model.Lease, port int) (*http.Transport, func()) {
	k := tkey{lease.SandboxID, port}
	var idle []*http.Transport
	h.mu.Lock()
	e := h.transports[k]
	if e != nil && (e.node != lease.Node || e.incarnation != lease.Incarnation) {
		idle = append(idle, h.retireLocked(e))
		e = nil
	}
	if e == nil {
		if len(h.transports) >= h.t.maxTransports {
			if old := h.leastRecentLocked(); old != nil {
				idle = append(idle, h.retireLocked(old))
			}
		}
		e = &entry{key: k, node: lease.Node, incarnation: lease.Incarnation}
		e.tr = h.newTransport(k, lease.Node)
		h.transports[k] = e
	}
	e.active++
	e.lastUsed = time.Now()
	h.mu.Unlock()
	closeIdle(idle)
	return e.tr, func() {
		h.mu.Lock()
		e.active--
		e.lastUsed = time.Now()
		retired := e.retired
		h.mu.Unlock()
		if retired {
			e.tr.CloseIdleConnections()
		}
	}
}

// retireLocked takes e out of service and returns its transport, whose
// idle connections the caller closes outside the lock.
func (h *Handler) retireLocked(e *entry) *http.Transport {
	if h.transports[e.key] == e {
		delete(h.transports, e.key)
	}
	if !e.retired {
		e.retired = true
		h.retired = append(h.retired, e)
	}
	return e.tr
}

// leastRecentLocked is the pool to evict for a new one: the least
// recently used idle one, else the least recently used.
func (h *Handler) leastRecentLocked() *entry {
	var best, bestIdle *entry
	for _, e := range h.transports {
		if best == nil || e.lastUsed.Before(best.lastUsed) {
			best = e
		}
		if e.active == 0 && (bestIdle == nil || e.lastUsed.Before(bestIdle.lastUsed)) {
			bestIdle = e
		}
	}
	if bestIdle != nil {
		return bestIdle
	}
	return best
}

func closeIdle(trs []*http.Transport) {
	for _, tr := range trs {
		tr.CloseIdleConnections()
	}
}

// retireWhere retires the pools match selects and closes their idle
// connections.
func (h *Handler) retireWhere(match func(e *entry) bool) {
	var idle []*http.Transport
	h.mu.Lock()
	for _, e := range h.transports {
		if match(e) {
			idle = append(idle, h.retireLocked(e))
		}
	}
	h.mu.Unlock()
	closeIdle(idle)
}

// closeIdleOf closes the idle connections of every pool of a sandbox,
// retired ones included: their Forward streams hold slots of the
// sandbox's cap.
func (h *Handler) closeIdleOf(id uuid.UUID) {
	var idle []*http.Transport
	h.mu.Lock()
	for _, e := range h.transports {
		if e.key.id == id {
			idle = append(idle, e.tr)
		}
	}
	for _, e := range h.retired {
		if e.key.id == id {
			idle = append(idle, e.tr)
		}
	}
	h.mu.Unlock()
	closeIdle(idle)
}

// retiredLinger is how long a retired pool without requests is kept after
// its last request, so that the connection that request put back is
// closed too (the transport pools it asynchronously).
const retiredLinger = 5 * time.Second

// sweep evicts the pools unused for unusedAfter, and closes the idle
// connections of the retired ones, which it forgets once they have no
// request left.
func (h *Handler) sweep() {
	now := time.Now()
	var idle []*http.Transport
	h.mu.Lock()
	for _, e := range h.transports {
		if e.active == 0 && now.Sub(e.lastUsed) >= h.t.unusedAfter {
			h.retireLocked(e)
		}
	}
	kept := h.retired[:0]
	for _, e := range h.retired {
		idle = append(idle, e.tr)
		if e.active > 0 || now.Sub(e.lastUsed) < retiredLinger {
			kept = append(kept, e)
		}
	}
	clear(h.retired[len(kept):])
	h.retired = kept
	h.mu.Unlock()
	closeIdle(idle)
}

// newTransport is the pool of one (pod, port) on node: HTTP/1.1 over
// Forward streams, at most as many connections as the sandbox has slots
// (more requests wait for one, at most capWait: waitForConn),
// maxIdlePerHost of them kept idle. The pool's own bound keeps a burst on
// one port on the connections it has: a request it queues does not dial,
// and so neither takes a slot nor closes pooled connections for one.
func (h *Handler) newTransport(k tkey, node string) *http.Transport {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	return &http.Transport{
		// The address is the pseudo-host, which only keys the pool.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return h.dial(ctx, k, node)
		},
		Protocols:           &protocols,
		MaxIdleConnsPerHost: h.t.maxIdlePerHost,
		MaxConnsPerHost:     h.t.perSandbox,
		IdleConnTimeout:     h.t.idleTimeout,
		DisableCompression:  true,
	}
}

// waitForConn bounds a request's wait for its turn at a connection of its
// pool to capWait: the wait in the pool's queue, which the transport would
// let last without end, and its dial's wait for a slot of the sandbox's
// cap. Past it the request fails with errCapFull, a 503. The wait is timed
// per attempt of the transport, from httptrace's GetConn to its GotConn or
// to the moment the attempt's dial holds its slot (connWait.dialing): what
// is left of a dial, the node's connection (which waits out the node's
// rollout grace) and the guest's ready, ends with the dial's own result,
// and a connection that has been got is never cut. It returns the
// request's context and the function that ends it.
func (h *Handler) waitForConn(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	cw := &connWait{wait: h.t.capWait, cancel: cancel}
	ctx = context.WithValue(ctx, connWaitKey{}, cw)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn: func(string) { cw.begin() },
		GotConn: func(httptrace.GotConnInfo) { cw.end() },
	})
	return ctx, func() {
		cw.end()
		cancel(nil)
	}
}

// connWait is one request's wait for a connection (waitForConn).
type connWait struct {
	wait   time.Duration
	cancel context.CancelCauseFunc

	mu sync.Mutex
	// timer is the current attempt's; nil once the attempt has a
	// connection or its dial a slot.
	timer *time.Timer
}

// connWaitKey carries a request's *connWait in its context, and so in the
// context of the transport's dial for it, which keeps the request's
// values.
type connWaitKey struct{}

// connWaitOf is the wait of the request a dial's ctx is for; nil when
// there is none.
func connWaitOf(ctx context.Context) *connWait {
	cw, _ := ctx.Value(connWaitKey{}).(*connWait)
	return cw
}

// begin starts an attempt's wait.
func (cw *connWait) begin() {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if cw.timer != nil {
		cw.timer.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(cw.wait, func() {
		cw.mu.Lock()
		defer cw.mu.Unlock()
		if cw.timer == t {
			cw.cancel(errCapFull)
		}
	})
	cw.timer = t
}

// end ends the current attempt's wait.
func (cw *connWait) end() {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if cw.timer != nil {
		cw.timer.Stop()
		cw.timer = nil
	}
}

// dialing is called by a dial as it begins. The function it returns, which
// the dial calls once it holds its slot, ends the wait of the attempt the
// dial began for; not a later attempt's, which a dial outliving its own
// attempt must not leave unbounded. On a nil cw it does nothing.
func (cw *connWait) dialing() (slotTaken func()) {
	if cw == nil {
		return func() {}
	}
	cw.mu.Lock()
	t := cw.timer
	cw.mu.Unlock()
	return func() {
		cw.mu.Lock()
		defer cw.mu.Unlock()
		if t != nil && cw.timer == t {
			t.Stop()
			cw.timer = nil
		}
	}
}

// capSem is the slots of one sandbox's Forward streams on this instance;
// refs counts the holders and waiters, and the entry is dropped at 0.
type capSem struct {
	slots chan struct{}
	refs  int
}

// errCapFull is a request that got neither a connection of its pool nor
// a slot of the sandbox's cap for its dial within capWait, or a dial that
// found no slot within capWait.
var errCapFull = errors.New("too many forwarded connections into the sandbox on this instance")

// takeSlot takes one of the sandbox's slots, waiting up to capWait (or
// ctx) for one, and returns its release.
func (h *Handler) takeSlot(ctx context.Context, id uuid.UUID) (func(), error) {
	h.mu.Lock()
	c := h.caps[id]
	if c == nil {
		c = &capSem{slots: make(chan struct{}, h.t.perSandbox)}
		h.caps[id] = c
	}
	c.refs++
	h.mu.Unlock()
	unref := func() {
		h.mu.Lock()
		c.refs--
		if c.refs == 0 && h.caps[id] == c {
			delete(h.caps, id)
		}
		h.mu.Unlock()
	}
	release := func() {
		<-c.slots
		unref()
	}
	select {
	case c.slots <- struct{}{}:
		return release, nil
	default:
	}
	// Full. Pooled idle connections hold slots they do not use: they are
	// closed now, and again while the dial waits, as requests in flight
	// end and put theirs back into the pools.
	h.closeIdleOf(id)
	t := time.NewTimer(h.t.capWait)
	defer t.Stop()
	reclaim := time.NewTicker(capReclaimEvery)
	defer reclaim.Stop()
	for {
		select {
		case c.slots <- struct{}{}:
			return release, nil
		case <-reclaim.C:
			h.closeIdleOf(id)
		case <-t.C:
			unref()
			return nil, errCapFull
		case <-ctx.Done():
			unref()
			return nil, ctx.Err()
		}
	}
}

// tryProbeSlot takes one of the sandbox's probe slots when one is free,
// without waiting, and returns its release; false when every one is
// taken. The probe slots are apart from the stream cap: a check neither
// takes nor waits for a slot of the cap, nor closes pooled connections
// for one.
func (h *Handler) tryProbeSlot(id uuid.UUID) (func(), bool) {
	h.mu.Lock()
	c := h.probes[id]
	if c == nil {
		c = &capSem{slots: make(chan struct{}, h.t.probeSlots)}
		h.probes[id] = c
	}
	c.refs++
	h.mu.Unlock()
	unref := func() {
		h.mu.Lock()
		c.refs--
		if c.refs == 0 && h.probes[id] == c {
			delete(h.probes, id)
		}
		h.mu.Unlock()
	}
	select {
	case c.slots <- struct{}{}:
		return func() {
			<-c.slots
			unref()
		}, true
	default:
		unref()
		return nil, false
	}
}

// capReclaimEvery is how often a dial that waits for a slot closes the
// sandbox's idle pooled connections.
const capReclaimEvery = 50 * time.Millisecond

// dialError is a Forward stream that could not be opened: the node's (or
// nodelink's) gRPC status says why.
type dialError struct {
	node string
	err  error
}

func (e *dialError) Error() string { return fmt.Sprintf("forward on node %s: %v", e.node, e.err) }
func (e *dialError) Unwrap() error { return e.err }

// dial opens one forwarded connection: a slot of the sandbox's cap, then a
// Forward stream on the node connection, open{port}, and the guest's
// ready, so that "nothing listens on the port" arrives as a failed dial
// and not as a stream that stays silent. Once it holds the slot, its
// request no longer waits on the cap: the rest takes as long as the node
// and the guest take. The stream does not end with ctx: an upgraded
// connection outlives the dial by far; Close ends it.
func (h *Handler) dial(ctx context.Context, k tkey, node string) (net.Conn, error) {
	slotTaken := connWaitOf(ctx).dialing()
	release, err := h.takeSlot(ctx, k.id)
	if err != nil {
		return nil, err
	}
	slotTaken()
	return h.openStream(ctx, k, node, release)
}

// openStream is the rest of a dial once it holds its slot, which release
// gives back (at once when the stream cannot be opened, else when the
// connection closes): the Forward stream, open{port}, and the guest's
// ready, until which ctx ends the stream.
func (h *Handler) openStream(ctx context.Context, k tkey, node string, release func()) (net.Conn, error) {
	cc, err := h.conns.Conn(ctx, node)
	if err != nil {
		release()
		return nil, &dialError{node: node, err: err}
	}
	sctx, cancel := context.WithCancel(h.base)
	fail := func(err error) (net.Conn, error) {
		cancel()
		release()
		return nil, &dialError{node: node, err: err}
	}
	stream, err := portforwardv1.NewPortForwardServiceClient(cc).Forward(nodelink.PodContext(sctx, k.id))
	if err != nil {
		return fail(err)
	}
	// Until the guest is ready, the dial's context ends the stream.
	stop := context.AfterFunc(ctx, cancel)
	err = stream.Send(&portforwardv1.ForwardRequest{
		Frame: &portforwardv1.ForwardRequest_Open{Open: &portforwardv1.Open{Port: uint32(k.port)}},
	})
	var first *portforwardv1.ForwardResponse
	if err == nil || errors.Is(err, io.EOF) {
		// A stream the node ended answers Send with io.EOF; Recv has the
		// status.
		first, err = stream.Recv()
	}
	if !stop() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
	}
	if err == nil && first.GetReady() == nil {
		err = status.Errorf(codes.Internal, "port %d: the guest answered %T instead of ready", k.port, first.GetFrame())
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = status.Errorf(codes.Unavailable, "the forward of port %d ended before the guest was ready", k.port)
		}
		return fail(err)
	}
	c := &streamConn{stream: stream, cancel: cancel, release: release,
		addr: forwardAddr(k.id.String() + ":" + strconv.Itoa(k.port))}
	return c, nil
}

// streamConn is one Forward stream used as a net.Conn: Write sends data
// frames, Read returns the data frames' bytes, Close ends the stream.
// gRPC allows one concurrent sender and one concurrent receiver, which is
// what a net.Conn needs; each side has its own lock. Deadlines are
// accepted and ignored: neither the transport nor the proxy sets one, and
// a forwarded connection may sit idle for as long as its peers like.
type streamConn struct {
	stream  portforwardv1.PortForwardService_ForwardClient
	cancel  context.CancelFunc
	release func()
	addr    forwardAddr

	rmu   sync.Mutex
	spare []byte // the rest of the last data frame

	wmu     sync.Mutex
	wclosed bool

	closed atomic.Bool
	once   sync.Once
}

// forwardChunk is the largest data frame sent: well under the gRPC
// servers' default message limit of 4 MiB.
const forwardChunk = 32 << 10

func (c *streamConn) Read(b []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for len(c.spare) == 0 {
		msg, err := c.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, io.EOF
			}
			if c.closed.Load() {
				return 0, net.ErrClosed
			}
			return 0, err
		}
		c.spare = msg.GetData()
	}
	n := copy(b, c.spare)
	c.spare = c.spare[n:]
	return n, nil
}

func (c *streamConn) Write(b []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.wclosed || c.closed.Load() {
		return 0, net.ErrClosed
	}
	written := 0
	for len(b) > 0 {
		n := min(len(b), forwardChunk)
		err := c.stream.Send(&portforwardv1.ForwardRequest{
			Frame: &portforwardv1.ForwardRequest_Data{Data: append([]byte(nil), b[:n]...)},
		})
		if err != nil {
			if c.closed.Load() {
				return written, net.ErrClosed
			}
			return written, err
		}
		written += n
		b = b[n:]
	}
	return written, nil
}

// CloseWrite half-closes: the guest sees the end of what it reads.
func (c *streamConn) CloseWrite() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.wclosed || c.closed.Load() {
		return nil
	}
	c.wclosed = true
	return c.stream.CloseSend()
}

// Close ends the stream both ways and gives its slot back.
func (c *streamConn) Close() error {
	c.once.Do(func() {
		c.closed.Store(true)
		c.cancel()
		c.release()
	})
	return nil
}

func (c *streamConn) LocalAddr() net.Addr              { return c.addr }
func (c *streamConn) RemoteAddr() net.Addr             { return c.addr }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

// forwardAddr is a forwarded connection's address: "<pod>:<port>".
type forwardAddr string

func (a forwardAddr) Network() string { return "feos-forward" }
func (a forwardAddr) String() string  { return string(a) }
