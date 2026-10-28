// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// Hold sends a Hold on the current connection (D11): it holds and
// releases instances, and with Leaving releases every hold of the
// instance. It returns ErrNotConnected without a connection; the caller
// bounds it with ctx.
func (a *Agent) Hold(ctx context.Context, req wire.HoldRequest) (wire.HoldResponse, error) {
	s := a.current()
	if s == nil {
		return wire.HoldResponse{}, ErrNotConnected
	}
	resp, err := s.client.Hold(ctx, &req)
	if err != nil {
		return wire.HoldResponse{}, err
	}
	return *resp, nil
}

// Leave tells the control plane that the instance is shutting down: every
// hold it has is released at once, so the idle timers start, and the
// idle fence stops counting it. The program calls it at the end of a
// drain, while Run still runs.
func (a *Agent) Leave(ctx context.Context) error {
	_, err := a.Hold(ctx, wire.HoldRequest{Leaving: true})
	return err
}
