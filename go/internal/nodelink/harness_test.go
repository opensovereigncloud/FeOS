// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	portforwardv1 "github.com/gardenlinux/feos/go/gen/feos/portforward/v1"
	toolv1 "github.com/gardenlinux/feos/go/gen/feos/tool/v1"
	"github.com/gardenlinux/feos/go/internal/pki"
)

const (
	testTD     = "feos.test"
	testDomain = "example.test"
	serverName = "ferun." + testDomain
)

// logs counts what the port logs, by message.
type logs struct {
	mu   sync.Mutex
	msgs map[string]int
}

func (l *logs) Enabled(context.Context, slog.Level) bool { return true }
func (l *logs) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.msgs[r.Message]++
	l.mu.Unlock()
	return nil
}
func (l *logs) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logs) WithGroup(string) slog.Handler      { return l }
func (l *logs) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.msgs[msg]
}

// answer is what the in-test checker answers for a serial.
type answer struct {
	revokeAt time.Time
	err      error
}

type checkCall struct {
	node, serial, addr string
	atAccept           bool
}

// checker is the in-test CheckNode: answers per serial, set by the test,
// and a log of the calls. A check's answer is the one set when it starts,
// as a query reads the database when it runs, however long it takes to
// return.
type checker struct {
	mu      sync.Mutex
	answers map[string]answer
	recheck map[string]answer // answers for re-checks only, when set
	calls   []checkCall
	hook    func(c checkCall) // runs before answering, outside the lock
}

func (c *checker) set(serial string, revokeAt time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answers[serial] = answer{revokeAt, err}
}

func (c *checker) setRecheck(serial string, revokeAt time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recheck[serial] = answer{revokeAt, err}
}

func (c *checker) check(ctx context.Context, node, serial, addr string, atAccept bool) (time.Time, error) {
	call := checkCall{node, serial, addr, atAccept}
	c.mu.Lock()
	c.calls = append(c.calls, call)
	hook := c.hook
	a := c.answers[serial]
	if r, ok := c.recheck[serial]; ok && !atAccept {
		a = r
	}
	c.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	return a.revokeAt, a.err
}

func (c *checker) setHook(h func(checkCall)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hook = h
}

func (c *checker) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *checker) count(serial string, atAccept bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, call := range c.calls {
		if call.serial == serial && call.atAccept == atAccept {
			n++
		}
	}
	return n
}

// env is an instance's nodes' port with an in-test CA, checker and
// FleetService stubs.
type env struct {
	t        *testing.T
	iss      *pki.Issuer
	srv      *Server
	chk      *checker
	logs     *logs
	fleet    *fleetStub
	served   chan error
	cancel   context.CancelFunc
	mu       sync.Mutex
	present  []checkCall
	nodeConn []string
}

type envOpt struct {
	cfg   func(*Config)
	tune  func(*Server)
	joins *joinStub // the join server's FleetService, when not the default
}

// caIss is the package's CA, made once by TestMain.
var caIss *pki.Issuer

