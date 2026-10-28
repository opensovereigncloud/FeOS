// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package mcpsrv is the MCP front door at /mcp: the tool baseline as an MCP
// server, for agents, whose sandbox token is the sandbox.
//
// The go-sdk's Streamable HTTP handler runs stateless, so that any instance
// serves any request: the bearer is resolved on every request, and the
// factory builds a server for its sandbox. The handler is wrapped by the
// door's own class of answers, which are not the REST API's JSON: a foreign
// Origin, a GET, a missing, unknown or unresolvable bearer each have their
// answer that the SDK's clients act on. An unknown token on a request other
// than initialize or server/discover is answered like a session the server
// no longer knows, so that a client drops its session and initialises
// again (and gets 401 then).
//
// Session ids exist only to cancel: an initialize answer carries a signed,
// self-describing Mcp-Session-Id (an HMAC with the cluster's mcp-session
// secret over the token's hash and a nonce) that any instance verifies. A
// notifications/cancelled sent with a verified session id, which may reach
// another instance than its call, is broadcast with ferun_cancel; every
// instance cancels the call it runs under that session and request id.
//
// A tool call runs through tools.Runner.CallKeyed, with progress
// notifications every 10 s and per output chunk when the client sent a
// progress token, and SSE keepalive comments every 25 s while it runs, so
// that balancers with an idle timeout keep the response open. A call
// without an id is bound to its request: when the request ends, so does
// the call.
//
// A call of a tool with a side effect whose _meta carries "ferun.callId"
// is a call with an id (ADR 0023): it runs on a context of its own and
// outlives its request, which only waits for it. A repeat of the id with
// the same call attaches to it or reads its record. Its result carries
// _meta "ferun.callId", "ferun.callState" and "ferun.callReplayed"; the
// initialize and server/discover answers advertise the tools, the
// retention and the longest timeout as experimental["ferun.callId"]. An
// invalid id, an id that names another call and a bash timeout over the
// cap are JSON-RPC errors -32602 with data.reason. A cancel reaches a call
// with an id by REST DELETE /api/v1/sandboxes/{id}/calls/{key}, and by
// notifications/cancelled for session (initialize) clients only, also
// shortly after its request ended (cancel.go). The go-sdk's default
// protocol (server/discover) has no session: its clients cancel with the
// DELETE.
package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// Resolver resolves a bearer to its sandbox: model.ErrUnknownToken for a
// token that names none, any other error for a failure to look it up.
type Resolver func(ctx context.Context, bearer string) (auth.Sandbox, error)

// Config is the door's configuration.
type Config struct {
	// AllowedOrigins are the Origins a request may come from
	// (--mcp-allowed-origins); with none, a request that has an Origin is
	// refused.
	AllowedOrigins []string
	// SessionKey keys the session ids: the cluster_secrets row
	// mcp-session. Empty: it is read from the database when first needed.
	SessionKey []byte
	// Version is the server's version in its implementation info.
	Version string
	Log     *slog.Logger
	// KeepaliveInterval is the time between SSE keepalive comments while a
	// tool call runs; 25 s when zero.
	KeepaliveInterval time.Duration
	// ProgressInterval is the time between progress notifications of a
	// tool call whose client asked for them; 10 s when zero.
	ProgressInterval time.Duration
}

// The door's defaults and fixed texts.
const (
	defaultKeepalive = 25 * time.Second
	defaultProgress  = 10 * time.Second
	// serverName is the implementation's name in the initialize answer.
	serverName = "ferun"
	// instructions are the server's instructions to the agent.
	instructions = "Tools run in an isolated, persistent sandboxed workspace. " +
		"Files survive across sessions; the workspace is not shared with other sandboxes."
	// touchEvery throttles the writes of a token's last use.
	touchEvery = time.Minute
	// maxTouched bounds the remembered touches before the old ones go.
	maxTouched = 4096
	// metaCallID is the _meta key of a call's id (ADR 0023), and the name
	// of the experimental capability that advertises it.
	metaCallID = "ferun.callId"
	// metaCallState and metaCallReplayed are the _meta keys of a keyed
	// result: its state (done, interrupted, not_run, running), and whether
	// it is another request's call.
	metaCallState    = "ferun.callState"
	metaCallReplayed = "ferun.callReplayed"
)

// Server is the http.Handler of /mcp, with the door's own class of
// answers.
type Server struct {
	cfg     Config
	log     *slog.Logger
	rl      *rateLog
	runner  *tools.Runner
	resolve Resolver
	d       *db.DB
	st      *store.Store
	origins map[string]bool
	handler *mcp.StreamableHTTPHandler
	tools   []*mcp.Tool

	keyMu sync.Mutex
	key   []byte

	mu     sync.Mutex
	calls  map[string]map[uint64]context.CancelFunc // tool calls by "<session>:<request id>"
	seq    uint64
	keyed  *memory[keyedCancel] // calls with an id by "<session>:<request id>", per client, kept cancelLinger after
	recent *memory[struct{}]    // cancels that found no call, per client, for recentFor
	now    func() time.Time     // the clock of keyed and recent

	touchMu sync.Mutex
	touched map[string]time.Time
}

