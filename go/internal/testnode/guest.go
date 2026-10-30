// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
)

// PodIDMetadata is the gRPC metadata that sends a call to a pod's guest.
const PodIDMetadata = "feos-pod-id"

// forwarded are the services feosd forwards to a pod's guest (forward.rs),
// with PortForwardService, which feosd is to forward too (§19.3 of the
// design).
var forwarded = []string{
	"/feos.container.v1.ContainerService/",
	"/feos.image.v1.ImageService/",
	"/feos.tool.v1.ToolService/",
	"/feos.watch.v1.WatchService/",
	"/feos.portforward.v1.PortForwardService/",
}

func isForwarded(method string) bool {
	for _, prefix := range forwarded {
		if strings.HasPrefix(method, prefix) {
			return true
		}
	}
	return false
}

type guestKey struct{}

func guestFrom(ctx context.Context) (*guest, error) {
	g, ok := ctx.Value(guestKey{}).(*guest)
	if !ok {
		return nil, status.Error(codes.Internal, "the call reached a guest service without a pod")
	}
	return g, nil
}

// route applies the script's hook and the routing of feosd's forward
// layer: a call with feos-pod-id goes to that pod's guest, if its service
// is forwarded and the pod is reachable; a call without it goes to the
// node's own services, which do not include the guest's.
func (n *Node) route(ctx context.Context, method string, req any) (context.Context, error) {
	if h := n.script.hook(method); h != nil {
		if err := h(ctx, method, req); err != nil {
			if _, ok := status.FromError(err); ok {
				return ctx, err
			}
			return ctx, status.Error(codes.Unknown, err.Error())
		}
	}
	md, _ := metadata.FromIncomingContext(ctx)
	ids := md.Get(PodIDMetadata)
	if len(ids) == 0 {
		if isForwarded(method) {
			return ctx, status.Errorf(codes.Unimplemented, "%s is served in pods only: the test node runs no containers of its own", method)
		}
		return ctx, nil
	}
	if !isForwarded(method) {
		return ctx, status.Errorf(codes.Unimplemented, "%s is not served for pods", method)
	}
	g, err := n.pods.reach(ids[0])
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, guestKey{}, g), nil
}

func (n *Node) unaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, err := n.route(ctx, info.FullMethod, req)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (n *Node) streamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, err := n.route(ss.Context(), info.FullMethod, nil)
	if err != nil {
		return err
	}
	return handler(srv, &routedStream{ServerStream: ss, ctx: ctx})
}

// routedStream is a server stream with the context the routing made.
type routedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *routedStream) Context() context.Context { return s.ctx }

// containerService is the guest's ContainerService: Exec runs the command
// on the machine of the test node, in the pod's directory, or as the
// script says. The other calls are not served by the fake.
type containerService struct {
	containerv1.UnimplementedContainerServiceServer
	n *Node
}

// execChunk is the largest output frame.
const execChunk = 32 << 10

// frameSender sends an Exec's frames; the output of stdout and stderr
// arrives from two goroutines.
type frameSender struct {
	mu     sync.Mutex
	stream grpc.ServerStreamingServer[containerv1.ExecStreamFrame]
	err    error
}

func (f *frameSender) send(frame *containerv1.ExecStreamFrame) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.err = f.stream.Send(frame)
	return f.err
}

