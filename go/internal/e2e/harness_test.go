// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/server"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// The world every test runs in.
const (
	trustDomain = "feos.test"
	dnsDomain   = "example.test"
	// tenantToken is the bootstrap token: the tenant "operator", as
	// FeCode's FECODE_SANDBOX_API_TOKEN.
	tenantToken = "e2e-tenant-token-0123456789abcdef"
	tenant      = "operator"
	adminUser   = "admin"
	adminPass   = "pw"
	// testImage is pinned by digest, so that no registry is asked.
	testImage = "example.test/img@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// poolSize bounds every instance's database pool: a test runs up to
	// three instances, and the tests run in parallel against one server.
	poolSize = 10
	// maxClusters bounds how many clusters of this package run at once,
	// whatever -parallel says: a cluster holds up to about 44 connections
	// to PostgreSQL (the test's pool, and a pool and a listener per
	// instance), and the server is shared with the other packages' tests.
	maxClusters = 4
	// processRoot is where the process tools keep their records in a
	// container; the tests point it at a directory of their own, since
	// the test node runs commands on the host.
	processRoot = "/var/lib/sandbox-processes"
)

var bg = context.Background()

// clusterSlots holds a token for every cluster that runs (maxClusters).
var clusterSlots = make(chan struct{}, maxClusters)

var (
	tenantAuth = "Bearer " + tenantToken
	adminAuth  = "Basic " + base64.StdEncoding.EncodeToString([]byte(adminUser+":"+adminPass))
)

// cluster is one control plane: a database, a CA, the instances that run
// on them and the test nodes joined to them. Everything stops, and the
// database is dropped, when the test ends.
type cluster struct {
	t       *testing.T
	log     *slog.Logger
	url     string
	caDir   string
	issuer  *pki.Issuer
	db      *db.DB
	st      *store.Store
	objects *testnode.ObjectStore
	// cfg is the instances' configuration; start sets each instance's id
	// and addresses.
	cfg config.Config

	mu        sync.Mutex
	instances []*instance
	nodes     []*node
	next      int
	// ends run after the instances and the nodes have stopped.
	ends []func()
}

// instance is one ferund instance of a cluster, running.
type instance struct {
	*server.Instance
	name   string
	cancel context.CancelFunc
	done   chan error

	stopOnce sync.Once
	stopErr  error
	stopped  chan struct{} // closed when the stop begins
	finished chan struct{} // closed when Run has returned
}

// node is a test node of a cluster, running.
type node struct {
	*testnode.Node
	name   string
	cancel context.CancelFunc
	done   chan struct{}
	err    error // Run's result, once done is closed
}

// newCluster makes the database and the CA of a cluster; tune changes the
// instances' configuration. No instance runs yet.
func newCluster(t *testing.T, tune func(*config.Config)) *cluster {
	t.Helper()
	// The slot is taken first and so given back last, after the
	// cluster has stopped and its database is dropped.
	clusterSlots <- struct{}{}
	t.Cleanup(func() { <-clusterSlots })
	u := withParams(t, dbtest.NewDatabase(t), "pool_max_conns", strconv.Itoa(poolSize))
	base := t.TempDir()
	caDir := filepath.Join(base, "ca")
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: caDir, TrustDomain: trustDomain,
		DNSDomains: []string{dnsDomain}}); err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(caDir, trustDomain)
	if err != nil {
		t.Fatal(err)
	}
	log := dbtest.Logger(t)
	d, err := db.Open(bg, u, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	c := &cluster{t: t, log: log, url: u, caDir: caDir, issuer: iss, db: d, st: store.New(d),
		objects: testnode.NewObjectStore("e2e-objects-" + uuid.NewString()[:8])}
	c.cfg = config.Default()
	c.cfg.DatabaseURL, c.cfg.CADir, c.cfg.TrustDomain = u, caDir, trustDomain
	c.cfg.Listen = "127.0.0.1:0"
	c.cfg.BootstrapToken = tenantToken
	c.cfg.AdminUsers = map[string]string{adminUser: adminPass}
	c.cfg.DefaultImage = testImage
	c.cfg.IdlePause = 0
	c.cfg.ReadinessDelay = 0
	c.cfg.ShutdownGrace = 30 * time.Second
	c.cfg.Version = "e2e"
	if tune != nil {
		tune(&c.cfg)
	}
	t.Cleanup(c.shutdown)
	return c
}

