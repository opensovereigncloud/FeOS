// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ironcore-dev/feos/go/internal/model"
)

// NodeError is a call into a pod's guest that failed on its node: the gRPC
// status of the call. It reads "node <n>: <message>", unless the message
// names the node already (the nodes' port's own refusals do).
type NodeError struct {
	Node   string
	Status *status.Status
}

func (e *NodeError) Error() string {
	msg := e.Status.Message()
	prefix := "node " + e.Node
	if strings.HasPrefix(msg, prefix+":") || strings.HasPrefix(msg, prefix+" ") {
		return msg
	}
	return prefix + ": " + msg
}

// GRPCStatus lets status.Code see the call's code through the error.
func (e *NodeError) GRPCStatus() *status.Status { return e.Status }

// nodeError makes the NodeError of a failed call on node.
func nodeError(node string, err error) *NodeError {
	return &NodeError{Node: node, Status: status.Convert(err)}
}

// ErrExecCap is a call refused because the sandbox has as many execs
// running on this instance as it may (§4: 64).
var ErrExecCap = errors.New("too many execs at once")

// ErrCancelled is a call the control plane cancelled (ferun_cancel of kind
// exec): the sandbox's node is being drained by force.
var ErrCancelled = errors.New("the call was cancelled by the control plane: the sandbox's node is being drained")

// TimeoutError is a call into the guest that ran past its deadline. It
// reads "timeout after <N>s", the deadline in seconds.
type TimeoutError struct{ Seconds int }

func (e *TimeoutError) Error() string { return fmt.Sprintf("timeout after %ds", e.Seconds) }

// Retryable reports whether a failed call is worth repeating shortly, as
// the REST tool routes tell with Retry-After: no capacity, a node or
// instance that is unavailable or stopping, a stream or tool budget or the
// exec cap exhausted, a call cut by a forced drain.
func Retryable(err error) bool {
	switch {
	case errors.Is(err, model.ErrNoCapacity), errors.Is(err, model.ErrNodeUnavailable),
		errors.Is(err, model.ErrStopping), errors.Is(err, ErrExecCap), errors.Is(err, ErrCancelled):
		return true
	}
	var ne *NodeError
	if errors.As(err, &ne) {
		switch ne.Status.Code() {
		case codes.ResourceExhausted, codes.Unavailable:
			return true
		}
	}
	return false
}

// guestUnreachable reports whether err is feosd's answer for a pod whose
// guest does not answer: UNAVAILABLE "the guest of pod <id>: ...".
func guestUnreachable(err error) bool {
	s, ok := status.FromError(err)
	return ok && s.Code() == codes.Unavailable && strings.HasPrefix(s.Message(), "the guest of pod")
}

// notOnPod reports whether err says the pod is not where the lease put it,
// or not running: FAILED_PRECONDITION "pod <id> is <state>" or NOT_FOUND
// "no pod <id>". The sandbox is synchronised and the call made once more.
func notOnPod(err error) bool {
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.NotFound:
		return true
	}
	return false
}

// statusOf is the gRPC status of err, if it carries one.
func statusOf(err error) (*status.Status, bool) {
	return status.FromError(err)
}
