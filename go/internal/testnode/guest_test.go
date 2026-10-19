// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	portforwardv1 "github.com/ironcore-dev/feos/go/gen/feos/portforward/v1"
	toolv1 "github.com/ironcore-dev/feos/go/gen/feos/tool/v1"
	watchv1 "github.com/ironcore-dev/feos/go/gen/feos/watch/v1"
)

// guestEnv is a node with a running pod.
type guestEnv struct {
	e   *env
	in  *instance
	n   *Node
	pc  podv1.PodServiceClient
	uid uuid.UUID
	id  string
}

func newGuestEnv(t *testing.T, mod ...func(*Config)) *guestEnv {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in, mod...)
	pc := pods(t, in, "n1")
	uid, _ := createRunning(t, pc)
	return &guestEnv{e: e, in: in, n: n, pc: pc, uid: uid, id: uid.String()}
}

func (g *guestEnv) tools(t *testing.T) toolv1.ToolServiceClient {
	return toolv1.NewToolServiceClient(conn(t, g.in, "n1"))
}

func TestGuestRouting(t *testing.T) {
	g := newGuestEnv(t, func(c *Config) { c.PendingDelay = 200 * time.Millisecond })
	ctx := context.Background()

	r, err := execOut(t, g.in, "n1", g.id, "bash", "-c", "pwd; echo err >&2; exit 3")
	if err != nil || !r.exited || r.code != 3 || r.stdout != g.n.PodDir(g.uid)+"\n" || r.stderr != "err\n" {
		t.Fatalf("exec: %+v, %v", r, err)
	}

	// A call into a pod's guest that feosd does not forward.
	_, err = g.pc.GetPod(podCtx(g.id), &podv1.GetPodRequest{PodId: g.id})
	wantCode(t, err, codes.Unimplemented, "/feos.pod.v1.PodService/GetPod is not served for pods")
	// A guest service without a pod.
	_, err = g.tools(t).Ls(ctx, &toolv1.LsRequest{Path: "/"})
	wantPrefix(t, err, codes.Unimplemented, "/feos.tool.v1.ToolService/Ls is served in pods only")

	unknown := uuid.NewString()
	_, err = execOut(t, g.in, "n1", unknown, "true")
	wantCode(t, err, codes.NotFound, "no pod "+unknown)
	_, err = execOut(t, g.in, "n1", "nope", "true")
	wantCode(t, err, codes.InvalidArgument, `pod ID "nope" is not a UUID`)

	g.n.Script().SetGuestUnreachable(g.uid, true)
	_, err = execOut(t, g.in, "n1", g.id, "true")
	wantPrefix(t, err, codes.Unavailable, "the guest of pod "+g.id+": ")
	g.n.Script().SetGuestUnreachable(g.uid, false)

	if _, err := g.pc.SuspendPod(ctx, &podv1.SuspendPodRequest{PodId: g.id}); err != nil {
		t.Fatal(err)
	}
	_, err = execOut(t, g.in, "n1", g.id, "true")
	wantCode(t, err, codes.FailedPrecondition, "pod "+g.id+" is POD_STATE_SUSPENDED")
	if _, err := g.pc.StopPod(ctx, &podv1.StopPodRequest{PodId: g.id}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.pc.StartPod(ctx, &podv1.StartPodRequest{PodId: g.id}); err != nil {
		t.Fatal(err)
	}
	// PENDING: the guest does not listen yet.
	_, err = execOut(t, g.in, "n1", g.id, "true")
	wantPrefix(t, err, codes.Unavailable, "the guest of pod "+g.id+": ")
	waitState(t, g.pc, g.id, podv1.PodState_POD_STATE_RUNNING)

	r, err = execOut(t, g.in, "n1", g.id, "/no/such/binary")
	if err != nil || r.code != 127 {
		t.Fatalf("a missing binary: %+v, %v", r, err)
	}
	s, err := containerv1.NewContainerServiceClient(conn(t, g.in, "n1")).Exec(podCtx(g.id), &containerv1.ExecRequest{ContainerId: "other", Command: []string{"true"}})
	if err == nil {
		_, err = s.Recv()
	}
	wantCode(t, err, codes.NotFound, "no container other")
}

// Ending an Exec kills the command's process group, as the guest does.
func TestExecCancelKillsTheGroup(t *testing.T) {
	g := newGuestEnv(t)
	ctx, cancel := context.WithCancel(podCtx(g.id))
	s, err := containerv1.NewContainerServiceClient(conn(t, g.in, "n1")).Exec(ctx, &containerv1.ExecRequest{
		Command: []string{"bash", "-c", "sleep 60 & echo $!; wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(f.GetStdout())))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	eventually(t, 5*time.Second, "the child killed", func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	})
}

func TestScriptedExec(t *testing.T) {
	g := newGuestEnv(t)
	g.n.Script().SetExec(func(_ context.Context, pod uuid.UUID, req *containerv1.ExecRequest, out ExecOutput) (int32, bool, error) {
		if req.Command[0] != "scripted" {
			return 0, false, nil
		}
		if err := out.Stdout([]byte("pod " + pod.String())); err != nil {
			return 0, true, err
		}
		return 42, true, out.Stderr(bytes.Repeat([]byte("x"), execChunk+1))
	})
	r, err := execOut(t, g.in, "n1", g.id, "scripted")
	if err != nil || r.code != 42 || r.stdout != "pod "+g.id || len(r.stderr) != execChunk+1 {
		t.Fatalf("scripted: code %d, stdout %q, %d bytes of stderr, %v", r.code, r.stdout, len(r.stderr), err)
	}
	r, err = execOut(t, g.in, "n1", g.id, "echo", "real")
	if err != nil || r.stdout != "real\n" || r.code != 0 {
		t.Fatalf("passed on: %+v, %v", r, err)
	}
}

func TestTools(t *testing.T) {
	g := newGuestEnv(t)
	tc := g.tools(t)
	ctx := podCtx(g.id)

	w, err := tc.Write(ctx, &toolv1.WriteRequest{Path: "/a/b.txt", Content: []byte("one\ntwo\nthree\n")})
	if err != nil || w.Error != "" {
		t.Fatalf("write: %v, %v", w, err)
	}
	if data, err := os.ReadFile(filepath.Join(g.n.PodDir(g.uid), "a", "b.txt")); err != nil || string(data) != "one\ntwo\nthree\n" {
		t.Fatalf("written %q, %v", data, err)
	}
	rd, err := tc.Read(ctx, &toolv1.ReadRequest{Path: "a/b.txt"})
	if err != nil || rd.Text != "one\ntwo\nthree\n" || rd.Truncated {
		t.Fatalf("read: %v, %v", rd, err)
	}
	rd, _ = tc.Read(ctx, &toolv1.ReadRequest{Path: "/a/b.txt", Offset: 2, Limit: 1})
	if rd.Text != "two\n" || !rd.Truncated {
		t.Fatalf("window: %v", rd)
	}
	rd, _ = tc.Read(ctx, &toolv1.ReadRequest{Path: "/a/b.txt", Offset: 9})
	if rd.Text != "" || rd.Error != "" {
		t.Fatalf("past the end: %v", rd)
	}
	rd, _ = tc.Read(ctx, &toolv1.ReadRequest{Path: "/missing"})
	if !strings.HasPrefix(rd.Error, "Path not found: /missing (") {
		t.Fatalf("missing: %v", rd)
	}
	rd, _ = tc.Read(ctx, &toolv1.ReadRequest{Path: "../../etc/passwd"})
	if rd.Error != `path "../../etc/passwd" escapes the container root` {
		t.Fatalf("escape: %v", rd)
	}
	rd, _ = tc.Read(ctx, &toolv1.ReadRequest{Path: "/x", Container: "other"})
	if rd.Error != "container other has no rootfs (not created?)" {
		t.Fatalf("other container: %v", rd)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"), 0, 0, 5, 0, 0, 0, 2, 0xd0, 8, 6, 0, 0, 0)
	if err := os.WriteFile(filepath.Join(g.n.PodDir(g.uid), "shot.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	rd, _ = tc.Read(ctx, &toolv1.ReadRequest{Path: "/shot.png"})
	if rd.MediaType != "image/png" || rd.Width != 1280 || rd.Height != 720 || !bytes.Equal(rd.Image, png) {
		t.Fatalf("image: %s %dx%d", rd.MediaType, rd.Width, rd.Height)
	}

	ed, _ := tc.Edit(ctx, &toolv1.EditRequest{Path: "/a/b.txt", Edits: []*toolv1.EditOp{{Old: "two", New: "TWO"}}})
	if ed.Error != "" || ed.Replacements != 1 {
		t.Fatalf("edit: %v", ed)
	}
	ed, _ = tc.Edit(ctx, &toolv1.EditRequest{Path: "/a/b.txt", Edits: []*toolv1.EditOp{{Old: "e", New: "E"}}})
	if ed.Error != "edit 0: oldText not unique (occurrences: 3)" {
		t.Fatalf("ambiguous edit: %v", ed)
	}
	ed, _ = tc.Edit(ctx, &toolv1.EditRequest{Path: "/a/b.txt"})
	if ed.Error != "edits must not be empty" {
		t.Fatalf("empty edit: %v", ed)
	}

	ls, _ := tc.Ls(ctx, &toolv1.LsRequest{Path: "/"})
	if !slices.Equal(ls.Entries, []string{"a/", "shot.png"}) || ls.Truncated {
		t.Fatalf("ls: %v", ls)
	}
	ls, _ = tc.Ls(ctx, &toolv1.LsRequest{Path: "/", Limit: 1})
	if !slices.Equal(ls.Entries, []string{"a/"}) || !ls.Truncated {
		t.Fatalf("ls limit: %v", ls)
	}
	ls, _ = tc.Ls(ctx, &toolv1.LsRequest{Path: "/a/b.txt"})
	if ls.Error != "Not a directory: /a/b.txt" {
		t.Fatalf("ls file: %v", ls)
	}

	rg, _ := tc.Rg(ctx, &toolv1.RgRequest{Pattern: "TWO", Path: "/"})
	if rg.Text != "a/b.txt:2:TWO\n" || rg.Matches != 1 || rg.Truncated {
		t.Fatalf("rg: %v", rg)
	}
	rg, _ = tc.Rg(ctx, &toolv1.RgRequest{Pattern: "two", Path: "/a", IgnoreCase: true, Context: 1})
	if rg.Text != "/a/b.txt-1-one\n/a/b.txt:2:TWO\n/a/b.txt-3-three\n" {
		t.Fatalf("rg -C: %q", rg.Text)
	}
	rg, _ = tc.Rg(ctx, &toolv1.RgRequest{Pattern: "e", Path: "/", Limit: 1})
	if rg.Text != "a/b.txt:1:one\n" || !rg.Truncated {
		t.Fatalf("rg limit: %v", rg)
	}
	rg, _ = tc.Rg(ctx, &toolv1.RgRequest{Pattern: "(", Path: "/"})
	if !strings.HasPrefix(rg.Error, "rg: ") {
		t.Fatalf("rg bad pattern: %v", rg)
	}
	rg, _ = tc.Rg(ctx, &toolv1.RgRequest{Pattern: "TWO", Path: "/", Glob: "*.md"})
	if rg.Text != "" || rg.Matches != 0 {
		t.Fatalf("rg glob: %v", rg)
	}

	fd, _ := tc.Fd(ctx, &toolv1.FdRequest{Pattern: "B", Path: "/"})
	if len(fd.Entries) != 0 {
		t.Fatalf("fd smart case: %v", fd)
	}
	fd, _ = tc.Fd(ctx, &toolv1.FdRequest{Pattern: "b", Path: "/"})
	if !slices.Equal(fd.Entries, []string{"b.txt"}) {
		t.Fatalf("fd: %v", fd)
	}
	fd, _ = tc.Fd(ctx, &toolv1.FdRequest{Pattern: "*.txt", Path: "/a", Glob: true})
	if !slices.Equal(fd.Entries, []string{"/a/b.txt"}) {
		t.Fatalf("fd --glob: %v", fd)
	}

	g.n.Script().SetTool(func(_ context.Context, _ uuid.UUID, method string, _ proto.Message) (proto.Message, bool, error) {
		if method != toolv1.ToolService_Ls_FullMethodName {
			return nil, false, nil
		}
		return &toolv1.LsResponse{Entries: []string{"scripted"}}, true, nil
	})
	ls, _ = tc.Ls(ctx, &toolv1.LsRequest{Path: "/"})
	if !slices.Equal(ls.Entries, []string{"scripted"}) {
		t.Fatalf("scripted ls: %v", ls)
	}
	rd, _ = tc.Read(ctx, &toolv1.ReadRequest{Path: "/a/b.txt"})
	if rd.Text != "one\nTWO\nthree\n" {
		t.Fatalf("passed on: %v", rd)
	}
}

func TestWebfetch(t *testing.T) {
	g := newGuestEnv(t)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><head><title>A &amp; B</title><style>p{}</style></head><body><p>Hello</p><p>World &lt;3</p><script>x()</script></body></html>")
		case "/text":
			fmt.Fprint(w, "plain")
		default:
			http.NotFound(w, r)
		}
	}))
	defer web.Close()
	tc := g.tools(t)
	ctx := podCtx(g.id)
	wf, err := tc.Webfetch(ctx, &toolv1.WebfetchRequest{Url: web.URL + "/page"})
	if err != nil || wf.Status != 200 || wf.Title != "A & B" || wf.Text != "Hello\nWorld <3" || wf.EffectiveUrl != web.URL+"/page" {
		t.Fatalf("webfetch: %v, %v", wf, err)
	}
	wf, _ = tc.Webfetch(ctx, &toolv1.WebfetchRequest{Url: web.URL + "/text"})
	if wf.Text != "plain" {
		t.Fatalf("text: %v", wf)
	}
	wf, _ = tc.Webfetch(ctx, &toolv1.WebfetchRequest{Url: web.URL + "/missing"})
	if wf.Status != 404 || wf.Error != "webfetch: HTTP 404" {
		t.Fatalf("404: %v", wf)
	}
	_, err = tc.Webfetch(ctx, &toolv1.WebfetchRequest{Url: "ftp://example.test/x"})
	wantCode(t, err, codes.InvalidArgument, `invalid url "ftp://example.test/x" (http/https only)`)
}

func TestWatch(t *testing.T) {
	g := newGuestEnv(t)
	if err := os.Mkdir(filepath.Join(g.n.PodDir(g.uid), "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	wc := watchv1.NewWatchServiceClient(conn(t, g.in, "n1"))
	ctx, cancel := context.WithCancel(podCtx(g.id))
	defer cancel()

	bad, err := wc.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.Send(&watchv1.WatchRequest{Frame: &watchv1.WatchRequest_Subscribe{Subscribe: &watchv1.Subscribe{}}}); err != nil {
		t.Fatal(err)
	}
	_, err = bad.Recv()
	wantCode(t, err, codes.InvalidArgument, "first frame must be `open`")

	s, err := wc.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(&watchv1.WatchRequest{Frame: &watchv1.WatchRequest_Open{Open: &watchv1.Open{}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(&watchv1.WatchRequest{Frame: &watchv1.WatchRequest_Subscribe{Subscribe: &watchv1.Subscribe{Paths: []string{"/src", "/nope"}}}}); err != nil {
		t.Fatal(err)
	}
	resp, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	ack := resp.GetAck()
	if ack == nil || !slices.Equal(ack.Watching, []string{"/src"}) || len(ack.Failed) != 1 || ack.Failed[0].Path != "/nope" || ack.WatchLimit != watchLimit || ack.Cursor != 0 {
		t.Fatalf("ack %v", resp)
	}
	eventually(t, 2*time.Second, "the stream registered", func() bool { return g.n.Script().Watches(g.uid) == 1 })
	if got := g.n.Script().WatchEvents(g.uid, &watchv1.Event{Kind: watchv1.EventKind_EVENT_KIND_CREATED, Dir: "/src", Name: "main.go"}); got != 1 {
		t.Fatalf("delivered to %d streams", got)
	}
	resp, err = s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if evs := resp.GetEvents().GetEvents(); len(evs) != 1 || evs[0].Id != 1 || evs[0].Name != "main.go" {
		t.Fatalf("events %v", resp)
	}
	g.n.Script().WatchStale(g.uid, "/src")
	resp, err = s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if evs := resp.GetEvents().GetEvents(); len(evs) != 1 || evs[0].Id != 2 || evs[0].Kind != watchv1.EventKind_EVENT_KIND_STALE || !slices.Equal(evs[0].StalePaths, []string{"/src"}) {
		t.Fatalf("stale %v", resp)
	}
	if err := s.Send(&watchv1.WatchRequest{Frame: &watchv1.WatchRequest_Subscribe{Subscribe: &watchv1.Subscribe{Paths: []string{"/"}}}}); err != nil {
		t.Fatal(err)
	}
	if resp, err = s.Recv(); err != nil || resp.GetAck().GetCursor() != 2 {
		t.Fatalf("second ack %v, %v", resp, err)
	}
	cancel()
	eventually(t, 2*time.Second, "the stream gone", func() bool { return g.n.Script().Watches(g.uid) == 0 })
}

func TestPortForward(t *testing.T) {
	g := newGuestEnv(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	g.n.Script().SetPortAddr(3000, echo.Addr().String())
	fc := portforwardv1.NewPortForwardServiceClient(conn(t, g.in, "n1"))
	open := func(port uint32) (portforwardv1.PortForwardService_ForwardClient, error) {
		s, err := fc.Forward(podCtx(g.id))
		if err != nil {
			return nil, err
		}
		if err := s.Send(&portforwardv1.ForwardRequest{Frame: &portforwardv1.ForwardRequest_Open{Open: &portforwardv1.Open{Port: port}}}); err != nil {
			return nil, err
		}
		resp, err := s.Recv()
		if err != nil {
			return nil, err
		}
		if resp.GetReady() == nil {
			return nil, fmt.Errorf("first frame %v", resp)
		}
		return s, nil
	}
	s, err := open(3000)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(&portforwardv1.ForwardRequest{Frame: &portforwardv1.ForwardRequest_Data{Data: []byte("ping")}}); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for len(got) < 4 {
		resp, err := s.Recv()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, resp.GetData()...)
	}
	if string(got) != "ping" {
		t.Fatalf("echo %q", got)
	}
	// A half-close: the guest's side closes, the stream ends.
	if err := s.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); err != io.EOF {
		t.Fatalf("after the half-close: %v", err)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g.n.Script().SetPortAddr(3001, closed.Addr().String())
	closed.Close()
	_, err = open(3001)
	wantPrefix(t, err, codes.Unavailable, "127.0.0.1:3001: ")
	_, err = open(0)
	wantCode(t, err, codes.InvalidArgument, "port 0 is not a port")
	_, err = open(70000)
	wantCode(t, err, codes.InvalidArgument, "port 70000 out of range")
	g.n.Script().SetKubePorts(g.uid, 6443)
	_, err = open(6443)
	wantCode(t, err, codes.FailedPrecondition, "port 6443 is a kube listener")
}

func TestToRelAndGlobs(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", "a/./b": "a/b", "/a/../b/": "b", "a//b": "a/b"} {
		if got, err := toRel(in); err != nil || got != want {
			t.Errorf("toRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := toRel("a/../../b"); err == nil {
		t.Error("an escape passed")
	}
	cases := []struct {
		glob, name string
		match      bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "src/main.go", false},
		{"**/*.go", "src/main.go", true},
		{"**/*.go", "main.go", true},
		{"src/**", "src/a/b", true},
		{"*.{go,md}", "x.md", true},
		{"[!a]*", "abc", false},
		{"[!a]*", "bcd", true},
		{"a?c", "abc", true},
		{`a\*`, "a*", true},
	}
	for _, c := range cases {
		re, err := globRegexp(c.glob, false)
		if err != nil {
			t.Fatalf("%s: %v", c.glob, err)
		}
		if re.MatchString(c.name) != c.match {
			t.Errorf("glob %s on %s: %v", c.glob, c.name, !c.match)
		}
	}
	for _, bad := range []string{"[ab", "{a,b", "{a,{b}}", `a\`} {
		if _, err := globRegexp(bad, false); err == nil {
			t.Errorf("glob %q passed", bad)
		}
	}
}

// A command sees the container's environment and the request's, not the
// test node's own, which may hold its join token.
func TestExecEnvironment(t *testing.T) {
	t.Setenv("FERUN_TESTNODE_JOIN_TOKEN", "feos-join-secret")
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in)
	pc := pods(t, in, "n1")
	uid := uuid.New()
	spec := sandboxSpec(uid, "1")
	spec.Containers[0].Env = map[string]string{"FROM_SPEC": "spec", "SHARED": "spec"}
	if _, err := create(t, pc, uid.String(), spec); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc, uid.String(), podv1.PodState_POD_STATE_RUNNING)
	s, err := containerv1.NewContainerServiceClient(conn(t, in, "n1")).Exec(podCtx(uid.String()), &containerv1.ExecRequest{
		ContainerId: "main",
		Command:     []string{"bash", "-c", "env"},
		Env:         map[string]string{"FROM_REQUEST": "request", "SHARED": "request"},
		WorkingDir:  "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for {
		f, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out.Write(f.GetStdout())
	}
	env := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	if _, ok := env["FERUN_TESTNODE_JOIN_TOKEN"]; ok {
		t.Fatal("the command sees the test node's environment")
	}
	if env["FROM_SPEC"] != "spec" || env["FROM_REQUEST"] != "request" || env["SHARED"] != "request" ||
		env["HOME"] != n.PodDir(uid) || env["PWD"] != n.PodDir(uid) || env["PATH"] == "" {
		t.Fatalf("environment %v", env)
	}
}

// A container log's tail is bounded as the guest's: at most 10000 lines
// and 4 MiB from the log's end, each line with its newline; a line that
// starts further back keeps its last whole 64 KiB parts within the bytes,
// or goes when not one fits.
func TestTailOf(t *testing.T) {
	lines := make([]string, 12000)
	for i := range lines {
		lines[i] = fmt.Sprintf("%05d", i)
	}
	if got := tailOf(lines, 20000); len(got) != maxTailLines || got[0] != "02000" || got[len(got)-1] != "11999" {
		t.Errorf("a tail of more lines than the cap: %d lines, %q … %q", len(got), got[0], got[len(got)-1])
	}
	if got := tailOf(lines, 2); !slices.Equal(got, []string{"11998", "11999"}) {
		t.Errorf("a tail of 2: %q", got)
	}

	// Lines of 100 KiB: the last 40 lie within 4 MiB with their newlines;
	// the 98264 bytes left hold one whole 64 KiB part of the one before.
	big := make([]string, 50)
	for i := range big {
		big[i] = fmt.Sprintf("%02d", i) + strings.Repeat("x", 100<<10-2)
	}
	got := tailOf(big, 100)
	if len(got) != 41 || got[0] != big[9][len(big[9])-maxLinePart:] || got[1] != big[10] || got[40] != big[49] {
		t.Fatalf("a tail of long lines: %d lines", len(got))
	}
	total := 0
	for _, l := range got {
		total += len(l) + 1
	}
	if total > maxTailBytes {
		t.Errorf("the tail holds %d bytes", total)
	}
	if got := tailOf(big, 3); len(got) != 3 || got[0] != big[47] {
		t.Errorf("a tail of 3 long lines: %d lines", len(got))
	}
	// A line longer than the bytes keeps its last whole parts within them;
	// one whose part within them is shorter than a part goes.
	huge := strings.Repeat("y", 5<<20)
	if got := tailOf([]string{"a", huge}, 10); len(got) != 1 || len(got[0]) != 63*maxLinePart {
		t.Errorf("a tail of a 5 MiB line: %d lines", len(got))
	}
	fill := strings.Repeat("z", maxTailBytes-100)
	if got := tailOf([]string{huge, fill}, 10); len(got) != 1 || got[0] != fill {
		t.Errorf("a line with less than a part within the bytes: %d lines", len(got))
	}
}
