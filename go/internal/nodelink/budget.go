// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"strings"
	"sync"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	networkv1 "github.com/ironcore-dev/feos/go/gen/feos/network/v1"
	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	volumev1 "github.com/ironcore-dev/feos/go/gen/feos/volume/v1"
)

// PodIDMetadata is the gRPC metadata key that routes a call to a pod's
// guest on its node.
const PodIDMetadata = "feos-pod-id"

// PodContext returns ctx with the outgoing metadata feos-pod-id set to
// podID in lowercase hyphenated form: every call into a pod's guest
// carries it.
func PodContext(ctx context.Context, podID uuid.UUID) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(PodIDMetadata, podID.String())
	return metadata.NewOutgoingContext(ctx, md)
}

// callKind classifies a call for the per-connection accounting.
type callKind int

const (
	callPlain     callKind = iota // counted as active only
	callLifecycle                 // a unary PodService lifecycle call of an op
	callTool                      // a unary ToolService call: within the tool budget
	callLong                      // a long stream: within the stream budget
)

// lifecycleMethods are the unary PodService calls that ops make, and the
// VolumeService and SecurityGroupService calls that prepare a node for
// them (the environments' prepare hook and loops, ADR 0018); the drain
// timeout never cuts them.
var lifecycleMethods = map[string]bool{
	podv1.PodService_CreatePod_FullMethodName:    true,
	podv1.PodService_StartPod_FullMethodName:     true,
	podv1.PodService_StopPod_FullMethodName:      true,
	podv1.PodService_SuspendPod_FullMethodName:   true,
	podv1.PodService_ResumePod_FullMethodName:    true,
	podv1.PodService_HibernatePod_FullMethodName: true,
	podv1.PodService_RestorePod_FullMethodName:   true,
	podv1.PodService_DeletePod_FullMethodName:    true,

	volumev1.VolumeService_CreateVolume_FullMethodName: true,
	volumev1.VolumeService_DeleteVolume_FullMethodName: true,
	volumev1.VolumeService_GetVolume_FullMethodName:    true,
	volumev1.VolumeService_ListVolumes_FullMethodName:  true,

	networkv1.SecurityGroupService_PutSecurityGroup_FullMethodName:    true,
	networkv1.SecurityGroupService_UpdateSecurityGroup_FullMethodName: true,
	networkv1.SecurityGroupService_DeleteSecurityGroup_FullMethodName: true,
	networkv1.SecurityGroupService_GetSecurityGroup_FullMethodName:    true,
	networkv1.SecurityGroupService_ListSecurityGroups_FullMethodName:  true,
}

const toolServicePrefix = "/feos.tool.v1.ToolService/"

func unaryKind(method string) callKind {
	switch {
	case lifecycleMethods[method]:
		return callLifecycle
	case strings.HasPrefix(method, toolServicePrefix):
		return callTool
	}
	return callPlain
}

// streamKind: every stream counts against the stream budget (Exec, Watch,
// Forward, consoles, traffic and host log streams, and any other), except
// PodService/StreamPodEvents, which the op waiter and Ensure's wait need
// to find free.
func streamKind(method string) callKind {
	if method == podv1.PodService_StreamPodEvents_FullMethodName {
		return callPlain
	}
	return callLong
}

// reserveLocked takes a call's slot on l, or fails with the budget's
// error.
func (s *Server) reserveLocked(l *link, kind callKind) error {
	switch kind {
	case callLong:
		if l.long >= s.longBudget {
			return status.Errorf(codes.ResourceExhausted, "node %s: stream budget exhausted", l.node)
		}
		l.long++
	case callTool:
		if l.tools >= s.toolBudget {
			return status.Errorf(codes.ResourceExhausted, "node %s: tool budget exhausted", l.node)
		}
		l.tools++
	case callLifecycle:
		l.lifecycle++
	}
	l.active++
	return nil
}

// release gives a call's slot back; a draining connection whose last call
// ended is closed, and so is one past its drain timeout whose last
// lifecycle call ended.
func (s *Server) release(l *link, kind callKind) {
	var d deferred
	s.mu.Lock()
	switch kind {
	case callLong:
		l.long--
	case callTool:
		l.tools--
	case callLifecycle:
		l.lifecycle--
	}
	l.active--
	switch {
	case l.closed:
	case l.forceDrain && l.lifecycle == 0:
		s.closeLinkLocked(l, "drain timeout", &d)
	case l.draining && l.active == 0:
		s.closeLinkLocked(l, "drained", &d)
	}
	s.mu.Unlock()
	d.run()
}

// nodeConn is what Conn returns: every call goes to the node's connection
// that is current at that call, within its budgets.
type nodeConn struct {
	s    *Server
	node string
}

func (c *nodeConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	kind := unaryKind(method)
	l, err := c.s.waitCurrent(ctx, c.node, true, kind)
	if err != nil {
		return err
	}
	defer c.s.release(l, kind)
	return l.cc.Invoke(ctx, method, args, reply, opts...)
}

func (c *nodeConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	kind := streamKind(method)
	l, err := c.s.waitCurrent(ctx, c.node, true, kind)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	var id uint64
	if kind == callLong {
		c.s.mu.Lock()
		if l.closed || l.forceDrain {
			c.s.mu.Unlock()
			cancel()
			c.s.release(l, kind)
			return nil, status.Errorf(codes.Unavailable, "node %s: connection is closing", c.node)
		}
		l.streamSeq++
		id = l.streamSeq
		if l.streams == nil {
			l.streams = map[uint64]context.CancelFunc{}
		}
		l.streams[id] = cancel
		c.s.mu.Unlock()
	}
	cs := &countedStream{}
	cs.end = func() {
		if kind == callLong {
			c.s.mu.Lock()
			delete(l.streams, id)
			c.s.mu.Unlock()
		}
		cancel()
		c.s.release(l, kind)
	}
	stream, err := l.cc.NewStream(ctx, desc, method, opts...)
	if err != nil {
		cs.done()
		return nil, err
	}
	cs.ClientStream = stream
	// The slot is given back when the stream's context ends; end cancels
	// that context too, so this runs once in every case.
	context.AfterFunc(ctx, cs.done)
	return cs, nil
}

// countedStream holds its slot until RecvMsg returns an error or its
// context ends.
type countedStream struct {
	grpc.ClientStream
	once sync.Once
	end  func()
}

func (s *countedStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil {
		s.done()
	}
	return err
}

func (s *countedStream) done() { s.once.Do(s.end) }
