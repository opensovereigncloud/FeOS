// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/apierr"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// The route door's plain answers.
const (
	textRouteToken  = "route token required"
	textDotDot      = "the path has a .. segment"
	textDot         = "the path has a . segment"
	textNoDoor      = "not available on this instance"
	textRouteFailed = "the route door failed"
)

// RouteTokenHeader carries a route token to the route door beside the
// client's own Authorization, which then reaches the upstream.
const RouteTokenHeader = "X-FeRun-Route-Token"

// mintRouteToken returns a new route token and its hash: 32 random
// bytes, base64url without padding. Only the hash is stored.
func mintRouteToken() (token, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b[:])
	return token, store.HashToken(token), nil
}

// bearer is the token of "Authorization: Bearer <token>", or "".
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[len("Bearer "):])
}

// routeToken is a request's route token and the headers that carried
// it, which stop at the door: RouteTokenHeader's when it is set (the
// client's Authorization then goes on to the upstream), else
// Authorization's bearer token (old clients; Authorization stops).
func routeToken(r *http.Request) (string, []string) {
	if tok := strings.TrimSpace(r.Header.Get(RouteTokenHeader)); tok != "" {
		return tok, []string{RouteTokenHeader}
	}
	return bearer(r), []string{"Authorization", RouteTokenHeader}
}

// routePath is the path the route door forwards: the request's path
// after /api/v1/environments/{env}/http, unescaped and as it was escaped
// ("/" for none).
func routePath(r *http.Request) (path, raw string, err error) {
	parts := strings.SplitN(r.URL.EscapedPath(), "/", 7)
	raw = "/"
	if len(parts) == 7 {
		raw = "/" + parts[6]
	}
	path, err = url.PathUnescape(raw)
	return path, raw, err
}

// dotSegment is ".." or "." when a segment of a path's escaped form is
// that, as it is or decoded once or twice, else "". Each decode is
// lenient, as an upstream that decodes again may be: a valid %XX becomes
// its byte and any other "%" stays, so a "%" that the first decode made
// ("/q/100%25") is a character, not a reason to refuse, and still hides
// no "%2e%2e" beside it.
func dotSegment(raw string) string {
	p := raw
	for i := range 3 {
		for seg := range strings.SplitSeq(p, "/") {
			if seg == "." || seg == ".." {
				return seg
			}
		}
		if i < 2 {
			p = lenientUnescape(p)
		}
	}
	return ""
}

// lenientUnescape decodes each valid %XX of s and leaves any other "%"
// as it is.
func lenientUnescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// matchRoute returns the route whose path is the longest segment-aware
// prefix of raw's segments (each unescaped), and how many segments it
// has; ok false when none matches.
func matchRoute(routes map[string]spec.Route, raw string) (spec.Route, int, bool) {
	segs := strings.Split(strings.TrimPrefix(raw, "/"), "/")
	var best spec.Route
	bestN, found := -1, false
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		r := routes[name]
		prefix := spec.RoutePrefix(r.Path)
		var want []string
		if prefix != "/" {
			want = strings.Split(strings.TrimPrefix(prefix, "/"), "/")
		}
		if len(want) > len(segs) || len(want) <= bestN {
			continue
		}
		ok := true
		for i, w := range want {
			if seg, err := url.PathUnescape(segs[i]); err != nil || seg != w {
				ok = false
				break
			}
		}
		if ok {
			best, bestN, found = r, len(want), true
		}
	}
	return best, bestN, found
}

// stripSegments removes the first n segments of a path's escaped form
// and returns what remains, unescaped and escaped ("" when it needs no
// escaping).
func stripSegments(raw string, n int) (string, string, error) {
	segs := strings.Split(strings.TrimPrefix(raw, "/"), "/")
	rest := "/" + strings.Join(segs[n:], "/")
	path, err := url.PathUnescape(rest)
	if err != nil {
		return "", "", err
	}
	return path, rawOf(path, rest), nil
}

// rawOf is the escaped form of a path, "" when it is the default
// escaping.
func rawOf(path, raw string) string {
	if (&url.URL{Path: path}).EscapedPath() == raw {
		return ""
	}
	return raw
}

