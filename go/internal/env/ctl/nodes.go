// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
	"github.com/ironcore-dev/feos/go/internal/podid"
	"github.com/ironcore-dev/feos/go/internal/store"
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
func (c *Controller) getPod(ctx context.Context, node string, id podid.ID) (*podv1.PodInfo, error) {
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
// the lease's pod (tail_lines, no follow), within the budget b: each line
// cut to b.line, the oldest dropped beyond b.total. A node that does not
// serve container logs is ErrNoContainerLogs.
func (c *Controller) containerLog(ctx context.Context, l model.Lease, lines int, b logBudget) ([]string, error) {
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
	t := logTail{b: b, max: lines}
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
		t.add(strings.TrimRight(string(e.GetLine()), "\r\n"))
	}
	return t.result(), nil
}

// logBudget bounds what is read or kept of a log in bytes (§13.7): each
// line is cut to line bytes (its end replaced by cutMark), and the oldest
// lines are dropped while the lines hold more than total bytes.
type logBudget struct{ line, total int }

var (
	// answerLog bounds a logs answer (LogTail): lines of 8 KiB, 4 MiB.
	answerLog = logBudget{line: 8 << 10, total: 4 << 20}
	// storedLog bounds what a job's run and an incident's evidence keep
	// of a log: lines of 2 KiB, 64 KiB.
	storedLog = logBudget{line: 2 << 10, total: 64 << 10}
)

// cutMark ends a line cut to its budget.
const cutMark = "… (cut)"

// cut cuts s to the budget's line bytes, cutMark included, at a rune's
// start.
func (b logBudget) cut(s string) string {
	if len(s) <= b.line {
		return s
	}
	n := max(b.line-len(cutMark), 0)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + cutMark
}

// keep is the last lines within the budget: each cut, the oldest dropped
// beyond the total.
func (b logBudget) keep(lines []string) []string {
	if lines == nil {
		return nil
	}
	t := logTail{b: b, max: len(lines)}
	for _, l := range lines {
		t.add(l)
	}
	return t.result()
}

// logTail collects the last max lines of a log within a budget, holding
// no more than that while it reads.
type logTail struct {
	b     logBudget
	max   int
	lines []string
	// head is the oldest kept line; bytes what the kept lines hold.
	head, bytes int
}

func (t *logTail) add(s string) {
	s = t.b.cut(s)
	t.lines = append(t.lines, s)
	t.bytes += len(s)
	for len(t.lines)-t.head > max(t.max, 1) || (t.bytes > t.b.total && len(t.lines)-t.head > 1) {
		t.bytes -= len(t.lines[t.head])
		t.lines[t.head] = ""
		t.head++
	}
	if t.head > 64 && 2*t.head > len(t.lines) {
		t.lines = append(t.lines[:0], t.lines[t.head:]...)
		t.head = 0
	}
}

// result is the kept lines, oldest first; nil when none came.
func (t *logTail) result() []string {
	if t.lines == nil {
		return nil
	}
	return slices.Clone(t.lines[t.head:])
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

// storedTail is what an incident's evidence and a job's run keep of an
// instance's log (§13.7): its last logTailLines lines, redacted of the
// environment's secret values and addon credentials (envSecretValues),
// then cut to storedLog. They are read with the answer's line budget and
// redacted before the cut, so that no part of a value the cut splits is
// kept; nil when the log cannot be read or is empty.
func (c *Controller) storedTail(ctx context.Context, v *view, sb store.Sandbox) []string {
	tail, err := c.containerLog(ctx, lease(sb), logTailLines, answerLog)
	if err != nil || len(tail) == 0 {
		return tail
	}
	secrets := c.envSecretValues(ctx, v, sb.PodTemplate)
	for i := range tail {
		tail[i] = redact(tail[i], secrets)
	}
	return storedLog.keep(tail)
}