// New makes the door. d may be nil: then there are no session ids unless
// cfg.SessionKey has a key, no cancellation across instances and no token
// touches.
func New(cfg Config, r *tools.Runner, resolve Resolver, d *db.DB) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = defaultKeepalive
	}
	if cfg.ProgressInterval <= 0 {
		cfg.ProgressInterval = defaultProgress
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	s := &Server{
		cfg: cfg, log: cfg.Log, rl: newRateLog(cfg.Log), runner: r, resolve: resolve, d: d,
		origins: map[string]bool{},
		calls:   map[string]map[uint64]context.CancelFunc{},
		keyed:   newMemory[keyedCancel](cancelLinger, maxPerClient),
		recent:  newMemory[struct{}](recentFor, maxPerClient),
		now:     time.Now,
		touched: map[string]time.Time{},
	}
	if d != nil {
		s.st = store.New(d)
	}
	if len(cfg.SessionKey) > 0 {
		s.key = append([]byte(nil), cfg.SessionKey...)
	}
	for _, o := range cfg.AllowedOrigins {
		if o = strings.TrimSpace(o); o != "" {
			s.origins[o] = true
		}
	}
	for _, spec := range r.Specs() {
		s.tools = append(s.tools, &mcp.Tool{Name: spec.Name, Description: spec.Description, InputSchema: spec.InputSchema})
	}
	s.handler = mcp.NewStreamableHTTPHandler(s.factory, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		PropagateRequestCancellation: true,
		// ferund sits behind a local proxy on a single machine, where the
		// SDK would refuse every request on a loopback address with an
		// external Host; the door checks Origin itself and requires a
		// bearer on every request.
		DisableLocalhostProtection: true,
	})
	return s
}

// requestState is what the door learnt of a request, for the factory and
// the tool handlers: the sandbox, the request's own context, the key under
// which its tool call can be cancelled and the client it is counted for
// (clientOf), the keepalive of its response, and the server built for it.
type requestState struct {
	sandbox   auth.Sandbox
	reqCtx    context.Context
	callKey   string
	client    string
	keepalive *keepaliveWriter
	server    *mcp.Server
}

type stateKey struct{}

func stateFrom(ctx context.Context) *requestState {
	st, _ := ctx.Value(stateKey{}).(*requestState)
	return st
}

// ServeHTTP answers /mcp: the door's checks in order, then the SDK.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !s.origins[origin] {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet {
		http.Error(w, "no server-initiated stream", http.StatusMethodNotAllowed)
		return
	}
	authz := r.Header.Get("Authorization")
	bearer, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sandbox"`)
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	sb, err := s.resolve(r.Context(), bearer)
	switch {
	case errors.Is(err, model.ErrUnknownToken):
		s.unknownToken(w, r)
		return
	case err != nil:
		if r.Context().Err() != nil {
			return
		}
		s.rl.warn("lookup", "mcp: token lookup failed", "err", err)
		// The SDK's clients take a 503 as transient: the session goes on.
		w.Header().Set("Retry-After", "1")
		http.Error(w, "token lookup failed", http.StatusServiceUnavailable)
		return
	}
	s.touch(r.Context(), sb.TokenHash)

	msgs, err := peekBody(r)
	if err != nil {
		// The SDK answers a body it cannot read itself.
		msgs = nil
	}
	session := ""
	if sid := r.Header.Get(sessionIDHeader); sid != "" && s.verifySession(r.Context(), sid, sb.TokenHash) {
		session = sid
	}
	st := &requestState{sandbox: sb, reqCtx: r.Context()}
	kw := newKeepaliveWriter(w, s.cfg.KeepaliveInterval)
	st.keepalive = kw
	if len(msgs) == 1 {
		m := msgs[0]
		switch {
		case m.Method == methodInitialize && m.hasID():
			if sid, err := s.newSession(r.Context(), sb.TokenHash); err == nil {
				kw.onOK = func(h http.Header) { h.Set(sessionIDHeader, sid) }
			} else {
				s.rl.warn("session", "mcp: no session id for the initialize answer", "err", err)
			}
		case m.Method == methodToolsCall && m.hasID() && session != "":
			st.callKey, st.client = callKey(session, m.ID), clientOf(sb.TokenHash)
		case m.Method == methodCancelled && session != "":
			s.broadcastCancel(r.Context(), clientOf(sb.TokenHash), session, m.Params)
		}
	}
	ctx := auth.WithSandbox(r.Context(), sb)
	ctx = context.WithValue(ctx, stateKey{}, st)
	s.handler.ServeHTTP(kw, r.WithContext(ctx))
	kw.stop()
}