func TestMain(m *testing.M) {
	base, err := os.MkdirTemp("", "nodelink-ca-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dir := filepath.Join(base, "ca")
	err = pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: dir, TrustDomain: testTD, DNSDomains: []string{testDomain}})
	if err == nil {
		caIss, err = pki.LoadIssuer(dir, testTD)
	}
	if err != nil {
		os.RemoveAll(base)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(base)
	os.Exit(code)
}

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	iss := caIss
	src, err := pki.NewServerCertSource(iss, []string{serverName}, time.Hour, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, iss: iss, logs: &logs{msgs: map[string]int{}}, fleet: &fleetStub{},
		chk: &checker{answers: map[string]answer{}, recheck: map[string]answer{}}, served: make(chan error, 1)}
	cfg := Config{
		Addr:             "127.0.0.1:0",
		TrustDomain:      testTD,
		InstanceID:       "i1",
		Bundle:           iss.Bundle(),
		ServerCert:       src.GetCertificate,
		NodeWait:         200 * time.Millisecond,
		ConnDrainTimeout: time.Minute,
		MaxStreams:       1024,
		CheckNode:        e.chk.check,
		OnPresented: func(node, serial, addr string) {
			e.mu.Lock()
			e.present = append(e.present, checkCall{node: node, serial: serial, addr: addr})
			e.mu.Unlock()
		},
		OnNodeConn: func(node string) {
			e.mu.Lock()
			e.nodeConn = append(e.nodeConn, node)
			e.mu.Unlock()
		},
		Log: slog.New(e.logs),
	}
	for _, o := range opts {
		if o.cfg != nil {
			o.cfg(&cfg)
		}
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	joins := &joinStub{}
	for _, o := range opts {
		if o.tune != nil {
			o.tune(srv)
		}
		if o.joins != nil {
			joins = o.joins
		}
	}
	fleetv1.RegisterFleetServiceServer(srv.Uplink(), e.fleet)
	fleetv1.RegisterFleetServiceServer(srv.Join(), joins)
	e.srv = srv
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go func() { e.served <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		srv.Shutdown(sctx)
		cancel()
		select {
		case <-e.served:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return")
		}
	})
	return e
}

func (e *env) presented() []checkCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]checkCall(nil), e.present...)
}

func (e *env) nodeConns() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.nodeConn...)
}

func (e *env) addr() string { return e.srv.Addr().String() }

// tnode is a node's identity: its certificate and key.
type tnode struct {
	name   string
	cert   tls.Certificate
	leaf   *x509.Certificate
	serial string
}

func (e *env) node(name string, ttl time.Duration) *tnode {
	e.t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		e.t.Fatal(err)
	}
	csr, err := pki.CreateCSR(key, name)
	if err != nil {
		e.t.Fatal(err)
	}
	pc, leaf, err := e.iss.IssueNode(csr, name, ttl)
	if err != nil {
		e.t.Fatal(err)
	}
	return &tnode{name: name, cert: tls.Certificate{Certificate: pc.Chain, PrivateKey: key, Leaf: leaf}, leaf: leaf, serial: pki.Serial(leaf)}
}

// tlsConfig is the node's client configuration: TLS 1.3, no resumption,
// the control plane verified by name and URI.
func (e *env) tlsConfig(n *tnode, alpn ...string) *tls.Config {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
		RootCAs:    e.iss.Bundle(),
		NextProtos: alpn,
		VerifyConnection: func(cs tls.ConnectionState) error {
			raw := make([][]byte, len(cs.PeerCertificates))
			for i, c := range cs.PeerCertificates {
				raw[i] = c.Raw
			}
			return pki.VerifyControlPlane(e.iss.Bundle(), serverName, testTD, raw)
		},
	}
	if n != nil {
		cfg.Certificates = []tls.Certificate{n.cert}
	}
	return cfg
}

// watchedConn records when it is closed: the node side's view of a
// connection that ferund (or the node) ended.
type watchedConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func watch(c net.Conn) *watchedConn { return &watchedConn{Conn: c, closed: make(chan struct{})} }

func (w *watchedConn) Close() error {
	err := w.Conn.Close()
	w.once.Do(func() { close(w.closed) })
	return err
}

// oneConnListener serves one connection to a grpc.Server.
type oneConnListener struct {
	c      chan net.Conn
	closed chan struct{}
	once   sync.Once
	addr   net.Addr
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{c: make(chan net.Conn, 1), closed: make(chan struct{}), addr: c.LocalAddr()}
	l.c <- c
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.c:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *oneConnListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *oneConnListener) Addr() net.Addr { return l.addr }

// nodeSide is one node connection as the node has it: the node serves
// its feos.* APIs over the TLS connection it dialled.
type nodeSide struct {
	id    string
	conn  *watchedConn
	srv   *grpc.Server
	pods  *podStub
	tools *toolStub
}

