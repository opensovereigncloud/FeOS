// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	watchv1 "github.com/ironcore-dev/feos/go/gen/feos/watch/v1"
)

// Hook runs before the node handles a call of its method, with the request
// of a unary call (nil for a stream). An error fails the call with it, as
// it is when it is a gRPC status; a hook may also block, as a slow node
// does.
type Hook func(ctx context.Context, method string, req any) error

// ExecFunc answers an Exec in place of running its command: it writes the
// output through out and returns the exit code. ok false hands the call to
// the real command.
type ExecFunc func(ctx context.Context, pod uuid.UUID, req *containerv1.ExecRequest, out ExecOutput) (exitCode int32, ok bool, err error)

// ExecOutput is where a scripted Exec writes its output frames.
type ExecOutput interface {
	Stdout(p []byte) error
	Stderr(p []byte) error
}

// ToolFunc answers a ToolService call (method is its full name, e.g.
// "/feos.tool.v1.ToolService/Read") in place of the node's tools. ok false
// hands the call to them.
type ToolFunc func(ctx context.Context, pod uuid.UUID, method string, req proto.Message) (resp proto.Message, ok bool, err error)

// BringUpFunc runs when a pod's bring-up is done waiting the PendingDelay:
// an error fails the pod (POD_STATE_FAILED with the error's text), and a
// function that blocks until ctx ends keeps it PENDING until a StopPod,
// DeletePod or RestorePod ends the bring-up.
type BringUpFunc func(ctx context.Context, pod uuid.UUID) error

// Script holds the faults and scripted answers of a node, settable while
// it runs. Methods are named by their full gRPC names, which the generated
// *_FullMethodName constants hold, e.g.
// podv1.PodService_RestorePod_FullMethodName.
type Script struct {
	n *Node

	mu           sync.Mutex
	hooks        map[string]Hook
	delays       map[string]time.Duration
	exec         ExecFunc
	tool         ToolFunc
	bringUp      BringUpFunc
	unreachable  map[uuid.UUID]bool
	unrestorable string
	portAddrs    map[uint32]string
	kubePorts    map[uuid.UUID]map[uint32]bool
}

func newScript(n *Node) *Script {
	return &Script{
		n:           n,
		hooks:       map[string]Hook{},
		delays:      map[string]time.Duration{},
		unreachable: map[uuid.UUID]bool{},
		portAddrs:   map[uint32]string{},
		kubePorts:   map[uuid.UUID]map[uint32]bool{},
	}
}

// HibernateUpload is the point of a hibernation where its disk and memory
// upload: a hook there holds it where a ResumePod abandons it; its ctx ends
// then (return nil). An error fails the hibernation as an upload's does.
const HibernateUpload = "testnode/HibernatePod/upload"

// HibernateHead is the point right after a hibernation's point of no
// return, before its head is put: a hook there holds it where a ResumePod
// waits for it and then restores the pod. An error fails the hibernation
// as a failed head does.
const HibernateHead = "testnode/HibernatePod/head"

// SetHook runs h before every call of method; nil removes it. method may
// also be one of the points inside a call's work, HibernateUpload and
// HibernateHead, where h runs with the HibernatePodRequest that started
// the hibernation.
func (s *Script) SetHook(method string, h Hook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h == nil {
		delete(s.hooks, method)
		return
	}
	s.hooks[method] = h
}

// SetDelay makes a mutating PodService call of method take d at the point
// where feosd waits for its work (the microVM, diskd, the upload): with
// Config.AbortOnCancel a cancelled call stops there, as feosd's does.
// The points: CreatePod after the pod is recorded and before its bring-up
// begins; StartPod before the pod goes PENDING; StopPod and DeletePod
// while the microVM goes; SuspendPod while the snapshot is written;
// ResumePod while the microVM resumes; HibernatePod while the checkpoint
// uploads, right after the hook at HibernateUpload, where a ResumePod
// that abandons the hibernation ends the wait; RestorePod after the pod
// went PENDING "restoring from <key>".
func (s *Script) SetDelay(method string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d <= 0 {
		delete(s.delays, method)
		return
	}
	s.delays[method] = d
}

// SetExec scripts Exec; nil runs the real commands again.
func (s *Script) SetExec(f ExecFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exec = f
}