func (f *frameSender) output(p []byte, stderr bool) error {
	for len(p) > 0 {
		chunk := p[:min(len(p), execChunk)]
		p = p[len(chunk):]
		frame := &containerv1.ExecStreamFrame{Frame: &containerv1.ExecStreamFrame_Stdout{Stdout: append([]byte(nil), chunk...)}}
		if stderr {
			frame.Frame = &containerv1.ExecStreamFrame_Stderr{Stderr: append([]byte(nil), chunk...)}
		}
		if err := f.send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (f *frameSender) Stdout(p []byte) error { return f.output(p, false) }
func (f *frameSender) Stderr(p []byte) error { return f.output(p, true) }

// streamWriter is an io.Writer of one output stream of a command.
type streamWriter struct {
	f      *frameSender
	stderr bool
}

func (w streamWriter) Write(p []byte) (int, error) {
	if err := w.f.output(p, w.stderr); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *containerService) Exec(req *containerv1.ExecRequest, stream grpc.ServerStreamingServer[containerv1.ExecStreamFrame]) error {
	ctx := stream.Context()
	g, err := guestFrom(ctx)
	if err != nil {
		return err
	}
	container := req.GetContainerId()
	if container == "" {
		container = "main"
	}
	if !g.containers[container] {
		return status.Errorf(codes.NotFound, "no container %s", container)
	}
	if len(req.GetCommand()) == 0 {
		return status.Error(codes.InvalidArgument, "no command")
	}
	out := &frameSender{stream: stream}
	for _, f := range []ExecFunc{c.n.script.podExecFunc(g.uid), c.n.script.execFunc()} {
		if f == nil {
			continue
		}
		code, ok, err := f(ctx, g.uid, req, out)
		if err != nil {
			return err
		}
		if ok {
			return out.send(&containerv1.ExecStreamFrame{Frame: &containerv1.ExecStreamFrame_ExitCode{ExitCode: code}})
		}
	}
	if c.n.cfg.FakeProcesses {
		code, ok, err := c.n.procs.exec(g, req.GetCommand(), out)
		if err != nil {
			return err
		}
		if ok {
			return out.send(&containerv1.ExecStreamFrame{Frame: &containerv1.ExecStreamFrame_ExitCode{ExitCode: code}})
		}
	}
	if c.n.cfg.FakeExec || g.envPod {
		// Nothing runs: an environment's probes and provisioning pass.
		return out.send(&containerv1.ExecStreamFrame{Frame: &containerv1.ExecStreamFrame_ExitCode{ExitCode: 0}})
	}
	code, err := c.run(ctx, g, container, req, out)
	if err != nil {
		return err
	}
	return out.send(&containerv1.ExecStreamFrame{Frame: &containerv1.ExecStreamFrame_ExitCode{ExitCode: code}})
}

// run runs the command as given (the control plane sends ["bash", "-c",
// cmd]) with the pod's directory standing in for the container's root:
// working_dir is taken beneath it. Ending the call kills the command's
// process group.
func (c *containerService) run(ctx context.Context, g *guest, container string, req *containerv1.ExecRequest, out *frameSender) (int32, error) {
	rel, err := toRel(req.GetWorkingDir())
	if err != nil {
		return 0, status.Error(codes.InvalidArgument, err.Error())
	}
	dir := filepath.Join(g.dir, filepath.FromSlash(rel))
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return 0, status.Errorf(codes.InvalidArgument, "working directory %q is not a directory in the container", req.GetWorkingDir())
	}
	argv := req.GetCommand()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = execEnv(g.dir, dir, g.env[container], req.GetEnv())
	cmd.Stdout = streamWriter{f: out}
	cmd.Stderr = streamWriter{f: out, stderr: true}
	// A command that leaves a child holding its output open still ends.
	cmd.WaitDelay = 2 * time.Second
	ownProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		if serr := out.Stderr(fmt.Appendf(nil, "%s: %v\n", argv[0], err)); serr != nil {
			return 0, serr
		}
		return 127, nil
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return 0, status.FromContextError(ctx.Err()).Err()
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) && !errors.Is(err, exec.ErrWaitDelay) {
		return 0, status.Errorf(codes.Internal, "exec: %v", err)
	}
	return exitCode(cmd.ProcessState), nil
}

// envLabel is the label of an environment's pods (model.LabelEnvironment).
const envLabel = "ferun.environment"

// The guest's bounds of a StreamContainerLogs tail: maxTailLines is the
// most lines tail_lines sends (MAX_TAIL_LINES), maxTailBytes how far back
// from the log's end it reaches (MAX_TAIL_BYTES), each line counted with
// its newline, and maxLinePart the parts of a line (MAX_LINE) that a line
// cut at that bound keeps.
const (
	maxTailLines = 10000
	maxTailBytes = 4 << 20
	maxLinePart  = 64 << 10
)