// dialNode opens a node connection from n and serves the in-test node's
// services on it, with the keepalive enforcement feosd will have (§17).
func (e *env) dialNode(n *tnode, id string) *nodeSide {
	e.t.Helper()
	ns, err := e.tryDialNode(n, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ns
}

func (e *env) tryDialNode(n *tnode, id string) (*nodeSide, error) {
	c, err := tls.Dial("tcp", e.addr(), e.tlsConfig(n, ALPNNode))
	if err != nil {
		return nil, err
	}
	if c.ConnectionState().NegotiatedProtocol != ALPNNode {
		c.Close()
		return nil, fmt.Errorf("ALPN %q", c.ConnectionState().NegotiatedProtocol)
	}
	ns := &nodeSide{id: id, conn: watch(c), pods: newPodStub(id), tools: newToolStub()}
	ns.srv = grpc.NewServer(
		grpc.Creds(insecure.NewCredentials()),
		grpc.MaxConcurrentStreams(1024),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
	)
	podv1.RegisterPodServiceServer(ns.srv, ns.pods)
	toolv1.RegisterToolServiceServer(ns.srv, ns.tools)
	containerv1.RegisterContainerServiceServer(ns.srv, &containerStub{})
	portforwardv1.RegisterPortForwardServiceServer(ns.srv, &forwardStub{})
	go ns.srv.Serve(newOneConnListener(ns.conn))
	e.t.Cleanup(ns.srv.Stop)
	return ns, nil
}

// waitClosed waits until the node side sees its connection closed.
func (ns *nodeSide) waitClosed(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-ns.conn.closed:
	case <-time.After(within):
		t.Fatalf("node connection %s not closed within %v", ns.id, within)
	}
}

func (ns *nodeSide) isClosed() bool {
	select {
	case <-ns.conn.closed:
		return true
	default:
		return false
	}
}

// dialUplink opens uplinks from n (nil: without a certificate) through one
// gRPC channel; every dial is a new TLS connection, recorded in conns.
type uplink struct {
	cc    *grpc.ClientConn
	mu    sync.Mutex
	conns []*watchedConn
	fleet fleetv1.FleetServiceClient
}

func (e *env) dialUplink(n *tnode) *uplink {
	e.t.Helper()
	u := &uplink{}
	cfg := e.tlsConfig(n, ALPNUplink)
	cc, err := grpc.NewClient("passthrough:///"+serverName,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			c, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", e.addr())
			if err != nil {
				return nil, err
			}
			w := watch(c)
			u.mu.Lock()
			u.conns = append(u.conns, w)
			u.mu.Unlock()
			return w, nil
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 10 * time.Second, Timeout: 5 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cc.Close() })
	u.cc = cc
	u.fleet = fleetv1.NewFleetServiceClient(cc)
	return u
}

func (u *uplink) dials() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.conns)
}

// fleetStub is the uplink server's FleetService: it answers with the Peer
// that nodelink put into the context.
type fleetStub struct {
	fleetv1.UnimplementedFleetServiceServer
}

func (f *fleetStub) ReportTelemetry(ctx context.Context, _ *fleetv1.Telemetry) (*fleetv1.TelemetryResponse, error) {
	p, ok := NodeFromContext(ctx)
	if !ok {
		return nil, errors.New("no peer")
	}
	return &fleetv1.TelemetryResponse{Instances: []*fleetv1.ControlPlaneInstance{{Id: p.Node, NodeAddress: p.Serial}}}, nil
}

func (f *fleetStub) PublishEvents(stream grpc.BidiStreamingServer[fleetv1.EventBatch, fleetv1.EventAck]) error {
	p, ok := NodeFromContext(stream.Context())
	if !ok {
		return errors.New("no peer")
	}
	for {
		b, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&fleetv1.EventAck{StartId: p.Node + "/" + b.StartId}); err != nil {
			return err
		}
	}
}

// joinStub is the join server's FleetService: Join only. With a gate,
// every Join reports its name on entered and then waits for a token from
// the gate.
type joinStub struct {
	fleetv1.UnimplementedFleetServiceServer
	gate    chan struct{}
	entered chan string
}

