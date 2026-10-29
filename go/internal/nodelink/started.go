// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// A node that stops sends GOAWAY on its node connections, lets the calls
// in flight end, and ends those that do not within its bound with
// UNAVAILABLE "the node stops: …" (node.md, "Suspend on stop"). A call that
// had not reached the node then never ran there: it goes to the node's
// next current connection, waiting as Conn does, up to the node's rollout
// deadline. A call that had reached it and whose connection ended without
// an answer reads that the node stopped during the call, or that its
// connection ended.

// callTrack is what the transport saw of one call: its stream was opened
// on a node connection (started), and the node sent the call's status
// (answered).
type callTrack struct {
	started, answered atomic.Bool
}

type callTrackKey struct{}

func withTrack(ctx context.Context) (context.Context, *callTrack) {
	t := &callTrack{}
	return context.WithValue(ctx, callTrackKey{}, t), t
}

// trackStats is the stats handler of the node connections' channels: it
// fills the callTrack of a call made through nodeConn.
type trackStats struct{}

func (trackStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }

func (trackStats) HandleRPC(ctx context.Context, s stats.RPCStats) {
	t, ok := ctx.Value(callTrackKey{}).(*callTrack)
	if !ok {
		return
	}
	switch s.(type) {
	case *stats.OutHeader:
		t.started.Store(true)
	case *stats.InTrailer:
		t.answered.Store(true)
	}
}

func (trackStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (trackStats) HandleConn(context.Context, stats.ConnStats)                       {}

// notStarted reports whether a call on l failed before it reached the
// node because l is going, as after the node's GOAWAY: no stream was
// opened, the call failed UNAVAILABLE and l's channel is no longer READY.
// l is then marked so (as watchState does), so that waitCurrent never
// picks it again and the call can go to the next connection.
func (s *Server) notStarted(ctx context.Context, l *link, t *callTrack, err error) bool {
	if t.started.Load() || status.Code(err) != codes.Unavailable || ctx.Err() != nil {
		return false
	}
	if l.cc.GetState() == connectivity.Ready {
		return false
	}
	s.markNotReady(l)
	return true
}

// markNotReady takes l out of the choice of the node's current
// connection: its channel left READY, and a node connection never comes
// back.
func (s *Server) markNotReady(l *link) {
	var d deferred
	s.mu.Lock()
	if !l.closed && !l.notReady {
		l.notReady = true
		if st := s.nodes[l.node]; st != nil {
			s.updateCurrentLocked(st, &d)
		}
	}
	s.mu.Unlock()
	d.run()
}

// cutError is how the error of a call on node reads: one that reached the
// node and whose connection ended before the node answered (UNAVAILABLE
// without the node's status, such as "error reading from server: EOF")
// says that the node stopped during the call, when it had announced its
// stop, or that its connection ended. Every other error is as it came.
func (s *Server) cutError(node string, t *callTrack, err error) error {
	if err == nil || !t.started.Load() || t.answered.Load() || status.Code(err) != codes.Unavailable {
		return err
	}
	msg := status.Convert(err).Message()
	s.mu.Lock()
	st := s.nodes[node]
	stopping := st != nil && st.stoppingUntil.After(time.Now())
	s.mu.Unlock()
	if stopping {
		return status.Errorf(codes.Unavailable, "node %s stopped during the call: %s", node, msg)
	}
	return status.Errorf(codes.Unavailable, "node %s: the connection ended during the call: %s", node, msg)
}