// ServeRoute is the route door, ANY
// /api/v1/environments/{env}/http/{path...}, an auth class of its own: a
// route token of the environment in X-FeRun-Route-Token, else as Bearer
// (401 "route token required"; a tenant token is refused the same way),
// which stops here (from X-FeRun-Route-Token, the client's Authorization
// goes on to the upstream), the longest segment-aware
// prefix match over the routes of the realised release (the latest when
// none is), and the request forwarded through the port door to a ready
// instance of the route's service, round-robin, or, when none is ready,
// to a paused or hibernated one (the forward wakes it: scale from zero);
// none at all is 503 with Retry-After 5. The port is the one the chosen
// instance's own release gives the route's port name (the matched
// release's when that release has none). An attached service's requests
// go to its sandbox, only while that is the environment owner's and the
// incarnation its process was started in (else 503). The path is
// stripped of the route's path when the route says so; the query is
// kept. Its answers are plain text; a failure inside ferund is logged
// and answered 500 "the route door failed", without its cause.
func (s *Service) ServeRoute(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tok, creds := routeToken(r)
	if tok == "" {
		http.Error(w, textRouteToken, http.StatusUnauthorized)
		return
	}
	hash := store.HashToken(tok)
	rt, err := s.st.ResolveRouteToken(ctx, hash)
	if errors.Is(err, store.ErrNoRow) {
		http.Error(w, textRouteToken, http.StatusUnauthorized)
		return
	}
	if err != nil {
		s.routeFailed(w, r, err)
		return
	}
	e, err := s.st.GetEnvironment(ctx, rt.EnvID)
	if errors.Is(err, store.ErrNoRow) {
		http.Error(w, textNoEnvironment, http.StatusNotFound)
		return
	}
	if err != nil {
		s.routeFailed(w, r, err)
		return
	}
	ref := r.PathValue("env")
	if id, ok := parseUUID(ref); (ok && id != e.ID) || (!ok && ref != e.Name) || e.Deleting {
		http.Error(w, textNoEnvironment, http.StatusNotFound)
		return
	}
	if s.touches.due(hash) {
		if err := s.st.TouchRouteToken(ctx, hash); err != nil {
			s.log.Warn("touch a route token", "env", e.ID, "err", err)
		}
	}
	path, raw, err := routePath(r)
	if err != nil {
		http.Error(w, "the path is not escaped correctly", http.StatusBadRequest)
		return
	}
	switch dotSegment(raw) {
	case "..":
		http.Error(w, textDotDot, http.StatusBadRequest)
		return
	case ".":
		http.Error(w, textDot, http.StatusBadRequest)
		return
	}
	number := e.Realised
	if number == 0 {
		number = e.Release
	}
	var sp *spec.Spec
	if number > 0 {
		if sp, err = s.releaseSpec(ctx, e, number); err != nil {
			s.routeFailed(w, r, err)
			return
		}
	}
	var route spec.Route
	var n int
	ok := false
	if sp != nil {
		route, n, ok = matchRoute(sp.Routes, raw)
	}
	if !ok {
		http.Error(w, "no route for "+path, http.StatusNotFound)
		return
	}
	svc := sp.Services[route.Service]
	port, portOK := spec.PortNumber(svc.Ports, route.Port)
	upPath, upRaw := path, rawOf(path, raw)
	if route.Strip {
		if upPath, upRaw, err = stripSegments(raw, n); err != nil {
			http.Error(w, "the path is not escaped correctly", http.StatusBadRequest)
			return
		}
	}
	if s.d.Ports == nil {
		http.Error(w, textNoDoor, http.StatusServiceUnavailable)
		return
	}
	if svc.Runtime == spec.RuntimeAttached {
		if !portOK {
			http.Error(w, "route "+route.Path+" names no port of "+route.Service, http.StatusBadGateway)
			return
		}
		id, refusal, err := s.attachedSandbox(ctx, e, route.Service)
		if err != nil {
			s.routeFailed(w, r, err)
			return
		}
		if refusal != "" {
			w.Header().Set("Retry-After", "5")
			http.Error(w, refusal, http.StatusServiceUnavailable)
			return
		}
		s.d.Ports.ForwardTo(w, r, id, port, upPath, upRaw, creds)
		return
	}
	inst, err := s.pickInstance(ctx, e.ID, route.Service)
	if err != nil {
		s.routeFailed(w, r, err)
		return
	}
	if inst.id == uuid.Nil {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "no ready instance of "+route.Service, http.StatusServiceUnavailable)
		return
	}
	// The instance listens on the port of its own release, which a
	// rolling release may have changed.
	if inst.release != number && inst.release > 0 {
		own, err := s.releaseSpec(ctx, e, inst.release)
		if err != nil && !isNotFound(err) {
			s.routeFailed(w, r, err)
			return
		}
		if own != nil {
			if p, ok := spec.PortNumber(own.Services[route.Service].Ports, route.Port); ok {
				port, portOK = p, true
			}
		}
	}
	if !portOK {
		http.Error(w, "route "+route.Path+" names no port of "+route.Service, http.StatusBadGateway)
		return
	}
	s.d.Ports.ForwardTo(w, r, inst.id, port, upPath, upRaw, creds)
}

