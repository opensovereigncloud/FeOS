// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	watchv1 "github.com/gardenlinux/feos/go/gen/feos/watch/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// Hook runs before the node handles a call of its method, with the request
// of a unary call (nil for a stream). An error fails the call with it, as
// it is when it is a gRPC status; a hook may also block, as a slow node
// does.
type Hook func(ctx context.Context, method string, req any) error

// ExecFunc answers an Exec in place of running its command: it writes the
// output through out and returns the exit code. ok false hands the call to
// the real command.
type ExecFunc func(ctx context.Context, pod podid.ID, req *containerv1.ExecRequest, out ExecOutput) (exitCode int32, ok bool, err error)

// ExecOutput is where a scripted Exec writes its output frames.
type ExecOutput interface {
	Stdout(p []byte) error
	Stderr(p []byte) error
}

// ToolFunc answers a ToolService call (method is its full name, e.g.
// "/feos.tool.v1.ToolService/Read") in place of the node's tools. ok false
// hands the call to them.
type ToolFunc func(ctx context.Context, pod podid.ID, method string, req proto.Message) (resp proto.Message, ok bool, err error)

// BringUpFunc runs when a pod's bring-up is done waiting the PendingDelay:
// an error fails the pod (POD_STATE_FAILED with the error's text), and a
// function that blocks until ctx ends keeps it PENDING until a StopPod,
// DeletePod or RestorePod ends the bring-up.
type BringUpFunc func(ctx context.Context, pod podid.ID) error

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
	unreachable  map[podid.ID]bool
	unrestorable string
	portAddrs    map[uint32]string
	podPortAddrs map[podid.ID]map[uint32]string
	podExec      map[podid.ID]ExecFunc
	kubePorts    map[podid.ID]map[uint32]bool
}

func newScript(n *Node) *Script {
	return &Script{
		n:            n,
		hooks:        map[string]Hook{},
		delays:       map[string]time.Duration{},
		unreachable:  map[podid.ID]bool{},
		portAddrs:    map[uint32]string{},
		podPortAddrs: map[podid.ID]map[uint32]string{},
		podExec:      map[podid.ID]ExecFunc{},
		kubePorts:    map[podid.ID]map[uint32]bool{},
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

// SetPodExec scripts the Execs into one pod, before the function of
// SetExec; nil removes it. Without a function that answers, an Exec into
// an environment's pod (its spec has the label ferun.environment) exits 0
// without running anything, so that an addon's probe and provisioning
// (pg_isready, psql) pass; any other runs its real command.
func (s *Script) SetPodExec(pod podid.ID, f ExecFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f == nil {
		delete(s.podExec, pod)
		return
	}
	s.podExec[pod] = f
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
func (s *Script) SetGuestUnreachable(pod podid.ID, on bool) {
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

// SetPodPortAddr makes a port forward to port of the pod dial addr, in
// place of SetPortAddr's address or 127.0.0.1:<port>; "" removes it.
func (s *Script) SetPodPortAddr(pod podid.ID, port uint32, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if addr == "" {
		delete(s.podPortAddrs[pod], port)
		return
	}
	if s.podPortAddrs[pod] == nil {
		s.podPortAddrs[pod] = map[uint32]string{}
	}
	s.podPortAddrs[pod][port] = addr
}

// ExitContainer makes the process of a container of a running pod exit
// with code, as a crashed or finished workload does: GetPod reports the
// container CONTAINER_STATE_EXITED with the code (none when code is
// negative: a node that reported no exit status) while the pod stays
// POD_STATE_RUNNING, until the pod boots again (a StopPod and a StartPod).
func (s *Script) ExitContainer(pod podid.ID, container string, code int) error {
	e := containerExit{}
	if code >= 0 {
		e = containerExit{code: int32(code), hasCode: true}
	}
	return s.n.pods.exitContainer(pod, container, e)
}

// AppendLog adds a line to the log of a container of a pod, which
// StreamContainerLogs serves.
func (s *Script) AppendLog(pod podid.ID, container, line string) error {
	return s.n.pods.appendLog(pod, container, line)
}

// SetKubePorts sets the pod's Kubernetes listener ports, which its guest
// never forwards.
func (s *Script) SetKubePorts(pod podid.ID, ports ...uint32) {
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
func (s *Script) FailPod(pod podid.ID, reason string) error {
	return s.n.pods.fail(pod, reason)
}

// ForgetPod drops a pod's record without an event, as a node that comes
// back without it does (its volumes are let go, its directory stays):
// the control plane finds it missing at its next reconcile.
func (s *Script) ForgetPod(pod podid.ID) error {
	return s.n.pods.forget(pod)
}

// LagPodEvents ends every StreamPodEvents stream with DATA_LOSS, as a
// reader that fell behind feosd's broadcast.
func (s *Script) LagPodEvents() {
	s.n.pods.lagAll()
}

// WatchEvents sends events in one batch to every open watch stream of the
// pod, which numbers them; it returns the number of streams.
func (s *Script) WatchEvents(pod podid.ID, events ...*watchv1.Event) int {
	return s.n.watch.deliver(pod, events)
}

// WatchStale sends an EVENT_KIND_STALE for paths to every open watch
// stream of the pod; it returns the number of streams.
func (s *Script) WatchStale(pod podid.ID, paths ...string) int {
	return s.n.watch.deliver(pod, []*watchv1.Event{{Kind: watchv1.EventKind_EVENT_KIND_STALE, StalePaths: paths}})
}

// Watches is the number of open watch streams of the pod.
func (s *Script) Watches(pod podid.ID) int {
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

func (s *Script) isUnreachable(pod podid.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unreachable[pod]
}

func (s *Script) unrestorableReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unrestorable
}

func (s *Script) podExecFunc(pod podid.ID) ExecFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.podExec[pod]
}

// PortAddrOf returns the address that a port forward to port of the pod
// dials as SetPodPortAddr or SetPortAddr mapped it, the pod's own first;
// false when neither did (the forward dials 127.0.0.1:<port>). A test
// that plays another way into the pod (the ingress's mesh) reaches the
// same server through it.
func (s *Script) PortAddrOf(pod podid.ID, port uint32) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.podPortAddrs[pod][port]; ok {
		return a, true
	}
	a, ok := s.portAddrs[port]
	return a, ok
}

func (s *Script) portAddr(pod podid.ID, port uint32) string {
	if a, ok := s.PortAddrOf(pod, port); ok {
		return a
	}
	return "127.0.0.1:" + strconv.FormatUint(uint64(port), 10)
}

func (s *Script) isKubePort(pod podid.ID, port uint32) bool {
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