// withParams sets connection parameters, given as name and value pairs,
// on the database URL u, in either of its forms (URL or key=value); they
// take precedence over what u says.
func withParams(t *testing.T, u string, kv ...string) string {
	t.Helper()
	if !strings.Contains(u, "://") {
		for i := 0; i+1 < len(kv); i += 2 {
			u += " " + kv[i] + "=" + kv[i+1]
		}
		return u
	}
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := p.Query()
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	p.RawQuery = q.Encode()
	return p.String()
}

// shutdown stops the instances, all at once, then the nodes: the
// instances' last ops still reach the nodes. Then the ends run.
func (c *cluster) shutdown() {
	c.mu.Lock()
	instances := append([]*instance(nil), c.instances...)
	nodes := append([]*node(nil), c.nodes...)
	ends := c.ends
	c.mu.Unlock()
	defer func() {
		for _, f := range ends {
			f()
		}
	}()
	var wg sync.WaitGroup
	for _, in := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = in.stop()
		}()
	}
	wg.Wait()
	for _, n := range nodes {
		n.stop()
	}
}

// start builds an instance with the cluster's configuration (tune changes
// it for this instance), on free ports, runs it and waits until it
// serves.
func (c *cluster) start(name string, tune ...func(*config.Config)) *instance {
	c.t.Helper()
	cfg := c.cfg
	cfg.InstanceID = "e2e-" + name
	for _, f := range tune {
		f(&cfg)
	}
	var si *server.Instance
	for attempt := 0; ; attempt++ {
		addr := "127.0.0.1:" + freePort(c.t)
		cfg.NodeListen, cfg.NodeAdvertiseAddress = addr, addr
		var err error
		si, err = server.New(bg, cfg, c.log)
		if err == nil {
			break
		}
		// Another listener may have taken the free port in between.
		if errors.Is(err, syscall.EADDRINUSE) && attempt < 5 {
			continue
		}
		c.t.Fatalf("instance %s: %v", name, err)
	}
	ctx, cancel := context.WithCancel(bg)
	in := &instance{Instance: si, name: name, cancel: cancel, done: make(chan error, 1), stopped: make(chan struct{}),
		finished: make(chan struct{})}
	go func() { in.done <- si.Run(ctx) }()
	c.mu.Lock()
	c.instances = append(c.instances, in)
	c.mu.Unlock()
	eventually(c.t, 30*time.Second, "instance "+name+" serves", func() bool {
		r, err := plainClient.Get(in.url("/healthz"))
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == http.StatusOK
	})
	return in
}

// freePort returns a TCP port that was free a moment ago.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// url is the URL of path on the instance's front doors.
func (in *instance) url(path string) string { return "http://" + in.HTTPAddr() + path }

// stop stops the instance as SIGTERM does (§7) and returns Run's result.
func (in *instance) stop() error { return <-in.stopAsync() }

// stopAsync begins the stop and returns at once; the channel receives
// Run's result. From its call on, the instance is not live.
func (in *instance) stopAsync() <-chan error {
	in.stopOnce.Do(func() {
		close(in.stopped)
		in.cancel()
		go func() {
			in.stopErr = <-in.done
			close(in.finished)
		}()
	})
	ch := make(chan error, 1)
	go func() {
		<-in.finished
		ch <- in.stopErr
	}()
	return ch
}

// isStopped reports whether the instance's stop has begun.
func (in *instance) isStopped() bool {
	select {
	case <-in.stopped:
		return true
	default:
		return false
	}
}

// live returns the instances that are not stopping.
func (c *cluster) live() []*instance {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*instance
	for _, in := range c.instances {
		if !in.isStopped() {
			out = append(out, in)
		}
	}
	return out
}