// attachedSandbox is the sandbox an attached service's requests go to:
// the one its controller row names, while that sandbox exists, is the
// environment owner's and is no instance row (§13.6); readiness decides
// the rest. A new incarnation of the owner's sandbox under the same ID
// (FeCode replaces a lost workspace sandbox so) is routed to, as the
// controller starts the service there anew. Otherwise refusal says why
// not: a sandbox ID is free again once its sandbox is deleted, and
// another tenant may take it, which the spec validated at apply never
// sees.
func (s *Service) attachedSandbox(ctx context.Context, e store.Environment, service string) (uuid.UUID, string, error) {
	a, err := s.st.GetAttached(ctx, e.ID, service)
	if errors.Is(err, store.ErrNoRow) {
		return uuid.Nil, "no ready instance of " + service, nil
	}
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("attached service %s: %w", service, err)
	}
	sb, err := s.st.GetSandbox(ctx, a.SandboxID)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return uuid.Nil, "", fmt.Errorf("sandbox %s: %w", a.SandboxID, err)
	}
	if err != nil || sb.Owner != e.Owner || sb.IsInstance() {
		return uuid.Nil, fmt.Sprintf("sandbox %s of service %s is gone or not this tenant's", a.SandboxID, service), nil
	}
	return sb.ID, "", nil
}

// picked is the instance a request goes to: its sandbox and its release.
type picked struct {
	id      uuid.UUID
	release int
}

// pickInstance chooses the instance of an image service a request goes
// to: a ready one whose sandbox runs, round-robin; else a paused or
// hibernated one, which the forward wakes; a zero id when there is none.
func (s *Service) pickInstance(ctx context.Context, envID uuid.UUID, service string) (picked, error) {
	rows, err := s.st.ListEnvInstanceRows(ctx, envID)
	if err != nil {
		return picked{}, err
	}
	var ready, asleep []picked
	for _, row := range rows {
		i, sb := row.Instance, row.Sandbox
		if i.Kind != spec.KindService || i.Object != service || i.Phase == model.PhaseRetiring {
			continue
		}
		p := picked{id: sb.ID, release: i.Release}
		switch {
		case i.Phase == model.PhaseReady && sb.ClientState == model.StateRunning && sb.Op == "":
			ready = append(ready, p)
		case i.Phase == model.PhaseReady && sb.ClientState == model.StateRunning:
			// Busy with an op (a pause on its way): only when nothing
			// else answers.
			asleep = append(asleep, p)
		case sb.ClientState == model.StatePaused || sb.ClientState == model.StateHibernated ||
			sb.ClientState == model.StatePausing || sb.ClientState == model.StateHibernating:
			asleep = append(asleep, p)
		}
	}
	if len(ready) > 0 {
		return ready[s.next.Add(1)%uint64(len(ready))], nil
	}
	if len(asleep) > 0 {
		return asleep[0], nil
	}
	return picked{}, nil
}

// routeFailed answers a route door request that failed inside ferund.
// The door serves anonymous clients, so the cause (database addresses,
// sandbox IDs) goes to the log only.
func (s *Service) routeFailed(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apierr.Error
	if errors.As(err, &ae) && ae.Status < 500 {
		http.Error(w, ae.Msg, ae.Status)
		return
	}
	s.log.Warn("route door request failed", "path", r.URL.Path, "err", err)
	http.Error(w, textRouteFailed, http.StatusInternalServerError)
}
