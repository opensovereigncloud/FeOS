// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package agent

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/gardenlinux/feos/go/internal/ingress/wg"
)

// errNoKernelMesh is the answer of the kernel's mesh device off Linux.
var errNoKernelMesh = fmt.Errorf("the mesh device needs Linux: %w", errors.ErrUnsupported)

// KernelMesh is the mesh device in the kernel; off Linux every call
// fails with errors.ErrUnsupported.
type KernelMesh struct{}

var _ MeshDevice = (*KernelMesh)(nil)

// NewKernelMesh returns the kernel's mesh device.
func NewKernelMesh(*slog.Logger) *KernelMesh { return &KernelMesh{} }

// Configure returns errors.ErrUnsupported.
func (*KernelMesh) Configure(MeshConfig) error { return errNoKernelMesh }

// SetPeers returns errors.ErrUnsupported.
func (*KernelMesh) SetPeers([]wg.Peer) error { return errNoKernelMesh }

// Status returns errors.ErrUnsupported.
func (*KernelMesh) Status() (wg.Device, error) { return wg.Device{}, errNoKernelMesh }

// MTU is 0.
func (*KernelMesh) MTU() int { return 0 }