// pick returns the next live instance, round robin: a balancer without
// stickiness, which FeCode's new connection per request meets anyway.
func (c *cluster) pick() *instance {
	live := c.live()
	if len(live) == 0 {
		c.t.Error("no instance runs")
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	return live[c.next%len(live)]
}

// leader returns the live instance that leads, waiting for one.
func (c *cluster) leader() *instance {
	c.t.Helper()
	var l *instance
	eventually(c.t, 30*time.Second, "a leader", func() bool {
		for _, in := range c.live() {
			if in.Elector.IsLeader() {
				l = in
				return true
			}
		}
		return false
	})
	return l
}

// joinToken issues a join token pinned to name ("" for none).
func (c *cluster) joinToken(name string) string {
	c.t.Helper()
	live := c.live()
	if len(live) == 0 {
		c.t.Fatal("no instance runs")
	}
	_, token, _, err := live[0].Fleet.IssueJoinToken(bg, "e2e", name, time.Hour)
	if err != nil {
		c.t.Fatal(err)
	}
	return token
}

// nodeConfig is the configuration of a test node named name that joins
// with token through the live instances: the shared object store, a quick
// bring-up.
func (c *cluster) nodeConfig(name, token string) testnode.Config {
	var addrs []string
	for _, in := range c.live() {
		addrs = append(addrs, in.NodeAddr())
	}
	return testnode.Config{
		Name: name, JoinToken: token, ServerName: c.issuer.ControlPlaneDomain(), TrustDomain: trustDomain,
		Addrs: addrs, ObjectStore: c.objects, PendingDelay: 50 * time.Millisecond, Log: c.log.With("node", name),
	}
}

// join joins a test node named name with a token pinned to it (tune
// changes its configuration) and waits until it is ready and connected to
// every live instance.
func (c *cluster) join(name string, tune ...func(*testnode.Config)) *node {
	c.t.Helper()
	cfg := c.nodeConfig(name, c.joinToken(name))
	for _, f := range tune {
		f(&cfg)
	}
	n := c.runNode(cfg)
	c.waitNode(n)
	return n
}

// runNode runs a test node until the test ends; it does not wait for its
// join.
func (c *cluster) runNode(cfg testnode.Config) *node {
	c.t.Helper()
	tn, err := testnode.New(cfg)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	n := &node{Node: tn, name: cfg.Name, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(n.done)
		n.err = tn.Run(ctx)
	}()
	c.mu.Lock()
	c.nodes = append(c.nodes, n)
	c.mu.Unlock()
	return n
}

// waitNode waits until the node is ready and every live instance has a
// current connection to it.
func (c *cluster) waitNode(n *node) {
	c.t.Helper()
	eventually(c.t, 60*time.Second, "node "+n.name+" ready and connected", func() bool {
		select {
		case <-n.done:
			c.t.Fatalf("node %s ended: %v", n.name, n.err)
		default:
		}
		row, err := c.st.GetNode(bg, n.name)
		if err != nil || row.Status != model.NodeReady {
			return false
		}
		for _, in := range c.live() {
			if !in.Links.Connected(n.name) {
				return false
			}
		}
		return true
	})
}

// stop ends the node's Run and waits for it.
func (n *node) stop() {
	n.cancel()
	<-n.done
}

// ended waits up to within for the node's Run to end and returns its
// result.
func (n *node) ended(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case <-n.done:
		return n.err
	case <-time.After(within):
		t.Fatalf("node %s still runs", n.name)
		return nil
	}
}

// pod returns the node's record of the pod; false when it has none.
func (n *node) pod(id podid.ID) (*podv1.PodInfo, bool) {
	for _, p := range n.Pods() {
		if p.GetPodId() == id.String() {
			return p, true
		}
	}
	return nil, false
}

// ------------------------------------------------------------------ HTTP --

// plainClient makes every request on a new connection, as FeCode's
// clients do.
var plainClient = &http.Client{Transport: newTransport(), Timeout: 3 * time.Minute}

func newTransport() *http.Transport { return &http.Transport{DisableKeepAlives: true} }

// reply is an HTTP answer.
type reply struct {
	code   int
	body   string
	header http.Header
}

// call sends a request to in, or through the balancer when in is nil,
// with auth as its Authorization header ("" for none).
func (c *cluster) call(in *instance, method, path, auth, body string) reply {
	c.t.Helper()
	r, err := c.callErr(bg, in, method, path, auth, body)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return r
}

// callErr is call that returns the transport's error.
func (c *cluster) callErr(ctx context.Context, in *instance, method, path, auth, body string) (reply, error) {
	if in == nil {
		if in = c.pick(); in == nil {
			return reply{}, errors.New("no instance runs")
		}
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, in.url(path), rd)
	if err != nil {
		return reply{}, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := plainClient.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return reply{}, err
	}
	return reply{code: resp.StatusCode, body: string(b), header: resp.Header}, nil
}

