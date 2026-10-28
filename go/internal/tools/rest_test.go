// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	toolv1 "github.com/gardenlinux/feos/go/gen/feos/tool/v1"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
	"github.com/gardenlinux/feos/go/internal/tools/toolstest"
)

// restMux mounts the tool routes as the API does, with the sandbox class
// reduced to putting sb into the context.
func restMux(h *REST, sb auth.Sandbox) http.Handler {
	mux := http.NewServeMux()
	p := "/api/v1/sandboxes/{id}"
	mux.HandleFunc("POST "+p+"/exec", h.Exec)
	mux.HandleFunc("GET "+p+"/files/{path...}", h.ReadFile)
	mux.HandleFunc("PUT "+p+"/files/{path...}", h.WriteFile)
	mux.HandleFunc("PATCH "+p+"/files/{path...}", h.EditFile)
	mux.HandleFunc("GET "+p+"/dirs/{path...}", h.Dirs)
	mux.HandleFunc("GET "+p+"/search", h.Search)
	mux.HandleFunc("POST "+p+"/fetch", h.Fetch)
	mux.HandleFunc("GET "+p+"/processes", h.ListProcesses)
	mux.HandleFunc("POST "+p+"/processes", h.StartProcess)
	mux.HandleFunc("GET "+p+"/processes/{pid}/logs", h.ProcessLogs)
	mux.HandleFunc("DELETE "+p+"/processes/{pid}", h.StopProcess)
	mux.HandleFunc("GET "+p+"/calls/{key}", h.Call)
	mux.HandleFunc("DELETE "+p+"/calls/{key}", h.CancelCall)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sb.ID.IsZero() {
			r = r.WithContext(auth.WithSandbox(r.Context(), sb))
		}
		mux.ServeHTTP(w, r)
	})
}

type restCase struct {
	method, path, body string
	status             int
	want               string // the body, exactly
	retryAfter         string
}

func doREST(t *testing.T, h http.Handler, c restCase) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != c.status || (c.want != "" && rec.Body.String() != c.want) || rec.Header().Get("Retry-After") != c.retryAfter {
		body := c.body
		if len(body) > 100 {
			body = body[:100] + "…"
		}
		t.Errorf("%s %s %s: %d %q Retry-After %q; want %d %q %q", c.method, c.path, body,
			rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"), c.status, c.want, c.retryAfter)
	}
	if rec.Code != 0 && rec.Body.Len() > 0 && rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("%s %s: content type %q", c.method, c.path, rec.Header().Get("Content-Type"))
	}
	return rec
}

