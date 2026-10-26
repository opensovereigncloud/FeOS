// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package mcpsrv

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/status"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	"github.com/ironcore-dev/feos/go/internal/auth"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/podid"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/testnode"
	"github.com/ironcore-dev/feos/go/internal/tools"
	"github.com/ironcore-dev/feos/go/internal/tools/toolstest"
)

var bg = context.Background()

// tokens is the resolver of the tests: sandbox tokens in memory, which a
// test mints and revokes, and failures of the lookup it injects.
type tokens struct {
	mu    sync.Mutex
	m     map[string]auth.Sandbox
	fails int
}

func (k *tokens) resolve(_ context.Context, bearer string) (auth.Sandbox, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fails > 0 {
		k.fails--
		return auth.Sandbox{}, errors.New("connection refused")
	}
	sb, ok := k.m[bearer]
	if !ok {
		return auth.Sandbox{}, model.ErrUnknownToken
	}
	return sb, nil
}

func (k *tokens) mint(sb auth.Sandbox) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	k.mu.Lock()
	defer k.mu.Unlock()
	sb.TokenHash = store.HashToken(token)
	k.m[token] = sb
	return token
}

func (k *tokens) revoke(token string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, token)
}

func (k *tokens) fail(n int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.fails = n
}

// methods records the JSON-RPC methods of the requests a door gets.
type methods struct {
	mu   sync.Mutex
	seen []string
}

func (m *methods) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if msgs, err := peekBody(r); err == nil {
			m.mu.Lock()
			for _, msg := range msgs {
				m.seen = append(m.seen, msg.Method)
			}
			m.mu.Unlock()
		}
		h.ServeHTTP(w, r)
	})
}

func (m *methods) has(method string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.seen {
		if s == method {
			return true
		}
	}
	return false
}

// harness is an instance's /mcp over HTTP, with the world of toolstest
// behind it.
type harness struct {
	t       *testing.T
	env     *toolstest.Env
	runner  *tools.Runner
	tokens  *tokens
	srv     *Server
	methods *methods
	http    *httptest.Server
	sb      auth.Sandbox
	token   string
}

func newHarness(t *testing.T, o toolstest.Options, cfg Config) *harness {
	env := toolstest.New(t, o)
	h := &harness{t: t, env: env, tokens: &tokens{m: map[string]auth.Sandbox{}}, methods: &methods{}}
	h.runner = tools.NewRunner(env.Engine, env.Port, env.Recorder, env.DB, toolstest.InstanceID, env.Log)
	h.srv = h.newServer(cfg)
	h.http = httptest.NewServer(h.methods.wrap(h.srv))
	t.Cleanup(h.http.Close)
	h.sb = env.NewSandbox()
	h.token = h.tokens.mint(h.sb)
	return h
}

// newServer makes another door of the same instance's world, running its
// cancellation loop.
func (h *harness) newServer(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = h.env.Log
	}
	cfg.Version = "test"
	s := New(cfg, h.runner, h.tokens.resolve, h.env.DB)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(ctx)
	}()
	h.t.Cleanup(func() {
		cancel()
		<-done
	})
	if err := h.env.DB.WaitListening(bg); err != nil {
		h.t.Fatal(err)
	}
	return s
}

// bearer is FeCode's transport: the sandbox token on every request, a new
// connection for every request.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func transport() http.RoundTripper {
	return &http.Transport{DisableKeepAlives: true}
}

// connect opens a client session the way FeCode does; opts nil is FeCode's
// own call (the server/discover path of the newest protocol).
func (h *harness) connect(url, token string, copts *mcp.ClientOptions, opts *mcp.ClientSessionOptions) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "fecode-backend", Version: "v1"}, copts)
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             url,
		DisableStandaloneSSE: true,
		HTTPClient:           &http.Client{Transport: bearer{token: token, base: transport()}},
	}, opts)
}

func (h *harness) session(opts *mcp.ClientSessionOptions) *mcp.ClientSession {
	h.t.Helper()
	cs, err := h.connect(h.http.URL, h.token, nil, opts)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { cs.Close() })
	return cs
}

// legacy is the protocol version of an initialize-only client.
var legacy = &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"}

// call runs a tool and returns its text, its isError and the call's error.
func call(cs *mcp.ClientSession, tool string, args map[string]any) (string, bool, error) {
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String(), res.IsError, nil
}

// post sends one raw JSON-RPC request to the door and returns the
// response.
func (h *harness) post(url, token, sid, body string, header ...string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sid != "" {
		req.Header.Set(sessionIDHeader, sid)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := (&http.Client{Transport: transport()}).Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`

// initialize opens a legacy session over raw HTTP and returns its id.
func (h *harness) initialize(url, token string) string {
	h.t.Helper()
	resp := h.post(url, token, "", initializeBody)
	sid := resp.Header.Get(sessionIDHeader)
	if b := body(h.t, resp); resp.StatusCode != 200 || !strings.Contains(b, `"serverInfo"`) {
		h.t.Fatalf("initialize: %d %s", resp.StatusCode, b)
	}
	return sid
}

// sseData returns the data lines of an SSE body.
func sseData(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, d)
		}
	}
	return out
}

// blockExec makes the node's Exec wait until its call ends, and tells
// started when one begins and ended when its context ended.
func blockExec(env *toolstest.Env) (started, ended chan struct{}) {
	started, ended = make(chan struct{}, 100), make(chan struct{}, 100)
	env.Node.Script().SetExec(func(ctx context.Context, _ podid.ID, _ *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		_ = out.Stdout([]byte("begun\n"))
		started <- struct{}{}
		<-ctx.Done()
		ended <- struct{}{}
		return 0, true, status.FromContextError(ctx.Err()).Err()
	})
	return started, ended
}

func wait(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}