// sandbox is a sandbox as a client holds it: its ID and a token.
type sandbox struct {
	id    podid.ID
	token string
}

func (sb sandbox) auth() string { return "Bearer " + sb.token }
func (sb sandbox) path(p string) string {
	return "/api/v1/sandboxes/" + sb.id.String() + p
}

// create creates a sandbox through in (nil: the balancer) as FeCode
// does: ferund mints its ID, a short one (ADR 0019), which the answer
// carries.
func (c *cluster) create(in *instance) sandbox {
	c.t.Helper()
	r := c.call(in, "POST", "/api/v1/sandboxes", tenantAuth, "{}")
	if r.code != http.StatusCreated {
		c.t.Fatalf("create: %d %s", r.code, r.body)
	}
	var out struct {
		Token  string `json:"token"`
		Status struct {
			SandboxID   string `json:"sandboxId"`
			ClientState string `json:"clientState"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil || out.Token == "" {
		c.t.Fatalf("create: %s (%v)", r.body, err)
	}
	id, err := podid.Parse(out.Status.SandboxID)
	if err != nil || id.IsLegacy() {
		c.t.Fatalf("create: the new ID %q is not a short ID (%v)", out.Status.SandboxID, err)
	}
	return sandbox{id: id, token: out.Token}
}

// createID makes a sandbox under a given ID, which no client can any more
// (ADR 0019): its row and a token written to the database directly, as
// for an ID used again (a sandbox with a UUID made before, or a short ID
// drawn again after its sandbox was deleted). The ID must be new;
// attachable gives the row the template of an attachable sandbox.
func (c *cluster) createID(id podid.ID, attachable bool) sandbox {
	c.t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		c.t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(b[:])
	n := store.NewSandbox{ID: id, Owner: tenant, Image: testImage, VCPUs: c.cfg.DefaultVCPUs, MemoryMiB: c.cfg.DefaultMemoryMiB}
	if attachable {
		n.PodTemplate = &model.PodTemplate{Command: []string{"sleep", "infinity"},
			SecurityGroups: []string{model.SandboxGroupID(id)}, NameTables: []string{model.SandboxTableID(id)}}
	}
	err := c.st.InTx(bg, func(tx *store.Store) error {
		_, created, err := tx.CreateSandbox(bg, n)
		if err != nil {
			return err
		}
		if !created {
			return fmt.Errorf("sandbox %s exists", id)
		}
		_, err = tx.CreateSandboxToken(bg, store.HashToken(token), id, "default")
		return err
	})
	if err != nil {
		c.t.Fatalf("create %s: %v", id, err)
	}
	return sandbox{id: id, token: token}
}

// execResult is the REST answer of a tool call.
type execResult struct {
	ExitCode  int    `json:"exitCode"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

// exec runs command in the sandbox over REST through in (nil: the
// balancer).
func (c *cluster) exec(in *instance, sb sandbox, command string) reply {
	c.t.Helper()
	r, err := c.execErr(bg, in, sb, command)
	if err != nil {
		c.t.Fatalf("exec %q: %v", command, err)
	}
	return r
}

func (c *cluster) execErr(ctx context.Context, in *instance, sb sandbox, command string) (reply, error) {
	b, _ := json.Marshal(map[string]any{"command": command})
	return c.callErr(ctx, in, "POST", sb.path("/exec"), sb.auth(), string(b))
}

// run runs command in the sandbox and returns its output; anything but
// a 200 with exit code 0 fails the test.
func (c *cluster) run(in *instance, sb sandbox, command string) string {
	c.t.Helper()
	r := c.exec(in, sb, command)
	res, err := decodeExec(r)
	if err != nil {
		c.t.Fatalf("exec %q in %s: %v", command, sb.id, err)
	}
	if res.ExitCode != 0 {
		c.t.Fatalf("exec %q in %s: exit code %d: %s", command, sb.id, res.ExitCode, res.Output)
	}
	return res.Output
}

// decodeExec reads a 200 answer of exec.
func decodeExec(r reply) (execResult, error) {
	if r.code != http.StatusOK {
		return execResult{}, fmt.Errorf("%d %s", r.code, r.body)
	}
	var res execResult
	if err := json.Unmarshal([]byte(r.body), &res); err != nil {
		return execResult{}, fmt.Errorf("%v: %s", err, r.body)
	}
	return res, nil
}

// pause pauses the sandbox through REST and expects the 200.
func (c *cluster) pause(in *instance, sb sandbox) {
	c.t.Helper()
	if r := c.call(in, "POST", sb.path("/pause"), tenantAuth, ""); r.code != http.StatusOK {
		c.t.Fatalf("pause %s: %d %s", sb.id, r.code, r.body)
	}
}

// ------------------------------------------------------------------- MCP --

// bearer is FeCode's MCP transport: the sandbox token on every request.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// balancer sends every request to the next live instance on a new
// connection: an HTTP balancer without sticky sessions.
type balancer struct{ c *cluster }

func (b balancer) RoundTrip(r *http.Request) (*http.Response, error) {
	in := b.c.pick()
	if in == nil {
		return nil, errors.New("no instance runs")
	}
	r = r.Clone(r.Context())
	r.URL.Host = in.HTTPAddr()
	r.Host = ""
	return plainClient.Transport.RoundTrip(r)
}

// lbURL is the front door's URL behind the balancer.
const lbURL = "http://ferun.lb.test"

// legacy is the protocol version of an initialize-only client.
var legacy = &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"}

// mcpConnect opens an MCP session as FeCode does: go-sdk's streamable
// client without the standalone SSE stream, the sandbox token on every
// request; opts nil is FeCode's own call. endpoint lbURL goes through the
// balancer.
func (c *cluster) mcpConnect(endpoint, token string, opts *mcp.ClientSessionOptions) (*mcp.ClientSession, error) {
	var base http.RoundTripper = plainClient.Transport
	if strings.HasPrefix(endpoint, lbURL) {
		base = balancer{c}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "fecode-backend", Version: "v1"}, nil)
	ctx, cancel := context.WithTimeout(bg, time.Minute)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		DisableStandaloneSSE: true,
		HTTPClient:           &http.Client{Transport: bearer{token: token, base: base}},
	}, opts)
	if err != nil {
		return nil, err
	}
	c.t.Cleanup(func() { _ = cs.Close() })
	return cs, nil
}