// unknownToken answers a bearer that names no sandbox: 401 for the
// requests that open a session (initialize, server/discover) and for a body
// that does not parse; for any other request the JSON-RPC error of a
// session the server does not know, with status 404, which the SDK's
// clients turn into an error carrying its text without failing their
// connection.
func (s *Server) unknownToken(w http.ResponseWriter, r *http.Request) {
	msgs, err := peekBody(r)
	if err != nil || len(msgs) == 0 {
		http.Error(w, "unknown sandbox token", http.StatusUnauthorized)
		return
	}
	for _, m := range msgs {
		if m.Method == methodInitialize || m.Method == methodDiscover {
			http.Error(w, "unknown sandbox token", http.StatusUnauthorized)
			return
		}
	}
	id := "null"
	for _, m := range msgs {
		if m.hasID() {
			id = compactJSON(m.ID)
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32001,"message":"session not found: unknown sandbox token"}}`, id)
}

// touch records the token's use, at most once a minute per token on this
// instance (the database throttles across instances).
func (s *Server) touch(ctx context.Context, hash string) {
	if s.st == nil || hash == "" {
		return
	}
	now := time.Now()
	s.touchMu.Lock()
	if last, ok := s.touched[hash]; ok && now.Sub(last) < touchEvery {
		s.touchMu.Unlock()
		return
	}
	if len(s.touched) >= maxTouched {
		for h, t := range s.touched {
			if now.Sub(t) >= touchEvery {
				delete(s.touched, h)
			}
		}
	}
	s.touched[hash] = now
	s.touchMu.Unlock()
	if err := s.st.TouchSandboxToken(ctx, hash); err != nil && ctx.Err() == nil {
		s.log.Debug("mcp: touch a sandbox token", "err", err)
	}
}

// factory is the SDK's server for a request: one per request, for the
// sandbox the door resolved. It never returns nil.
func (s *Server) factory(r *http.Request) *mcp.Server {
	st := stateFrom(r.Context())
	if st == nil {
		// Not through ServeHTTP: a server whose tools have no sandbox.
		st = &requestState{}
	}
	if st.server == nil {
		st.server = s.newServer(st.sandbox)
	}
	return st.server
}

// newServer is the baseline as an MCP server bound to one sandbox: tools
// only, each with its schema as it is. Its capabilities are the SDK's
// (logging, and tools inferred from them) and experimental["ferun.callId"]:
// the tools whose calls with an id are recorded, how long their records
// are kept, and the longest timeout such a call may name.
func (s *Server) newServer(sb auth.Sandbox) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: s.cfg.Version},
		&mcp.ServerOptions{
			Instructions: instructions,
			Capabilities: &mcp.ServerCapabilities{
				Logging: &mcp.LoggingCapabilities{},
				// Tools stays nil: the SDK infers {"listChanged":true}
				// from the tools added below, as it did without options.
				Experimental: map[string]any{metaCallID: map[string]any{
					"tools":             tools.KeyedTools(),
					"retentionSeconds":  int(s.runner.CallRetention() / time.Second),
					"maxTimeoutSeconds": int(tools.MaxKeyedTimeout / time.Second),
				}},
			},
		})
	for _, t := range s.tools {
		srv.AddTool(t, s.toolHandler(sb, t.Name))
	}
	return srv
}

// toolHandler runs one tool of the baseline in the sandbox and renders its
// result: the output (cut with the marker), "(no output)" for none,
// "\n(exit code N)" for a non-zero exit, a picture as image content, and
// isError per tool. A call that failed is isError with the output so far,
// a newline, and "error: <err>".
//
// A call with an id (_meta "ferun.callId") runs through CallKeyed: the
// request waits for the call, which outlives it, and the result carries
// the call's id, state and replay in _meta. A request of a session whose
// cancel came before it starts nothing, and one whose client has as many
// requests with an id running on the instance as it may (maxPerClient)
// is an isError result without _meta: its call was not looked up.
func (s *Server) toolHandler(sb auth.Sandbox, tool string) mcp.ToolHandler {
	keyedTool := slices.Contains(tools.KeyedTools(), tool)
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		key, err := callIDOf(req.Params)
		if err != nil {
			return nil, invalidParams("invalid _meta "+metaCallID+": "+tools.ErrBadCallKey.Error(),
				map[string]any{"reason": "invalid_call_id"})
		}
		st := stateFrom(ctx)
		if st != nil && st.keepalive != nil {
			st.keepalive.start()
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		if st != nil && st.reqCtx != nil {
			// A stateless call's answer has no way to its client but its
			// own request: when the request ends, so does the call without
			// an id, in every protocol version (the SDK cancels only the
			// newer ones), and the wait for a call with an id.
			stop := context.AfterFunc(st.reqCtx, cancel)
			defer stop()
		}
		if st != nil && st.callKey != "" {
			// Registered before the check of the early cancels, both
			// under s.mu: a cancel either finds the request here or is
			// remembered before the check (cancelKey).
			defer s.register(st.callKey, cancel)()
			if key != "" && keyedTool {
				done, err := s.registerKeyed(st.client, st.callKey, sb.ID, key)
				if err != nil {
					return render(tools.Result{}, err), nil
				}
				defer done()
			}
			if s.recentlyCancelled(st.callKey) {
				return nil, context.Canceled
			}
		}
		var progress func(string)
		if req.Params != nil && req.Session != nil {
			if token := req.Params.GetProgressToken(); token != nil {
				hb := s.newHeartbeats(ctx, req.Session, token)
				defer hb.stop()
				progress = func(string) { hb.beat() }
			}
		}
		var args json.RawMessage
		if req.Params != nil {
			args = req.Params.Arguments
		}
		res, info, err := s.runner.CallKeyed(ctx, sb, tool, args, key, progress)
		var conflict *tools.CallIDConflictError
		if errors.As(err, &conflict) {
			return nil, invalidParams(err.Error(), map[string]any{"reason": "call_id_conflict", "tool": conflict.Tool})
		}
		var tooLong *tools.KeyedTimeoutError
		if errors.As(err, &tooLong) {
			return nil, invalidParams(err.Error(),
				map[string]any{"reason": "timeout_too_long", "maxTimeoutSeconds": int(tools.MaxKeyedTimeout / time.Second)})
		}
		if ctx.Err() != nil {
			// The client went away or cancelled the call; a call without
			// an id was cancelled with it, a call with an id runs on.
			return nil, ctx.Err()
		}
		out := render(res, err)
		if info.Keyed {
			// A request released while another instance's call still
			// runs (*tools.AskAgainError) is an isError result, state
			// running.
			out.Meta = mcp.Meta{metaCallID: key, metaCallState: info.State, metaCallReplayed: info.Replay}
		}
		return out, nil
	}
}

// callIDOf is the call id in a tools/call's _meta: "" when there is none,
// tools.ErrBadCallKey for a value that is not a string or not a call id.
func callIDOf(p *mcp.CallToolParamsRaw) (string, error) {
	if p == nil || p.Meta == nil {
		return "", nil
	}
	v, ok := p.Meta[metaCallID]
	if !ok {
		return "", nil
	}
	key, ok := v.(string)
	if !ok || !tools.ValidCallKey(key) {
		return "", tools.ErrBadCallKey
	}
	return key, nil
}

// invalidParams is a JSON-RPC error -32602 (invalid params) with msg and
// data, whose "reason" a client acts on.
func invalidParams(msg string, data map[string]any) *jsonrpc.Error {
	e := &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: msg}
	if b, err := json.Marshal(data); err == nil {
		e.Data = b
	}
	return e
}

// render is the MCP result of a tool call: tools.RenderText as its text
// content, and the picture of a call that did not fail as image content.
func render(res tools.Result, err error) *mcp.CallToolResult {
	text, isError := tools.RenderText(res, err)
	content := []mcp.Content{&mcp.TextContent{Text: text}}
	if img := res.Image; img != nil && err == nil {
		content = append(content, &mcp.ImageContent{Data: img.Data, MIMEType: img.MediaType})
	}
	return &mcp.CallToolResult{Content: content, IsError: isError}
}

// heartbeats sends a call's progress notifications: on a timer, and on
// every chunk of output. The counter is the progress; there is no total.
type heartbeats struct {
	ctx     context.Context
	session *mcp.ServerSession
	token   any
	log     *slog.Logger

	mu sync.Mutex
	n  int

	done chan struct{}
	wg   sync.WaitGroup
}

// newHeartbeats starts the timer. The notifications go with the call's
// context, which routes them onto its response stream.
func (s *Server) newHeartbeats(ctx context.Context, ss *mcp.ServerSession, token any) *heartbeats {
	h := &heartbeats{ctx: ctx, session: ss, token: token, log: s.log, done: make(chan struct{})}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		t := time.NewTicker(s.cfg.ProgressInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				h.beat()
			case <-h.done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return h
}

func (h *heartbeats) beat() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n++
	if err := h.session.NotifyProgress(h.ctx, &mcp.ProgressNotificationParams{
		ProgressToken: h.token,
		Progress:      float64(h.n),
	}); err != nil && h.ctx.Err() == nil {
		h.log.Debug("mcp: progress notification", "err", err)
	}
}

// stop ends the timer and waits for it, so that nothing is sent after the
// call's answer.
func (h *heartbeats) stop() {
	close(h.done)
	h.wg.Wait()
}