// The routes' requests are checked with the texts the clients know; a
// tool's own failure is a 200 with its exit code.
func TestRESTRoutes(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	h := restMux(NewREST(r), sb)
	base := "/api/v1/sandboxes/" + sb.ID.String()

	// Requests the routes refuse before anything runs.
	for _, c := range []restCase{
		{"POST", base + "/exec", `{}`, 400, `{"error":"command is required"}` + "\n", ""},
		{"POST", base + "/exec", ``, 400, `{"error":"command is required"}` + "\n", ""},
		{"POST", base + "/exec", `[1]`, 400, `{"error":"request body must be a JSON object: json: cannot unmarshal array into Go value of type map[string]json.RawMessage"}` + "\n", ""},
		{"POST", base + "/exec", `{"command":5}`, 400, `{"error":"invalid request body: json: cannot unmarshal number into Go struct field BashParams.command of type string"}` + "\n", ""},
		{"GET", base + "/files/", ``, 400, `{"error":"path is required"}` + "\n", ""},
		{"GET", base + "/files/a?offset=x", ``, 400, `{"error":"offset must be an integer, got \"x\""}` + "\n", ""},
		{"GET", base + "/files/a?limit=1.5", ``, 400, `{"error":"limit must be an integer, got \"1.5\""}` + "\n", ""},
		{"PUT", base + "/files/", `x`, 400, `{"error":"path is required"}` + "\n", ""},
		{"PATCH", base + "/files/a", `{}`, 400, `{"error":"edits is required and must not be empty"}` + "\n", ""},
		{"PATCH", base + "/files/a", `{"edits":[{"old":"x","new":"y"},{"new":"z"}]}`, 400, `{"error":"edits[1].old is required"}` + "\n", ""},
		{"PATCH", base + "/files/", `{"edits":[{"old":"x","new":"y"}]}`, 400, `{"error":"path is required"}` + "\n", ""},
		{"GET", base + "/dirs/?limit=a", ``, 400, `{"error":"limit must be an integer, got \"a\""}` + "\n", ""},
		{"GET", base + "/search", ``, 400, `{"error":"q is required"}` + "\n", ""},
		{"GET", base + "/search?q=x&kind=both", ``, 400, `{"error":"kind must be content or name, got \"both\""}` + "\n", ""},
		{"GET", base + "/search?q=x&literal=maybe", ``, 400, `{"error":"literal must be a boolean, got \"maybe\""}` + "\n", ""},
		{"GET", base + "/search?q=x&kind=name&glob=2", ``, 400, `{"error":"glob must be a boolean, got \"2\""}` + "\n", ""},
		{"GET", base + "/search?q=x&context=-", ``, 400, `{"error":"context must be an integer, got \"-\""}` + "\n", ""},
		{"POST", base + "/fetch", `{}`, 400, `{"error":"url is required"}` + "\n", ""},
		{"POST", base + "/processes", `{"timeout":0}`, 400, `{"error":"command is required"}` + "\n", ""},
		{"POST", base + "/processes", `{"command":"npm run dev"}`, 400, `{"error":"timeout is required (seconds the process may run; 0 = no limit)"}` + "\n", ""},
		{"GET", base + "/processes/p1/logs?lines=many", ``, 400, `{"error":"lines must be an integer, got \"many\""}` + "\n", ""},
		// A negative count is the client's mistake: a 400, not a failed call.
		{"GET", base + "/files/a?offset=-1", ``, 400, `{"error":"offset must not be negative, got -1"}` + "\n", ""},
		{"GET", base + "/files/a?limit=-1", ``, 400, `{"error":"limit must not be negative, got -1"}` + "\n", ""},
		{"GET", base + "/dirs/?limit=-1", ``, 400, `{"error":"limit must not be negative, got -1"}` + "\n", ""},
		{"GET", base + "/search?q=x&context=-2", ``, 400, `{"error":"context must not be negative, got -2"}` + "\n", ""},
		{"GET", base + "/search?q=x&kind=name&limit=-3", ``, 400, `{"error":"limit must not be negative, got -3"}` + "\n", ""},
		// A body past the cap is refused, not cut; a file write's cap is the
		// guest's.
		{"PUT", base + "/files/big", strings.Repeat("x", maxGuestRequest+1), 413,
			`{"error":"write: the content is larger than 4032 KiB, the most one write carries into the sandbox"}` + "\n", ""},
		{"POST", base + "/exec", `{"command":"` + strings.Repeat("x", maxRequestBody) + `"}`, 413, `{"error":"request body larger than 32 MiB"}` + "\n", ""},
	} {
		doREST(t, h, c)
	}
	if n := env.Engine.Calls("Acquire", sb.ID); n != 0 {
		t.Errorf("a refused request acquired the sandbox %d times", n)
	}

	// Tools that ran: 200, whatever their exit code.
	var seen []proto.Message
	env.Node.Script().SetTool(func(_ context.Context, _ podid.ID, method string, req proto.Message) (proto.Message, bool, error) {
		seen = append(seen, req)
		switch method {
		case toolv1.ToolService_Rg_FullMethodName:
			return &toolv1.RgResponse{}, true, nil
		case toolv1.ToolService_Fd_FullMethodName:
			return &toolv1.FdResponse{Entries: []string{"/w/a.go"}}, true, nil
		case toolv1.ToolService_Webfetch_FullMethodName:
			return &toolv1.WebfetchResponse{Title: "T", Status: 200, EffectiveUrl: "https://e.x/", Text: "body"}, true, nil
		}
		return nil, false, nil
	})
	for _, c := range []restCase{
		{"POST", base + "/exec", `{"command":"echo hi; exit 2"}`, 200, `{"exitCode":2,"output":"hi\n"}` + "\n", ""},
		{"PUT", base + "/files/workspace/a.txt", "line1\nline2\n", 200, `{"exitCode":0,"output":"wrote /workspace/a.txt\n"}` + "\n", ""},
		{"GET", base + "/files/workspace/a.txt?offset=2", ``, 200, `{"exitCode":0,"output":"line2\n"}` + "\n", ""},
		{"GET", base + "/files/workspace/nope", ``, 200, ``, ""},
		{"PATCH", base + "/files/workspace/a.txt", `{"edits":[{"old":"line1","new":"first"}]}`, 200, `{"exitCode":0,"output":"edited /workspace/a.txt (1 replacements)\n"}` + "\n", ""},
		{"GET", base + "/dirs/workspace", ``, 200, `{"exitCode":0,"output":"a.txt\n"}` + "\n", ""},
		{"GET", base + "/dirs/", ``, 200, `{"exitCode":0,"output":"workspace/\n"}` + "\n", ""},
		{"GET", base + "/search?q=zzz&path=/w&glob=*.go&ignoreCase&literal=true&context=1&limit=5", ``, 200, `{"exitCode":1,"output":""}` + "\n", ""},
		{"GET", base + "/search?q=go&kind=name&glob&path=/w&limit=7", ``, 200, `{"exitCode":0,"output":"/w/a.go\n"}` + "\n", ""},
		{"POST", base + "/fetch", `{"url":"https://e.x/"}`, 200, `{"exitCode":0,"output":"T\n\n[200 https://e.x/]\nbody"}` + "\n", ""},
	} {
		doREST(t, h, c)
	}
	want := []proto.Message{
		&toolv1.WriteRequest{Container: "main", Path: "/workspace/a.txt", Content: []byte("line1\nline2\n")},
		&toolv1.ReadRequest{Container: "main", Path: "/workspace/a.txt", Offset: 2},
		&toolv1.ReadRequest{Container: "main", Path: "/workspace/nope"},
		&toolv1.EditRequest{Container: "main", Path: "/workspace/a.txt", Edits: []*toolv1.EditOp{{Old: "line1", New: "first"}}},
		&toolv1.LsRequest{Container: "main", Path: "/workspace"},
		&toolv1.LsRequest{Container: "main", Path: "/"},
		&toolv1.RgRequest{Container: "main", Pattern: "zzz", Path: "/w", Glob: "*.go", IgnoreCase: true, Literal: true, Context: 1, Limit: 5},
		&toolv1.FdRequest{Container: "main", Pattern: "go", Path: "/w", Glob: true, Limit: 7},
		&toolv1.WebfetchRequest{Url: "https://e.x/"},
	}
	if len(seen) != len(want) {
		t.Fatalf("%d tool requests", len(seen))
	}
	for i := range want {
		if !proto.Equal(seen[i], want[i]) {
			t.Errorf("request %d: %v, want %v", i, seen[i], want[i])
		}
	}

	// The process routes carry the handle's id and the lines.
	var argv [][]string
	scriptExec(env, func(req *containerv1.ExecRequest, out testnode.ExecOutput) int32 {
		argv = append(argv, req.GetCommand())
		if req.GetCommand()[2] == processStartScript {
			_ = out.Stdout([]byte(`{"id": "120001-abcdef", "pgid": 42, "log": "/l", "timeout": 0, "name": ""}` + "\n"))
		}
		return 0
	})
	doREST(t, h, restCase{"POST", base + "/processes", `{"command":"npm run dev","timeout":0}`, 200,
		`{"exitCode":0,"output":"{\"id\": \"120001-abcdef\", \"pgid\": 42, \"log\": \"/l\", \"timeout\": 0, \"name\": \"\"}\n"}` + "\n", ""})
	doREST(t, h, restCase{"GET", base + "/processes", ``, 200, `{"exitCode":0,"output":""}` + "\n", ""})
	doREST(t, h, restCase{"GET", base + "/processes/120001-abcdef/logs?lines=5", ``, 200, "", ""})
	doREST(t, h, restCase{"DELETE", base + "/processes/120001-abcdef", ``, 200, "", ""})
	if len(argv) != 4 || argv[0][3] != "npm run dev" || argv[0][4] != "0" || argv[2][3] != "120001-abcdef" || argv[2][4] != "5" ||
		argv[3][2] != processStopScript || argv[3][3] != "120001-abcdef" {
		t.Errorf("argv %q", argv)
	}
	if got := env.Engine.Released(sb.ID); len(got) != 1 || got[0] != "120001-abcdef" {
		t.Errorf("released %v", got)
	}
}