// mcpSession is mcpConnect that fails the test on an error.
func (c *cluster) mcpSession(endpoint, token string, opts *mcp.ClientSessionOptions) *mcp.ClientSession {
	c.t.Helper()
	cs, err := c.mcpConnect(endpoint, token, opts)
	if err != nil {
		c.t.Fatalf("MCP session at %s: %v", endpoint, err)
	}
	return cs
}

// callTool calls a tool and returns its text, its isError and the call's
// error.
func callTool(cs *mcp.ClientSession, tool string, args map[string]any) (string, bool, error) {
	ctx, cancel := context.WithTimeout(bg, 2*time.Minute)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError, nil
}

// -------------------------------------------------------------- database --

// row reads a sandbox's row.
func (c *cluster) row(id podid.ID) store.Sandbox {
	c.t.Helper()
	sb, err := c.st.GetSandbox(bg, id)
	if err != nil {
		c.t.Fatalf("sandbox %s: %v", id, err)
	}
	return sb
}

// waitState waits until the sandbox's row is in state without an op in
// flight, and returns it.
func (c *cluster) waitState(id podid.ID, state string, within time.Duration) store.Sandbox {
	c.t.Helper()
	var sb store.Sandbox
	eventually(c.t, within, "sandbox "+id.String()+" "+state, func() bool {
		var err error
		sb, err = c.st.GetSandbox(bg, id)
		return err == nil && sb.ClientState == state && sb.Op == ""
	})
	return sb
}

