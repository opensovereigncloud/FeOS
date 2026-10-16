// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

// Sandbox access over FeRun's MCP endpoint. This backend is an ordinary
// MCP client of ferund; it has no privileged way to run anything:
//
//   - Provisioning (once per conversation): POST /api/v1/sandboxes with
//     the conversation UUID as the sandbox id; a fresh sandbox returns
//     its first token in that response, an existing one gets a new token
//     from POST /api/v1/sandboxes/{id}/tokens.
//   - Execution: tools/call against /mcp with the sandbox token as
//     bearer, exactly the surface any other agent gets.
//   - The LLM's tool schemas come from GET /api/v1/tools; there are no
//     copies of them here.
//
// MCP sessions are cached per conversation; on 404 (ferund restarted, the
// session is gone) the client initializes again, once, with the same
// bearer, the path the MCP spec gives every client. The session is never
// the identity; the sandbox is, through its token. An empty conversation
// id gets a generated UUID (an anonymous throwaway sandbox, cleaned up by
// hand).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxToolOutput caps one tool result in the conversation (~50 KiB).
const maxToolOutput = 50_000

// toolRunWait is how long a tool may run once its sandbox is up, what
// the chat used to allow a whole call. A call can first wait for its
// sandbox to be created (a cold image pull) or resumed; ferund announces
// that bound itself (preparationWaitMs on GET /api/v1/tools: its
// preparation budget plus its reply budget), and the call timeout is the
// two added up. toolRunWait is also the margin that lets ferund's own
// answer at the end of a failed preparation reach us before we give up:
// a client that cancels sooner cancels the preparation, and its retry
// starts the pull from the beginning.
const toolRunWait = 4 * time.Minute

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// bearerRT sets the Authorization header (tenant token or sandbox
// token). Deliberately without keep-alives: chat and sandbox traffic are
// often minutes apart, and a half-open connection that k3s conntrack
// dropped silently then eats the full client timeout (Go does not retry
// such POSTs). Fresh connections are cheap and safe here.
type bearerRT struct{ token string }

var bearerTransport = &http.Transport{DisableKeepAlives: true}

func (rt bearerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+rt.token)
	return bearerTransport.RoundTrip(r)
}

// sandboxClient is the provisioning client (tenant bearer) plus the
// cache of MCP sessions per conversation.
type sandboxClient struct {
	base      string // e.g. http://ferund.ferun.svc.cluster.local:8100
	ctrlToken string
	http      *http.Client
	mu        sync.Mutex
	sessions  map[string]*convSession
	// tools is the first tool list that arrived (loadedTools); nil until then.
	toolsMu sync.Mutex
	tools   *toolset
}

// toolset is one answer of GET /api/v1/tools: the definitions the LLM is
// offered, their names, and ferund's announced preparation wait.
type toolset struct {
	defs     []Tool
	known    map[string]bool
	prepWait time.Duration
}

// has: was the tool in the tool list? A nil toolset (no ferund
// configured) knows no tools.
func (ts *toolset) has(name string) bool { return ts != nil && ts.known[name] }

type convSession struct {
	token string
	conn  *mcp.ClientSession
	image string // the image passed at creation, immutable afterwards
}

func newSandboxClient(base, ctrlToken string) (*sandboxClient, error) {
	return &sandboxClient{
		base:      strings.TrimRight(base, "/"),
		ctrlToken: ctrlToken,
		http: &http.Client{
			Transport: bearerRT{ctrlToken},
			Timeout:   30 * time.Second,
		},
		sessions: map[string]*convSession{},
	}, nil
}

// provisioningAPI calls one route of ferund's tenant-scoped REST API.
// body may be empty for a GET.
func (s *sandboxClient) provisioningAPI(ctx context.Context, method, path, body string, out any) error {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, rdr)
	if err != nil {
		return err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode,
			string(raw[:min(len(raw), 400)]))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// sessionFor returns a conversation's MCP session, lazily: it
