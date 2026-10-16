// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
