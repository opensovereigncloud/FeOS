// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// VMBusyDetail is the type of the status detail that marks feosd's
// UNAVAILABLE of a microVM that takes no change now, and does later ("the
// VM is busy: <why>", as while Firecracker still writes a snapshot that a
// suspend gave up on; node.md, "The suspend's budget"), and of a call to
// a pod's guest that got no answer because its VM is busy ("the guest of
// pod <id> did not answer within 5s; the VM is busy: <why>"): an Any of
// this type without a value (microvm_service::vmm::BUSY_DETAIL).
const VMBusyDetail = "type.googleapis.com/feos.microvm.v1.VmBusy"

// VMBusy reports whether a node call's error is feosd's busy VM: the call
// changed nothing, and the same call later may. The marker decides, not
// the message.
func VMBusy(err error) bool {
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unavailable {
		return false
	}
	for _, detail := range st.Proto().GetDetails() {
		if detail.GetTypeUrl() == VMBusyDetail {
			return true
		}
	}
	return false
}