// provisions sandbox + token and connects /mcp. The conversation UUID is
// the sandbox id. image only counts when the sandbox is created; it is
// immutable afterwards, and a later, different non-empty value is logged
// and ignored.
func (s *sandboxClient) sessionFor(ctx context.Context, convID, image string) (*convSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cs, ok := s.sessions[convID]; ok {
		if image != "" && cs.image != "" && image != cs.image {
			log.Printf("conversation %s: image mismatch %q != %q, keeping %q",
				convID, image, cs.image, cs.image)
		}
		return cs, nil
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := s.provisioningAPI(ctx, http.MethodPost, "/api/v1/sandboxes",
		fmt.Sprintf(`{"sandboxId":%q,"image":%q}`, convID, image), &created); err != nil {
		return nil, err
	}
	token := created.Token
	if token == "" {
		var minted struct {
			Token string `json:"token"`
		}
		if err := s.provisioningAPI(ctx, http.MethodPost,
			"/api/v1/sandboxes/"+url.PathEscape(convID)+"/tokens",
			`{"label":"fechat-backend"}`, &minted); err != nil {
			return nil, err
		}
		token = minted.Token
	}
	cs := &convSession{token: token, image: image}
	if err := s.connectSession(ctx, cs); err != nil {
		return nil, err
	}
	s.sessions[convID] = cs
	log.Printf("conversation %s: mcp session established (image=%q)", convID, image)
	return cs, nil
}

func (s *sandboxClient) connectSession(ctx context.Context, cs *convSession) error {
	if cs.conn != nil {
		cs.conn.Close()
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "fechat-backend", Version: "v1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: s.base + "/mcp",
		// no standalone SSE GET: ferund answers it with 405
		DisableStandaloneSSE: true,
		HTTPClient:           &http.Client{Transport: bearerRT{cs.token}},
	}, nil)
	if err != nil {
		return err
	}
	cs.conn = conn
	return nil
}

func isSessionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "404") || strings.Contains(msg, "session not found")
}

// executeSandboxTool runs one tool over MCP in the conversation's sandbox
// and returns the collected output as a string.
func (s *sandboxClient) executeSandboxTool(ts *toolset, convID, image, tool, argsJSON string) string {
	if convID == "" {
		convID = newUUID()
		log.Printf("no conversation_id -> anonymous sandbox %s", convID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ts.callTimeout())
	defer cancel()

	cs, err := s.sessionFor(ctx, convID, image)
	if err != nil {
		// to the LLM and to the log: otherwise the error is seen nowhere
		log.Printf("conversation %s: provisioning failed: %v", convID, err)
		return "error: sandbox provisioning: " + err.Error()
	}
	var args map[string]any
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "error: invalid tool arguments: " + err.Error()
		}
	}
	res, err := cs.conn.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil && isSessionError(err) {
		// the spec's path after a lost session: initialize again, once
		s.mu.Lock()
		cerr := s.connectSession(ctx, cs)
		s.mu.Unlock()
		if cerr == nil {
			res, err = cs.conn.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		}
	}
	if err != nil {
		return "error: mcp: " + err.Error()
	}
	var out strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	result := out.String()
	if len(result) > maxToolOutput {
		result = result[:maxToolOutput] + "\n... [truncated]\n"
	}
	if res.IsError {
		result = "error: " + result
	}
	if strings.TrimSpace(result) == "" {
		result = "(no output)"
	}
	return result
}

// callTimeout bounds one tool call: ferund's preparation wait plus the
// tool's own run time (toolRunWait).
func (ts *toolset) callTimeout() time.Duration { return ts.prepWait + toolRunWait }

// loadedTools returns the tool list, fetching it on the first chat
// message rather than at startup: a backend that fetched it once at boot,
// while ferund was down, would run without tools for its whole lifetime.
// A failed fetch is not kept: that message fails and the next one asks
// again.
func (s *sandboxClient) loadedTools(ctx context.Context) (*toolset, error) {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	if s.tools == nil {
		ts, err := s.sandboxTools(ctx)
		if err != nil {
			return nil, err
		}
		s.tools = ts
		log.Printf("sandbox tools loaded via %s (%d tools)", s.base, len(ts.defs))
	}
	return s.tools, nil
}

// sandboxTools fetches the tool schemas from GET /api/v1/tools and
// builds the OpenAI tool definitions from them, the one source. The
// schemas are not per sandbox, which is why they come from the tenant's
// REST API and not from an MCP session (no sandbox token needed). The
// same answer carries ferund's preparation wait, which sizes every tool
// call's timeout (callTimeout); a ferund that does not announce it is
// refused rather than guessed at.
func (s *sandboxClient) sandboxTools(ctx context.Context) (*toolset, error) {
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
		PreparationWaitMs int64 `json:"preparationWaitMs"`
	}
	if err := s.provisioningAPI(ctx, http.MethodGet, "/api/v1/tools", "", &res); err != nil {
		return nil, err
	}
	if res.PreparationWaitMs <= 0 {
		return nil, fmt.Errorf("GET /api/v1/tools announces no preparationWaitMs: " +
			"ferund does not say how long bringing a sandbox up may take")
	}
	ts := &toolset{
		known:    map[string]bool{},
		prepWait: time.Duration(res.PreparationWaitMs) * time.Millisecond,
	}
	for _, t := range res.Tools {
		var schema any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			return nil, fmt.Errorf("tools %s: %w", t.Name, err)
		}
		var tool Tool
		tool.Type = "function"
		tool.Function.Name = t.Name
		tool.Function.Description = t.Description
		tool.Function.Parameters = schema
		ts.defs = append(ts.defs, tool)
		ts.known[t.Name] = true
	}
	return ts, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