// tailOf is what a tail of n lines (n > 0) of a log sends, as the guest's
// does: the last n lines, at most maxTailLines, that start within
// maxTailBytes of the log's end. A line that starts further back goes out
// cut to its last whole maxLinePart parts within those bytes, as one line
// (the test node does not split lines into the guest's parts), or not at
// all when not one part fits.
func tailOf(lines []string, n int) []string {
	n = min(n, maxTailLines)
	first := max(0, len(lines)-n)
	left := maxTailBytes
	i := len(lines)
	for i > first && len(lines[i-1])+1 <= left {
		left -= len(lines[i-1]) + 1
		i--
	}
	out := slices.Clone(lines[i:])
	if i == first {
		return out
	}
	// lines[i-1] starts further back than the bytes reach: its own bytes
	// within them are all that is left but its newline.
	l := lines[i-1]
	if whole := (left - 1) / maxLinePart; whole > 0 {
		out = slices.Insert(out, 0, l[len(l)-whole*maxLinePart:])
	}
	return out
}

// StreamContainerLogs serves a container's log lines (Script.AppendLog):
// the last tail_lines of them when above 0 within the guest's bounds
// (tailOf: at most 10000 lines and 4 MiB), else all with tail, else none;
// with follow the stream then stays open until the caller ends it,
// without new lines.
func (c *containerService) StreamContainerLogs(req *containerv1.StreamContainerLogsRequest, stream grpc.ServerStreamingServer[containerv1.LogEntry]) error {
	ctx := stream.Context()
	g, err := guestFrom(ctx)
	if err != nil {
		return err
	}
	container := req.GetContainerId()
	if container == "" {
		container = "main"
	}
	if !g.containers[container] {
		return status.Errorf(codes.NotFound, "no container %s", container)
	}
	lines := c.n.pods.containerLog(g.id, container)
	switch n := int(min(req.GetTailLines(), maxTailLines)); {
	case n > 0:
		lines = tailOf(lines, n)
	case !req.GetTail():
		lines = nil
	}
	for _, l := range lines {
		if err := stream.Send(decodeEntry(l)); err != nil {
			return err
		}
	}
	if req.GetFollow() {
		<-ctx.Done()
		return status.FromContextError(ctx.Err()).Err()
	}
	return nil
}

// entryMark starts a log line that Script.AppendLogEntry stored with its
// source and partial flag: the mark, 'P' or 'F', the source's number,
// the text. A line of AppendLog is sent as it is, without a source.
const entryMark = "\x00"

func encodeEntry(text string, source containerv1.LogEntry_Source, partial bool) string {
	flag := "F"
	if partial {
		flag = "P"
	}
	return entryMark + flag + strconv.Itoa(int(source)) + text
}

// decodeEntry is the entry a stored log line is sent as.
func decodeEntry(l string) *containerv1.LogEntry {
	if !strings.HasPrefix(l, entryMark) || len(l) < 3 {
		return &containerv1.LogEntry{Line: []byte(l)}
	}
	src, _ := strconv.Atoi(l[2:3])
	return &containerv1.LogEntry{Line: []byte(l[3:]), Source: containerv1.LogEntry_Source(src), Partial: l[1] == 'P'}
}

// defaultPath is a command's PATH when the test node's own has none.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// execEnv is the environment of a command, as a guest's Exec gives it: the
// container's, then the request's, each overriding what came before. The
// test node's own environment stays out of it, which may hold its join
// token and the credentials of whoever runs it; only its PATH comes in, so
// that the tools of the machine are found. HOME is the pod's directory,
// the container's root here, and PWD the working directory.
func execEnv(root, dir string, layers ...map[string]string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = defaultPath
	}
	env := map[string]string{"PATH": path, "HOME": root, "PWD": dir}
	for _, layer := range layers {
		maps.Copy(env, layer)
	}
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}