// count runs a query that returns one integer.
func (c *cluster) count(query string, args ...any) int {
	c.t.Helper()
	var n int
	if err := c.db.Pool.QueryRow(bg, query, args...).Scan(&n); err != nil {
		c.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// sql runs a statement.
func (c *cluster) sql(query string, args ...any) {
	c.t.Helper()
	if _, err := c.db.Pool.Exec(bg, query, args...); err != nil {
		c.t.Fatalf("%s: %v", query, err)
	}
}

// wake wakes the leader's loops whose names contain one of hints, as the
// instances' own writes do, instead of waiting for their intervals.
func (c *cluster) wake(hints ...string) {
	c.t.Helper()
	for _, h := range hints {
		if err := db.Notify(bg, c.db.Pool, db.ChanLeader, h); err != nil {
			c.t.Fatal(err)
		}
	}
}

// onEnd runs f when the cluster has stopped: after its instances and its
// nodes, before its database is dropped.
func (c *cluster) onEnd(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ends = append(c.ends, f)
}

// dbProxy forwards connections to the cluster's PostgreSQL, so that a test
// can cut an instance off its database. While cut, nothing passes in
// either direction and the connections stay open, as on a network that
// drops every packet: the instance's statements hang, and to the database
// and the other instances it looks crashed. Healing closes the connections
// that lost bytes in the cut and forwards new ones again.
type dbProxy struct {
	// dsn is the cluster's database URL through the proxy.
	dsn             string
	ln              net.Listener
	network, target string
	wg              sync.WaitGroup

	mu     sync.Mutex
	cut    bool
	healed chan struct{} // closed when the cut ends
	closed bool
	conns  map[net.Conn]struct{}
}

// newDBProxy starts a proxy to the cluster's database. It is healed before
// the cluster stops, so that its instances stop with their database, and
// closed when the cluster has stopped.
func (c *cluster) newDBProxy() *dbProxy {
	c.t.Helper()
	pc, err := pgconn.ParseConfig(c.url)
	if err != nil {
		c.t.Fatal(err)
	}
	port := strconv.Itoa(int(pc.Port))
	network, target := "tcp", net.JoinHostPort(pc.Host, port)
	if strings.HasPrefix(pc.Host, "/") {
		network, target = "unix", filepath.Join(pc.Host, ".s.PGSQL."+port)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.t.Fatal(err)
	}
	_, lport, _ := net.SplitHostPort(ln.Addr().String())
	p := &dbProxy{dsn: withParams(c.t, c.url, "host", "127.0.0.1", "port", lport), ln: ln, network: network,
		target: target, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.serve()
	c.t.Cleanup(p.heal)
	c.onEnd(p.close)
	return p
}

// cutOff stops all traffic through the proxy until heal.
func (p *dbProxy) cutOff() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.cut {
		p.cut, p.healed = true, make(chan struct{})
	}
}

// heal ends a cut.
func (p *dbProxy) heal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cut {
		p.cut = false
		close(p.healed)
	}
}

// close stops the proxy and closes its connections.
func (p *dbProxy) close() {
	p.heal()
	p.mu.Lock()
	p.closed = true
	conns := make([]net.Conn, 0, len(p.conns))
	for conn := range p.conns {
		conns = append(conns, conn)
	}
	p.mu.Unlock()
	_ = p.ln.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
	p.wg.Wait()
}

func (p *dbProxy) serve() {
	defer p.wg.Done()
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		if !p.track(conn) {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.untrack(conn)
			// A connection made in the cut is held, as its SYN would be.
			if !p.pass() {
				return
			}
			up, err := net.DialTimeout(p.network, p.target, 10*time.Second)
			if err != nil || !p.track(up) {
				return
			}
			defer p.untrack(up)
			done := make(chan struct{})
			go func() {
				defer close(done)
				p.pipe(conn, up)
			}()
			p.pipe(up, conn)
			<-done
		}()
	}
}

// pipe copies from src to dst until either fails or a cut swallowed bytes;
// then it closes both, which ends the other direction too.
func (p *dbProxy) pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if !p.pass() {
				return
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// pass reports whether traffic may flow. In a cut it holds the caller
// until the cut ends and then reports false.
func (p *dbProxy) pass() bool {
	p.mu.Lock()
	if !p.cut {
		p.mu.Unlock()
		return true
	}
	healed := p.healed
	p.mu.Unlock()
	<-healed
	return false
}

// track registers a connection for close; false (and the connection
// closed) once the proxy is closed.
func (p *dbProxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = conn.Close()
		return false
	}
	p.conns[conn] = struct{}{}
	return true
}

func (p *dbProxy) untrack(conn net.Conn) {
	_ = conn.Close()
	p.mu.Lock()
	delete(p.conns, conn)
	p.mu.Unlock()
}

// ------------------------------------------------------------------ wait --

// eventually waits up to within for cond, checking every 50 ms.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// never checks for d that cond stays false.
func never(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("%s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