func (j *joinStub) Join(ctx context.Context, req *fleetv1.JoinRequest) (*fleetv1.JoinResponse, error) {
	if j.gate != nil {
		j.entered <- req.Name
		select {
		case <-j.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &fleetv1.JoinResponse{Name: req.Name}, nil
}

// podStub is the in-test node's PodService. CreatePod waits for its gate;
// GetPod answers with the node side's id and the feos-pod-id it got.
type podStub struct {
	podv1.UnimplementedPodServiceServer
	id      string
	gate    chan struct{}
	entered chan string
}

func newPodStub(id string) *podStub {
	return &podStub{id: id, gate: make(chan struct{}), entered: make(chan string, 16)}
}

func (p *podStub) CreatePod(ctx context.Context, req *podv1.CreatePodRequest) (*podv1.CreatePodResponse, error) {
	p.entered <- req.PodId
	select {
	case <-p.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &podv1.CreatePodResponse{Pod: &podv1.PodInfo{PodId: req.PodId, Reason: p.id}}, nil
}

func (p *podStub) GetPod(ctx context.Context, req *podv1.GetPodRequest) (*podv1.PodInfo, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	var podID string
	if v := md.Get(PodIDMetadata); len(v) == 1 {
		podID = v[0]
	}
	return &podv1.PodInfo{PodId: podID, Reason: p.id}, nil
}

func (p *podStub) StreamPodEvents(req *podv1.StreamPodEventsRequest, s grpc.ServerStreamingServer[podv1.PodEvent]) error {
	if err := s.Send(&podv1.PodEvent{PodId: req.PodId}); err != nil {
		return err
	}
	<-s.Context().Done()
	return nil
}

// toolStub is the in-test node's ToolService: Read waits for its gate.
type toolStub struct {
	toolv1.UnimplementedToolServiceServer
	gate    chan struct{}
	entered chan struct{}
}

func newToolStub() *toolStub {
	return &toolStub{gate: make(chan struct{}), entered: make(chan struct{}, 64)}
}

func (s *toolStub) Read(ctx context.Context, req *toolv1.ReadRequest) (*toolv1.ReadResponse, error) {
	s.entered <- struct{}{}
	select {
	case <-s.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &toolv1.ReadResponse{Text: req.Path}, nil
}

// containerStub's Exec sends one frame and runs until it is cancelled.
type containerStub struct {
	containerv1.UnimplementedContainerServiceServer
}

func (containerStub) Exec(_ *containerv1.ExecRequest, s grpc.ServerStreamingServer[containerv1.ExecStreamFrame]) error {
	if err := s.Send(&containerv1.ExecStreamFrame{}); err != nil {
		return err
	}
	<-s.Context().Done()
	return nil
}

// forwardStub's Forward echoes until it is cancelled.
type forwardStub struct {
	portforwardv1.UnimplementedPortForwardServiceServer
}

func (forwardStub) Forward(s grpc.BidiStreamingServer[portforwardv1.ForwardRequest, portforwardv1.ForwardResponse]) error {
	for {
		if _, err := s.Recv(); err != nil {
			return nil
		}
		if err := s.Send(&portforwardv1.ForwardResponse{}); err != nil {
			return err
		}
	}
}

// eventually polls cond until it holds or within passes.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// never checks that cond stays false for d.
func never(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("happened: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) linkCount(node string) (links, uplinks int) {
	e.srv.mu.Lock()
	defer e.srv.mu.Unlock()
	st := e.srv.nodes[node]
	if st == nil {
		return 0, 0
	}
	return len(st.links), len(st.uplinks)
}

func toolClient(cc grpc.ClientConnInterface) toolv1.ToolServiceClient {
	return toolv1.NewToolServiceClient(cc)
}

func readReq(path string) *toolv1.ReadRequest { return &toolv1.ReadRequest{Path: path} }

func forwardClient(cc grpc.ClientConnInterface) portforwardv1.PortForwardServiceClient {
	return portforwardv1.NewPortForwardServiceClient(cc)
}

func forwardReq() *portforwardv1.ForwardRequest { return &portforwardv1.ForwardRequest{} }