// SetTool scripts the ToolService; nil runs the node's tools again.
func (s *Script) SetTool(f ToolFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tool = f
}

// SetBringUp scripts the end of pods' bring-ups; nil lets them succeed.
func (s *Script) SetBringUp(f BringUpFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bringUp = f
}

// SetGuestUnreachable makes calls into the pod's guest fail with
// UNAVAILABLE "the guest of pod <id>: ..." while on, as when its vsock does
// not answer.
func (s *Script) SetGuestUnreachable(pod uuid.UUID, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.unreachable[pod] = true
	} else {
		delete(s.unreachable, pod)
	}
}

// SetUnrestorable makes RestorePod boot pods anew on their checkpoint's
// disk, as a node whose Firecracker, CPU or kernel cannot load the
// snapshot does, with reason as the response's reason; "" restores their
// memory again.
func (s *Script) SetUnrestorable(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unrestorable = reason
}

// SetPortAddr makes a port forward to port dial addr; "" dials
// 127.0.0.1:<port> again, the default.
func (s *Script) SetPortAddr(port uint32, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if addr == "" {
		delete(s.portAddrs, port)
		return
	}
	s.portAddrs[port] = addr
}

// SetKubePorts sets the pod's Kubernetes listener ports, which its guest
// never forwards.
func (s *Script) SetKubePorts(pod uuid.UUID, ports ...uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := map[uint32]bool{}
	for _, p := range ports {
		set[p] = true
	}
	s.kubePorts[pod] = set
}

// FailPod fails a pod as a crashed microVM does: POD_STATE_FAILED with
// reason, and an event.
func (s *Script) FailPod(pod uuid.UUID, reason string) error {
	return s.n.pods.fail(pod, reason)
}

// LagPodEvents ends every StreamPodEvents stream with DATA_LOSS, as a
// reader that fell behind feosd's broadcast.
func (s *Script) LagPodEvents() {
	s.n.pods.lagAll()
}

// WatchEvents sends events in one batch to every open watch stream of the
// pod, which numbers them; it returns the number of streams.
func (s *Script) WatchEvents(pod uuid.UUID, events ...*watchv1.Event) int {
	return s.n.watch.deliver(pod, events)
}

// WatchStale sends an EVENT_KIND_STALE for paths to every open watch
// stream of the pod; it returns the number of streams.
func (s *Script) WatchStale(pod uuid.UUID, paths ...string) int {
	return s.n.watch.deliver(pod, []*watchv1.Event{{Kind: watchv1.EventKind_EVENT_KIND_STALE, StalePaths: paths}})
}

// Watches is the number of open watch streams of the pod.
func (s *Script) Watches(pod uuid.UUID) int {
	return s.n.watch.count(pod)
}

func (s *Script) hook(method string) Hook {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hooks[method]
}

func (s *Script) delay(method string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delays[method]
}

func (s *Script) execFunc() ExecFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exec
}

func (s *Script) toolFunc() ToolFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tool
}

func (s *Script) bringUpFunc() BringUpFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bringUp
}

func (s *Script) isUnreachable(pod uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unreachable[pod]
}

func (s *Script) unrestorableReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unrestorable
}

func (s *Script) portAddr(port uint32) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.portAddrs[port]; ok {
		return a
	}
	return "127.0.0.1:" + strconv.FormatUint(uint64(port), 10)
}

func (s *Script) isKubePort(pod uuid.UUID, port uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kubePorts[pod][port]
}

// point runs the hook set for a point inside a call's work (HibernateUpload,
// HibernateHead), with the call's request; its error as the call's.
func (s *Script) point(ctx context.Context, name string, req any) error {
	h := s.hook(name)
	if h == nil {
		return nil
	}
	if err := h(ctx, name, req); err != nil {
		if _, ok := status.FromError(err); ok {
			return err
		}
		return status.Error(codes.Unknown, err.Error())
	}
	return nil
}

// wait waits the delay set for method, or until ctx ends: the point at
// which a mutating call is cut when its request is cancelled.
func (s *Script) wait(ctx context.Context, method string) error {
	if !sleep(ctx, s.delay(method)) {
		return status.FromContextError(ctx.Err()).Err()
	}
	return nil
}
