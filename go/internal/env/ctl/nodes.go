// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
)

// errNoConns: the controller was made without the nodes' connections.
var errNoConns = errors.New("no connections to nodes")

// conn is the node's current connection, waited for at most the node-wait.
func (c *Controller) conn(ctx context.Context, node string) (grpc.ClientConnInterface, error) {
	if c.d.Conns == nil {
		return nil, errNoConns
	}
	return c.d.Conns.Conn(ctx, node)
}

// connected reports whether this instance has a connection to node.
func (c *Controller) connected(node string) bool {
	return c.d.Conns != nil && node != "" && c.d.Conns.Connected(node)
}

// getPod reads a pod from its node.
func (c *Controller) getPod(ctx context.Context, node string, id uuid.UUID) (*podv1.PodInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, node)
	if err != nil {
		return nil, err
	}
	return podv1.NewPodServiceClient(cc).GetPod(ctx, &podv1.GetPodRequest{PodId: id.String()})
}

// mainContainer is the status of a pod's container "main" (nil when the
// node reports none).
func mainContainer(info *podv1.PodInfo) *podv1.ContainerStatus {
	for _, c := range info.GetContainers() {
		if c.GetName() == "main" {
			return c
		}
	}
	return nil
}

// containerLog reads the last lines lines of the main container's log of
// the lease's pod (tail_lines, no follow). A node that does not serve
// container logs is ErrNoContainerLogs.
func (c *Controller) containerLog(ctx context.Context, l model.Lease, lines int) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, l.Node)
	if err != nil {
		return nil, err
	}
	stream, err := containerv1.NewContainerServiceClient(cc).StreamContainerLogs(nodelink.PodContext(ctx, l.SandboxID),
		&containerv1.StreamContainerLogsRequest{ContainerId: "main", TailLines: uint32(lines)})
	if err != nil {
		return nil, logError(err)
	}
	var out []string
	for {
		e, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, logError(err)
		}
		// A line over 64 KiB comes in parts; a node that predates
		// tail_lines sends the whole log: the last lines are kept.
		out = append(out, strings.TrimRight(string(e.GetLine()), "\r\n"))
		if len(out) > 2*lines {
			out = append(out[:0], out[len(out)-lines:]...)
		}
	}
	if len(out) > lines {
		out = out[len(out)-lines:]
	}
	return out, nil
}

// logError maps a node's Unimplemented to ErrNoContainerLogs.
func logError(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return ErrNoContainerLogs
	}
	return err
}

// logTailLines is how much of a log an incident's evidence and a job's
// run keep.
const logTailLines = 50
