// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/nodelink"
)

// kindError is an error with a text of its own that is one of the
// sentinels of internal/model (errors.Is), so that a door can map it to
// its status while the client reads the text alone.
type kindError struct {
	msg  string
	kind error
}

func (e *kindError) Error() string { return e.msg }
func (e *kindError) Unwrap() error { return e.kind }

// errorf makes an error that reads as the formatted text and is kind.
func errorf(kind error, format string, args ...any) error {
	return &kindError{msg: fmt.Sprintf(format, args...), kind: kind}
}

// laterError is a kindError that says when a retry is worth it: the
// doors send it as Retry-After (apierr.RetryAfterOf).
type laterError struct {
	kindError
	after time.Duration
}

// RetryAfter is how long the caller should wait before it tries again.
func (e *laterError) RetryAfter() time.Duration { return e.after }

// laterf makes an error that reads as the formatted text, is kind, and
// asks for a retry after after.
func laterf(after time.Duration, kind error, format string, args ...any) error {
	return &laterError{kindError: kindError{msg: fmt.Sprintf(format, args...), kind: kind}, after: after}
}

// refusal is a gate that refused an admission or a placement candidate.
type refusal struct {
	node string
	gate int
	text string
}

func (r *refusal) Error() string { return r.text }

// asRefusal returns the refusal err is, if it is one.
func asRefusal(err error) (*refusal, bool) {
	var r *refusal
	ok := errors.As(err, &r)
	return r, ok
}

// grpcCode is the gRPC code of a node call's error.
func grpcCode(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return codes.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return codes.Canceled
	}
	return status.Code(err)
}

// callText is the message of a node call's error, without the gRPC
// decoration, for descriptions.
func callText(err error) string {
	if err == nil {
		return ""
	}
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}

// vmBusyDetail marks feosd's busy VM (nodelink.VMBusyDetail).
const vmBusyDetail = nodelink.VMBusyDetail

// busyRetry is when a caller told that the sandbox's VM is busy should
// come back.
const busyRetry = 30 * time.Second

// vmBusy reports whether a node call's error is feosd's busy VM
// (nodelink.VMBusy): the call changed nothing, and the same call later may.
func vmBusy(err error) bool { return nodelink.VMBusy(err) }
