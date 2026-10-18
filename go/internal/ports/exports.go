// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ports

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/model"
)

// What the environments (ADR 0018) use of the door (envdeps.Ports): the
// readiness checks of their instances and attached services, which take
// no hold and count as no activity, and the route door's forward.

// errProbeCap is a check that found every slot of the sandbox's cap taken:
// it fails at once rather than wait for one, so that a busy sandbox's
// checks do not stall.
var errProbeCap = errors.New("every forwarded connection into the sandbox on this instance is in use")

// DialPod opens a port-forward to the pod's port of the lease's sandbox,
// answered by the guest's ready (something listens): a TCP readiness
// check, or the connection of an HTTP one. It takes no hold and touches
// nothing; the dial has its own timeout (2 s) and never waits for a slot
// of the sandbox's stream cap (errProbeCap when none is free once the
// idle pooled connections are closed). The connection lives until Close.
func (h *Handler) DialPod(ctx context.Context, lease model.Lease, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d is out of range", port)
	}
	ctx, cancel := context.WithTimeout(ctx, h.t.probeTimeout)
	defer cancel()
	release, ok := h.trySlot(lease.SandboxID)
	if !ok {
		return nil, errProbeCap
	}
	c, err := h.openStream(ctx, tkey{lease.SandboxID, port}, lease.Node, release)
	if err != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("port %d did not answer within %s: %w", port, h.t.probeTimeout, err)
	}
	return c, err
}

// Probe GETs path (with its query) from the pod's port over a
// port-forward, with Host localhost:<port> (dev servers check it) and
// Connection: close, and returns the answer's status code; the body is
// not read. It takes no hold and touches nothing; the whole check has its
// own timeout (2 s) and never waits for a slot of the sandbox's stream
// cap.
func (h *Handler) Probe(ctx context.Context, lease model.Lease, port int, path string) (int, error) {
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return 0, fmt.Errorf("probe path %q does not begin with /", path)
	}
	host := "localhost:" + strconv.Itoa(port)
	req, err := http.NewRequest(http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		return 0, fmt.Errorf("probe path %q: %w", path, err)
	}
	req.Close = true
	req.Header.Set("User-Agent", "ferun-probe")
	ctx, cancel := context.WithTimeout(ctx, h.t.probeTimeout)
	defer cancel()
	conn, err := h.DialPod(ctx, lease, port)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	// The stream ignores deadlines: the end of ctx closes it.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	fail := func(err error) (int, error) {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("GET %s on port %d did not answer within %s", path, port, h.t.probeTimeout)
		}
		return 0, fmt.Errorf("GET %s on port %d: %w", path, port, err)
	}
	if err := req.Write(conn); err != nil {
		return fail(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fail(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// textDotDot answers a route door's path with a ".." segment.
const textDotDot = "the path has a .. segment"

// ForwardTo forwards the request to the sandbox's port as the door does
// for a sandbox token, for the environments' route door, which has
// authorised the request: the sandbox is brought up and held with a
// forward hold for the request, and touched after it (a route to a paused
// or hibernated instance wakes it); the Host the client named goes on,
// Authorization stops here, upgrades and streams are kept, and a port of
// the sandbox's Kubernetes bindings is refused (403). upstreamPath is the
// path the pod sees ("" is "/"), rawPath its escaped form ("" when it
// needs none); the query is kept. A path with a ".." segment, also one
// percent-encoded, is refused with 400. Every answer of the door's own is
// plain text.
func (h *Handler) ForwardTo(w http.ResponseWriter, r *http.Request, sandboxID uuid.UUID, port int, upstreamPath, rawPath string) {
	if port < 1 || port > 65535 {
		reply{http.StatusBadGateway, fmt.Sprintf("port %d is out of range", port), 0}.write(w)
		return
	}
	if dotDot(upstreamPath, rawPath) {
		reply{http.StatusBadRequest, textDotDot, 0}.write(w)
		return
	}
	if !strings.HasPrefix(upstreamPath, "/") {
		upstreamPath = "/" + upstreamPath
		if rawPath != "" {
			rawPath = "/" + rawPath
		}
	}
	if rp := h.kubeRefusal(r.Context(), sandboxID, port); rp != nil {
		rp.write(w)
		return
	}
	h.forward(w, r, sandboxID, target{id: sandboxID.String(), port: port, path: upstreamPath, rawPath: rawPath})
}

// dotDot reports whether a path has a ".." segment: in the path, in its
// escaped form once unescaped (an encoded "/" splits a segment), or in
// either unescaped once more (a server that decodes twice).
func dotDot(path, rawPath string) bool {
	paths := []string{path}
	if rawPath != "" {
		dec, err := url.PathUnescape(rawPath)
		if err != nil {
			return true
		}
		paths = append(paths, dec)
	}
	for _, p := range paths {
		if hasDotDot(p) {
			return true
		}
		if dec, err := url.PathUnescape(p); err == nil && hasDotDot(dec) {
			return true
		}
	}
	return false
}

func hasDotDot(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