// The answer's shape: the output cut at MaxToolOutput without a marker and
// truncated set; a picture as base64.
func TestRESTResultShape(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	sb := env.NewSandbox()
	h := restMux(NewREST(newRunner(env)), sb)
	base := "/api/v1/sandboxes/" + sb.ID.String()

	scriptExec(env, func(_ *containerv1.ExecRequest, out testnode.ExecOutput) int32 {
		_ = out.Stdout([]byte(strings.Repeat("a", 30000)))
		_ = out.Stderr([]byte(strings.Repeat("b", 30000)))
		return 0
	})
	rec := doREST(t, h, restCase{"POST", base + "/exec", `{"command":"x"}`, 200, "", ""})
	var res struct {
		ExitCode  int     `json:"exitCode"`
		Output    string  `json:"output"`
		Truncated *bool   `json:"truncated"`
		Image     *string `json:"image"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Output) != MaxToolOutput || res.Truncated == nil || !*res.Truncated || strings.Contains(res.Output, "truncated") {
		t.Errorf("output %d bytes, truncated %v", len(res.Output), res.Truncated)
	}

	png := []byte("\x89PNG fake")
	scriptTool(env, toolv1.ToolService_Read_FullMethodName, &toolv1.ReadResponse{Image: png, MediaType: "image/png", Width: 3, Height: 4})
	rec = doREST(t, h, restCase{"GET", base + "/files/s.png", ``, 200, "", ""})
	want := fmt.Sprintf(`{"exitCode":0,"output":"/s.png: image/png, 3x4 px, 1 KiB\n","image":{"mediaType":"image/png","data":"%s","width":3,"height":4}}`+"\n",
		base64.StdEncoding.EncodeToString(png))
	if rec.Body.String() != want {
		t.Errorf("image answer\n got %s\nwant %s", rec.Body.String(), want)
	}
}

// A call that failed is a 502 "<tool>: <err>" on every tool route, with
// Retry-After for causes worth a retry (the error's own time, else 5),
// whatever another door answers for the same cause; a client that hung up
// gets nothing.
func TestRESTErrors(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{MaxStreams: 4}) // 2 long streams
	r := newRunner(env)
	sb := env.NewSandbox()
	h := restMux(NewREST(r), sb)
	base := "/api/v1/sandboxes/" + sb.ID.String()

	for _, tc := range []struct {
		err        error
		path, body string
		want       string
		retryAfter string
	}{
		{model.ErrLost, "/exec", `{"command":"x"}`, `{"error":"bash: ensure sandbox: sandbox lost"}` + "\n", ""},
		{fmt.Errorf("%w (retryable): no feasible node", model.ErrNoCapacity), "/files/a", ``,
			`{"error":"read: ensure sandbox: no capacity (retryable): no feasible node"}` + "\n", "5"},
		{fmt.Errorf("%w: node n1 is down", model.ErrNodeUnavailable), "/dirs/", ``,
			`{"error":"ls: ensure sandbox: node unavailable: node n1 is down"}` + "\n", "5"},
		{fmt.Errorf("instance i1 is stopping: %w", model.ErrStopping), "/processes", ``,
			`{"error":"process_list: ensure sandbox: instance i1 is stopping: instance is stopping"}` + "\n", "5"},
		{hibernating{sb.ID}, "/exec", `{"command":"x"}`,
			`{"error":"bash: ensure sandbox: sandbox ` + sb.ID.String() + ` is hibernating; retry in a minute"}` + "\n", "60"},
	} {
		env.Engine.FailAcquire(sb.ID, tc.err)
		method := "GET"
		if tc.body != "" {
			method = "POST"
		}
		doREST(t, h, restCase{method, base + tc.path, tc.body, 502, tc.want, tc.retryAfter})
	}

	// The stream budget: 502, retryable.
	started, release := blockExec(env)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doREST(t, h, restCase{"POST", base + "/exec", `{"command":"wait"}`, 200, "", ""})
		}()
	}
	waitN(t, started, 2)
	doREST(t, h, restCase{"POST", base + "/exec", `{"command":"x"}`, 502, `{"error":"bash: node n1: stream budget exhausted"}` + "\n", "5"})
	close(release)
	wg.Wait()

	// The tool budget: 502, retryable.
	gate := make(chan struct{})
	entered := make(chan struct{}, 10)
	env.Node.Script().SetHook(toolv1.ToolService_Read_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		entered <- struct{}{}
		<-gate
		return nil
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		doREST(t, h, restCase{"GET", base + "/files/a", ``, 200, "", ""})
	}()
	waitN(t, entered, 1)
	doREST(t, h, restCase{"GET", base + "/files/a", ``, 502, `{"error":"read: node n1: tool budget exhausted"}` + "\n", "5"})
	close(gate)
	wg.Wait()
	env.Node.Script().SetHook(toolv1.ToolService_Read_FullMethodName, nil)

	// A timeout: 502 without Retry-After.
	env.Node.Script().SetExec(nil)
	doREST(t, h, restCase{"POST", base + "/exec", `{"command":"sleep 60","timeout":1}`, 502, `{"error":"bash: timeout after 1s"}` + "\n", ""})

	// A client that hung up: nothing written, and the guest's command is
	// cancelled with the request.
	started, _ = blockExec(env)
	ctx, cancel := context.WithCancel(bg)
	req := httptest.NewRequest("POST", base+"/exec", strings.NewReader(`{"command":"wait"}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req)
	}()
	waitN(t, started, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the hung-up request did not end")
	}
	if rec.Body.Len() != 0 || len(rec.Header()) != 0 {
		t.Errorf("a hung-up client got %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}

	// Without the API's sandbox class there is no sandbox.
	doREST(t, restMux(NewREST(r), auth.Sandbox{}), restCase{"POST", base + "/exec", `{"command":"x"}`, 401, `{"error":"sandbox token required"}` + "\n", ""})
	if env.Engine.Holds(sb.ID) != 0 {
		t.Errorf("%d holds left", env.Engine.Holds(sb.ID))
	}
}

// A file write carries its body byte for byte, whatever the bytes: 0x00
// to 0xFF reach the node as sent. The activity log records the write as
// MCP's call of the tool is recorded when the content is UTF-8 text, and
// by its path and size when it is not.
func TestRESTWriteBytes(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	sb := env.NewSandbox()
	h := restMux(NewREST(newRunner(env)), sb)
	base := "/api/v1/sandboxes/" + sb.ID.String()
	var mu sync.Mutex
	var got [][]byte
	env.Node.Script().SetTool(func(_ context.Context, _ podid.ID, _ string, req proto.Message) (proto.Message, bool, error) {
		if w, ok := req.(*toolv1.WriteRequest); ok {
			mu.Lock()
			got = append(got, w.GetContent())
			mu.Unlock()
		}
		return nil, false, nil
	})
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	doREST(t, h, restCase{"PUT", base + "/files/workspace/bytes.bin", string(all), 200,
		`{"exitCode":0,"output":"wrote /workspace/bytes.bin\n"}` + "\n", ""})
	doREST(t, h, restCase{"PUT", base + "/files/workspace/text.txt", "h\u00e9llo\n", 200,
		`{"exitCode":0,"output":"wrote /workspace/text.txt\n"}` + "\n", ""})
	mu.Lock()
	if len(got) != 2 || !bytes.Equal(got[0], all) || string(got[1]) != "h\u00e9llo\n" {
		t.Errorf("the node received %q", got)
	}
	mu.Unlock()

	want := map[string]map[string]any{
		"write: /workspace/bytes.bin": {"path": "/workspace/bytes.bin", "size": 256.0},
		"write: /workspace/text.txt":  {"path": "/workspace/text.txt", "content": "h\u00e9llo\n"},
	}
	for _, row := range env.Activity(sb.ID, 4) {
		if row.Kind != "tool_start" {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(row.Params, &params); err != nil || !maps.Equal(params, want[row.Name]) {
			t.Errorf("%s: params %s", row.Name, row.Params)
		}
		delete(want, row.Name)
	}
	if len(want) != 0 {
		t.Errorf("no tool_start rows for %v", want)
	}
}

// A write, an edit or a command larger than the guest takes is a 413 that
// states the limit and never reaches the node; MCP's call of the tool
// gets the same error. The largest write the limit lets through, the node,
// whose services take what the guest's take, accepts.
func TestRESTGuestLimit(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	sb := env.NewSandbox()
	h := restMux(NewREST(r), sb)
	base := "/api/v1/sandboxes/" + sb.ID.String()
	var mu sync.Mutex
	var sizes []int
	env.Node.Script().SetTool(func(_ context.Context, _ podid.ID, _ string, req proto.Message) (proto.Message, bool, error) {
		mu.Lock()
		sizes = append(sizes, proto.Size(req))
		mu.Unlock()
		return nil, false, nil
	})

	path := "/workspace/big.bin"
	// The content whose request is exactly maxGuestRequest bytes: 1 byte of
	// tag and 4 of length besides the rest of the request.
	n := maxGuestRequest - proto.Size(&toolv1.WriteRequest{Container: mainContainer, Path: path}) - 5
	if s := proto.Size(&toolv1.WriteRequest{Container: mainContainer, Path: path, Content: make([]byte, n)}); s != maxGuestRequest {
		t.Fatalf("a request of %d bytes", s)
	}
	doREST(t, h, restCase{"PUT", base + "/files" + path, strings.Repeat("\xff", n), 200,
		`{"exitCode":0,"output":"wrote /workspace/big.bin\n"}` + "\n", ""})
	mu.Lock()
	if len(sizes) != 1 || sizes[0] != maxGuestRequest {
		t.Errorf("the node received requests of %v bytes", sizes)
	}
	mu.Unlock()
	acquired := env.Engine.Calls("Acquire", sb.ID)

	big := strings.Repeat("x", 5<<20)
	const (
		writeText = `the content is larger than 4032 KiB, the most one write carries into the sandbox`
		editText  = `the edits are larger than 4032 KiB, the most one edit carries into the sandbox`
		cmdText   = `the command is larger than 4032 KiB, the most one call carries into the sandbox`
	)
	for _, c := range []restCase{
		{"PUT", base + "/files" + path, strings.Repeat("\xff", n+1), 413, `{"error":"write: ` + writeText + `"}` + "\n", ""},
		{"PUT", base + "/files" + path, big, 413, `{"error":"write: ` + writeText + `"}` + "\n", ""},
		{"PATCH", base + "/files" + path, `{"edits":[{"old":"a","new":"` + big + `"}]}`, 413,
			`{"error":"edit: ` + editText + `"}` + "\n", ""},
		{"POST", base + "/exec", `{"command":"` + big + `"}`, 413, `{"error":"bash: ` + cmdText + `"}` + "\n", ""},
		{"POST", base + "/processes", `{"command":"` + big + `","timeout":0}`, 413,
			`{"error":"process_start: ` + cmdText + `"}` + "\n", ""},
	} {
		doREST(t, h, c)
	}
	args, _ := json.Marshal(WriteParams{Path: path, Content: big})
	var tooLarge *TooLargeError
	if _, err := r.Call(bg, sb, ToolWrite, args, nil); !errors.As(err, &tooLarge) || err.Error() != writeText {
		t.Errorf("MCP's write: %v", err)
	}
	if n := env.Engine.Calls("Acquire", sb.ID); n != acquired {
		t.Errorf("a refused call acquired the sandbox %d times", n-acquired)
	}
	mu.Lock()
	if len(sizes) != 1 {
		t.Errorf("the node received requests of %v bytes", sizes)
	}
	mu.Unlock()
}

// The routes over a real HTTP server, as FeCode's client reaches them: a
// path with segments it escaped.
func TestRESTOverHTTP(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	sb := env.NewSandbox()
	srv := httptest.NewServer(restMux(NewREST(newRunner(env)), sb))
	defer srv.Close()
	base := srv.URL + "/api/v1/sandboxes/" + sb.ID.String()
	put, _ := http.NewRequest("PUT", base+"/files/my%20dir/a%3Fb.txt", strings.NewReader("content"))
	resp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != `{"exitCode":0,"output":"wrote /my dir/a?b.txt\n"}`+"\n" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, err = http.Get(base + "/dirs/" + url.PathEscape("my dir") + "?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != `{"exitCode":0,"output":"a?b.txt\n"}`+"\n" {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}

// keyedREST sends a request with one Idempotency-Key header per key, on
// ctx, and returns the answer.
func keyedREST(ctx context.Context, h http.Handler, method, path, body string, keys ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	for _, k := range keys {
		req.Header.Add(idempotencyHeader, k)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// keyedAsync sends keyedREST's request in a goroutine; the channel gets
// the answer.
func keyedAsync(ctx context.Context, h http.Handler, method, path, body string, keys ...string) chan *httptest.ResponseRecorder {
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() { ch <- keyedREST(ctx, h, method, path, body, keys...) }()
	return ch
}

func awaitREST(t *testing.T, ch chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(10 * time.Second):
		t.Fatal("no answer within 10 s")
	}
	return nil
}

// notYet checks that ch has no answer for 300 ms: its request waits.
func notYet(t *testing.T, what string, ch chan *httptest.ResponseRecorder) {
	t.Helper()
	select {
	case rec := <-ch:
		t.Fatalf("%s answered %d %q", what, rec.Code, rec.Body.String())
	case <-time.After(300 * time.Millisecond):
	}
}

// keyedWant is a keyed answer: its status, its body exactly ("" for any),
// and its headers (Ferun-Call-State, Ferun-Call-Replayed: true,
// Retry-After).
type keyedWant struct {
	status     int
	body       string
	state      string
	replayed   bool
	retryAfter string
}

func checkKeyed(t *testing.T, what string, rec *httptest.ResponseRecorder, want keyedWant) {
	t.Helper()
	replayed := rec.Header().Get(callReplayedHeader)
	if rec.Code != want.status || (want.body != "" && rec.Body.String() != want.body) ||
		rec.Header().Get(callStateHeader) != want.state || (replayed == "true") != want.replayed ||
		(replayed != "" && replayed != "true") || rec.Header().Get("Retry-After") != want.retryAfter {
		t.Errorf("%s: %d %q state %q replayed %q Retry-After %q; want %+v", what, rec.Code, rec.Body.String(),
			rec.Header().Get(callStateHeader), replayed, rec.Header().Get("Retry-After"), want)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("%s: content type %q", what, rec.Header().Get("Content-Type"))
	}
}

// recordOfREST decodes the record route's answer, and checks that it has
// exactly the fields of its state: the result's only once the call ended.
func recordOfREST(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("the record: %d %q", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	fields := []string{"key", "tool", "state", "instance", "startedAt"}
	if m["state"] != CallRunning {
		fields = append(fields, "finishedAt", "exitCode", "output", "truncated", "isError", "error", "text")
	}
	if len(m) != len(fields) {
		t.Errorf("the record has %d fields: %s", len(m), rec.Body.String())
	}
	for _, f := range fields {
		if _, ok := m[f]; !ok {
			t.Errorf("the record has no %s: %s", f, rec.Body.String())
		}
	}
	for _, f := range []string{"startedAt", "finishedAt"} {
		s, ok := m[f].(string)
		if !ok {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, s); err != nil || !strings.HasSuffix(s, "Z") || ts.IsZero() {
			t.Errorf("the record's %s %q: %v", f, s, err)
		}
	}
	return m
}

// The REST answers of calls with an id (ADR 0023): the Idempotency-Key
// header, the call's state and replay in the answer's headers, the
// statuses of a conflict, a timeout over the cap, a call that did not run
// and one that ran and failed; the record route with its wait, and the
// cancel.
func TestRESTKeyed(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	h := restMux(NewREST(r), sb)
	base := "/api/v1/sandboxes/" + sb.ID.String()
	const okOut = `{"exitCode":0,"output":"out\n"}` + "\n"

	// Twice: the second request attaches to the first's call while it
	// runs, and answers the same, as a replay.
	p := probeExec(env, "out\n")
	first := keyedAsync(bg, h, "POST", base+"/exec", `{"command":"work"}`, "k1")
	waitN(t, p.started, 1)
	second := keyedAsync(bg, h, "POST", base+"/exec", `{ "command": "work", "timeout": 0 }`, "k1")
	notYet(t, "the second request", second)
	close(p.release)
	checkKeyed(t, "the first request", awaitREST(t, first), keyedWant{200, okOut, CallDone, false, ""})
	checkKeyed(t, "the second request", awaitREST(t, second), keyedWant{200, okOut, CallDone, true, ""})
	// The id as a quoted sf-string, and with spaces around it, is the same
	// id: the stored answer.
	for _, k := range []string{`"k1"`, " k1 ", ` "k1"`} {
		checkKeyed(t, "the id "+k, keyedREST(bg, h, "POST", base+"/exec", `{"command":"work"}`, k),
			keyedWant{200, okOut, CallDone, true, ""})
	}
	if n := p.n.Load(); n != 1 {
		t.Errorf("%d Execs for one id", n)
	}

	// Another command under the same id: 422, and nothing runs.
	checkKeyed(t, "another command", keyedREST(bg, h, "POST", base+"/exec", `{"command":"other"}`, "k1"),
		keyedWant{422, `{"error":"bash: call id conflict: the id names another call in this sandbox (bash with other ` +
			`arguments); a new call needs a new id","code":"call_id_conflict"}` + "\n", "", false, ""})
	if n := p.n.Load(); n != 1 {
		t.Errorf("%d Execs after a conflict", n)
	}

	// Ids the header does not take: 400 on every tool route, read-only or
	// not, before anything runs.
	acquired := env.Engine.Calls("Acquire", sb.ID)
	const badKey = `{"error":"Idempotency-Key must be 1 to 255 characters of A-Z a-z 0-9 . _ ~ : @ -",` +
		`"code":"bad_idempotency_key"}` + "\n"
	for _, k := range []string{"a b", "a/b", strings.Repeat("a", MaxCallKey+1), "", `""`, `"k1`, "k\u00e91", `"a"b"`} {
		for _, c := range []struct{ method, path, body string }{
			{"POST", base + "/exec", `{"command":"x"}`},
			{"GET", base + "/files/a", ``},
			{"PUT", base + "/files/a", `content`},
			{"GET", base + "/dirs/", ``},
			{"DELETE", base + "/processes/p1", ``},
		} {
			checkKeyed(t, fmt.Sprintf("%s %s with the id %q", c.method, c.path, k),
				keyedREST(bg, h, c.method, c.path, c.body, k), keyedWant{400, badKey, "", false, ""})
		}
	}
	checkKeyed(t, "two ids", keyedREST(bg, h, "POST", base+"/exec", `{"command":"x"}`, "a", "b"),
		keyedWant{400, `{"error":"one Idempotency-Key, got 2","code":"bad_idempotency_key"}` + "\n", "", false, ""})
	// A bash timeout over the cap: 400, nothing recorded.
	checkKeyed(t, "timeout 601", keyedREST(bg, h, "POST", base+"/exec", `{"command":"x","timeout":601}`, "long"),
		keyedWant{400, `{"error":"bash: a call with an id may run at most 600 s: lower its timeout, or start longer ` +
			`work with process_start","code":"call_timeout_too_long"}` + "\n", "", false, ""})
	if n := env.Engine.Calls("Acquire", sb.ID); n != acquired {
		t.Errorf("refused requests acquired the sandbox %d times", n-acquired)
	}
	noCallRow(t, env, sb.ID, "long")

	// A read-only tool takes a valid id, the longest too, and ignores it.
	for _, k := range []string{"k1", strings.Repeat("a", MaxCallKey)} {
		checkKeyed(t, "ls with an id", keyedREST(bg, h, "GET", base+"/dirs/", ``, k), keyedWant{200, "", "", false, ""})
	}
	noCallRow(t, env, sb.ID, strings.Repeat("a", MaxCallKey))

	// A call that did not run (no capacity): 502 with Retry-After, not_run;
	// sent again once the cause is gone, it runs.
	env.Engine.FailAcquire(sb.ID, fmt.Errorf("%w (retryable): no feasible node", model.ErrNoCapacity))
	checkKeyed(t, "no capacity", keyedREST(bg, h, "POST", base+"/exec", `{"command":"cap"}`, "cap"),
		keyedWant{502, `{"error":"bash: ensure sandbox: no capacity (retryable): no feasible node"}` + "\n", CallNotRun, false, "5"})
	checkKeyed(t, "no capacity, sent again", keyedREST(bg, h, "POST", base+"/exec", `{"command":"cap"}`, "cap"),
		keyedWant{200, okOut, CallDone, false, ""})
	if n := p.n.Load(); n != 2 {
		t.Errorf("%d Execs", n)
	}

	// A call that ran and failed (its timeout): 502 without Retry-After,
	// done; its record carries the error as MCP renders it.
	p = probeExec(env, "")
	checkKeyed(t, "timeout", keyedREST(bg, h, "POST", base+"/exec", `{"command":"sleep","timeout":1}`, "slow"),
		keyedWant{502, `{"error":"bash: timeout after 1s"}` + "\n", CallDone, false, ""})
	m := recordOfREST(t, keyedREST(bg, h, "GET", base+"/calls/slow", ``))
	wantText, _ := RenderText(Result{}, &TimeoutError{Seconds: 1})
	if m["state"] != CallDone || m["error"] != "timeout after 1s" || m["isError"] != true || m["text"] != wantText ||
		m["output"] != "" || m["exitCode"] != float64(callRow(t, env, sb.ID, "slow").ExitCode) || m["key"] != "slow" ||
		m["tool"] != ToolBash {
		t.Errorf("the record of a timeout: %v", m)
	}

	// A client that hangs up: nothing is written, the call runs on, and its
	// record says running until it ended.
	p = probeExec(env, "late\n")
	ctx, cancel := context.WithCancel(bg)
	hungUp := keyedAsync(ctx, h, "POST", base+"/exec", `{"command":"x"}`, "hu")
	waitN(t, p.started, 1)
	cancel()
	if rec := awaitREST(t, hungUp); rec.Body.Len() != 0 || len(rec.Header()) != 0 {
		t.Errorf("a hung-up client got %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	m = recordOfREST(t, keyedREST(bg, h, "GET", base+"/calls/hu", ``))
	if m["state"] != CallRunning || m["key"] != "hu" || m["tool"] != ToolBash || m["instance"] != toolstest.InstanceID {
		t.Errorf("the record while running: %v", m)
	}
	waited := keyedAsync(bg, h, "GET", base+"/calls/hu?wait=5", ``)
	notYet(t, "the record with a wait", waited)
	close(p.release)
	m = recordOfREST(t, awaitREST(t, waited))
	wantText, _ = RenderText(Result{Text: "late\n"}, nil)
	if m["state"] != CallDone || m["output"] != "late\n" || m["exitCode"] != 0.0 || m["text"] != wantText ||
		m["isError"] != false || m["error"] != "" || m["truncated"] != false || m["instance"] != toolstest.InstanceID {
		t.Errorf("the record after the wait: %v", m)
	}
	select {
	case <-p.cancelled:
		t.Error("the hung-up request cancelled its call")
	default:
	}

	// The record route's requests.
	for _, c := range []restCase{
		{"GET", base + "/calls/hu?wait=61", ``, 400, `{"error":"wait must be 0 to 60 seconds, got 61"}` + "\n", ""},
		{"GET", base + "/calls/hu?wait=-1", ``, 400, `{"error":"wait must be 0 to 60 seconds, got -1"}` + "\n", ""},
		{"GET", base + "/calls/hu?wait=x", ``, 400, `{"error":"wait must be an integer, got \"x\""}` + "\n", ""},
		{"GET", base + "/calls/a%20b", ``, 400, badKey, ""},
		{"DELETE", base + "/calls/a%2Fb", ``, 400, badKey, ""},
		{"GET", base + "/calls/unknown", ``, 404, `{"error":"no call with this id in the sandbox","code":"no_such_call"}` + "\n", ""},
		{"GET", base + "/calls/unknown?wait=1", ``, 404, `{"error":"no call with this id in the sandbox","code":"no_such_call"}` + "\n", ""},
		{"DELETE", base + "/calls/unknown", ``, 404, `{"error":"no running call with this id in the sandbox","code":"no_such_call"}` + "\n", ""},
	} {
		doREST(t, h, c)
	}

	// The cancel: 202, and the waiting request gets the cancel, final;
	// then nothing runs under the id.
	p = probeExec(env, "")
	waiting := keyedAsync(bg, h, "POST", base+"/exec", `{"command":"x"}`, "del")
	waitN(t, p.started, 1)
	doREST(t, h, restCase{"DELETE", base + "/calls/del", ``, 202, `{"cancelled":true}` + "\n", ""})
	checkKeyed(t, "the cancelled request", awaitREST(t, waiting),
		keyedWant{502, `{"error":"bash: the call was cancelled by its caller"}` + "\n", CallDone, false, ""})
	waitN(t, p.cancelled, 1)
	doREST(t, h, restCase{"DELETE", base + "/calls/del", ``, 404,
		`{"error":"no running call with this id in the sandbox","code":"no_such_call"}` + "\n", ""})
	checkKeyed(t, "the cancelled call again", keyedREST(bg, h, "POST", base+"/exec", `{"command":"x"}`, "del"),
		keyedWant{502, `{"error":"bash: the call was cancelled by its caller"}` + "\n", CallDone, true, ""})

	// A file write with an id, of bytes that are not UTF-8: sent twice, it
	// is written once, and the second answer is the first's, replayed.
	var mu sync.Mutex
	var writes [][]byte
	env.Node.Script().SetTool(func(_ context.Context, _ podid.ID, _ string, req proto.Message) (proto.Message, bool, error) {
		if w, ok := req.(*toolv1.WriteRequest); ok {
			mu.Lock()
			writes = append(writes, w.GetContent())
			mu.Unlock()
		}
		return nil, false, nil
	})
	content := "\xff\xfe\x00 not UTF-8 \xc3"
	const wrote = `{"exitCode":0,"output":"wrote /workspace/bin\n"}` + "\n"
	checkKeyed(t, "the write", keyedREST(bg, h, "PUT", base+"/files/workspace/bin", content, "w1"),
		keyedWant{200, wrote, CallDone, false, ""})
	checkKeyed(t, "the write again", keyedREST(bg, h, "PUT", base+"/files/workspace/bin", content, "w1"),
		keyedWant{200, wrote, CallDone, true, ""})
	checkKeyed(t, "another write under its id", keyedREST(bg, h, "PUT", base+"/files/workspace/bin", content+"!", "w1"),
		keyedWant{422, `{"error":"write: call id conflict: the id names another call in this sandbox (write with other ` +
			`arguments); a new call needs a new id","code":"call_id_conflict"}` + "\n", "", false, ""})
	mu.Lock()
	if len(writes) != 1 || string(writes[0]) != content {
		t.Errorf("the node received %q", writes)
	}
	mu.Unlock()
}

// A request that waits for another instance's call is answered 503
// call_running with Retry-After 1 once this instance drains; a call that
// was interrupted is a 502 call_interrupted without Retry-After.
func TestRESTKeyedOtherInstance(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	startRun(t, r)
	sb := env.NewSandbox()
	h := restMux(NewREST(r), sb)
	base := "/api/v1/sandboxes/" + sb.ID.String()
	args := `{"command":"x"}`

	row := foreignRow(t, env, sb, "gone", args, true, time.Minute)
	const interrupted = "interrupted: instance i9 stopped before the call ended"
	if ok, err := env.Store.FinishToolCall(bg, row.ID, store.ToolCallOutcome{State: store.ToolCallInterrupted,
		Output: []byte("half"), Error: interrupted}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	checkKeyed(t, "an interrupted call", keyedREST(bg, h, "POST", base+"/exec", args, "gone"),
		keyedWant{502, `{"error":"bash: ` + interrupted + `","code":"call_interrupted"}` + "\n", CallInterrupted, true, ""})
	m := recordOfREST(t, keyedREST(bg, h, "GET", base+"/calls/gone", ``))
	wantText, _ := RenderText(Result{Text: "half"}, errors.New(interrupted))
	if m["state"] != CallInterrupted || m["instance"] != "i9" || m["output"] != "half" || m["text"] != wantText ||
		m["isError"] != true || m["error"] != interrupted {
		t.Errorf("the record of an interrupted call: %v", m)
	}

	foreignRow(t, env, sb, "x", args, true, time.Minute)
	waiting := keyedAsync(bg, h, "POST", base+"/exec", args, "x")
	notYet(t, "the request of another instance's call", waiting)
	r.BeginDrain(time.Now().Add(time.Minute))
	checkKeyed(t, "the released request", awaitREST(t, waiting),
		keyedWant{503, `{"error":"bash: the call is still running on instance i9, and this instance is stopping: ` +
			`ask again with the same id","code":"call_running"}` + "\n", CallRunning, true, "1"})
	// The record says running, at once, while this instance drains.
	start := time.Now()
	m = recordOfREST(t, keyedREST(bg, h, "GET", base+"/calls/x?wait=30", ``))
	if m["state"] != CallRunning || m["instance"] != "i9" || time.Since(start) > 2*time.Second {
		t.Errorf("the record while draining: %v after %s", m, time.Since(start))
	}
	// A new call while draining did not run: 502 with Retry-After.
	checkKeyed(t, "a new call while draining", keyedREST(bg, h, "POST", base+"/exec", args, "new"),
		keyedWant{502, `{"error":"bash: instance ` + toolstest.InstanceID + ` is stopping: instance is stopping"}` + "\n",
			CallNotRun, false, "5"})
}
