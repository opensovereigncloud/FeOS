// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// Scenario 1: joins. A server of another CA never sees the token; the
// token then joins the node at the control plane, which marks it ready
// once it reports; a used token is refused; a Join repeated with the same
// key is answered again, on another instance too; an unpinned token for an
// existing name gets ALREADY_EXISTS.
func TestJoin(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	a := c.start("a")
	b := c.start("b")

	token := c.joinToken("n1")
	foreign := newForeignServer(t, c)
	cfg := c.nodeConfig("n1", token)
	cfg.Addrs = []string{foreign.addr}
	impostor := c.runNode(cfg)
	if err := impostor.ended(t, 30*time.Second); !errors.Is(err, testnode.ErrUntrusted) {
		t.Fatalf("the node joined a server of another CA: %v", err)
	}
	foreign.idle()
	if foreign.handshakes.Load() == 0 || foreign.received.Load() != 0 {
		t.Fatalf("the server of another CA: %d handshakes, %d bytes received after one", foreign.handshakes.Load(),
			foreign.received.Load())
	}
	if n := c.count("SELECT count(*) FROM join_tokens WHERE node_name = 'n1' AND used_at IS NULL"); n != 1 {
		t.Fatalf("%d unused tokens of n1: the token was spent", n)
	}

	// The token joins at the control plane; the node is ready on its
	// first report and connected to both instances.
	n1 := c.runNode(c.nodeConfig("n1", token))
	c.waitNode(n1)
	row, err := c.st.GetNode(bg, "n1")
	if err != nil || row.Status != model.NodeReady || row.CurrentSerial != n1.Serial() || row.Hostname != "n1" {
		t.Fatalf("node row %+v %v", row, err)
	}

	// The used token is refused, whatever name the node proposes.
	again := c.runNode(c.nodeConfig("n9", token))
	if err := again.ended(t, 30*time.Second); !errors.Is(err, testnode.ErrJoinRefused) {
		t.Fatalf("a used token: %v", err)
	}

	// A Join whose answer was lost is sent again with the same key, here
	// to another instance, and answered; another key is refused.
	unpinned := c.joinToken("")
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	first, err := c.rawJoin(a.NodeAddr(), unpinned, "n2", key)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := c.rawJoin(b.NodeAddr(), unpinned, "n2", key)
	if err != nil {
		t.Fatalf("the repeated join: %v", err)
	}
	if first.GetName() != "n2" || repeat.GetName() != "n2" {
		t.Fatalf("names %q %q", first.GetName(), repeat.GetName())
	}
	for _, resp := range []*fleetv1.JoinResponse{first, repeat} {
		id := identityOf(t, "n2", key, resp.GetCertificate())
		if name, err := pki.ParseNodeID(trustDomain, id.leaf); err != nil || name != "n2" {
			t.Fatalf("certificate of %q: %v", name, err)
		}
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := c.rawJoin(a.NodeAddr(), unpinned, "n2", other); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("the used token with another key: %v", err)
	}

	// An unpinned token never takes over a name that exists.
	_, key3, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := c.rawJoin(b.NodeAddr(), c.joinToken(""), "n1", key3); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("an unpinned token for n1: %v", err)
	}
	// n1 is untouched by all of it.
	if row, err := c.st.GetNode(bg, "n1"); err != nil || row.Status != model.NodeReady || row.CurrentSerial != n1.Serial() {
		t.Fatalf("node row after the refusals %+v %v", row, err)
	}
}

// foreignServer is a TLS server on the join's ALPN with a certificate of
// another CA, for the control plane's name: it counts the handshakes that
// began and the bytes a client sent after one completed, as they arrive.
type foreignServer struct {
	addr       string
	handshakes atomic.Int64 // begun
	received   atomic.Int64
	conns      sync.WaitGroup // the connections being served
}

// idle waits until the server has served every connection to its end, so
// that received counts all a client sent. Call it once the clients are
// gone.
func (s *foreignServer) idle() { s.conns.Wait() }

func newForeignServer(t *testing.T, c *cluster) *foreignServer {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "ca")
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: dir, TrustDomain: trustDomain,
		DNSDomains: []string{dnsDomain}}); err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(dir, trustDomain)
	if err != nil {
		t.Fatal(err)
	}
	if iss.ControlPlaneDomain() != c.issuer.ControlPlaneDomain() {
		t.Fatalf("control-plane domains %s and %s", iss.ControlPlaneDomain(), c.issuer.ControlPlaneDomain())
	}
	cert, err := iss.IssueServer([]string{iss.ControlPlaneDomain()}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{*cert}, NextProtos: []string{alpnUplink}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &foreignServer{addr: ln.Addr().String()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns.Add(1)
			go func() {
				defer s.conns.Done()
				defer conn.Close()
				s.handshakes.Add(1)
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				if err := conn.(*tls.Conn).Handshake(); err != nil {
					return
				}
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					s.received.Add(int64(n))
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return s
}

// Scenario 1: a pinned re-join from another machine revokes the old
// certificate, and the old machine's connections close on every instance;
// a new handshake with it is refused.
func TestRejoin(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	instances := []*instance{c.start("a"), c.start("b"), c.start("c")}

	old := c.joinIdentity(instances[0].NodeAddr(), c.joinToken("n1"), "n1")
	var conns []*nodeConn
	for _, in := range instances {
		nc, err := c.dialNodeConn(in.NodeAddr(), old)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, nc)
	}
	eventually(t, 30*time.Second, "the old machine connected to every instance", func() bool {
		for _, in := range instances {
			if !in.Links.Connected("n1") {
				return false
			}
		}
		return true
	})

	// The node is set up anew on another machine with a token pinned to
	// its name.
	n1 := c.runNode(c.nodeConfig("n1", c.joinToken("n1")))
	for i, nc := range conns {
		select {
		case <-nc.closed():
		case <-time.After(30 * time.Second):
			t.Fatalf("the old machine's connection to instance %s stays", instances[i].name)
		}
	}
	c.waitNode(n1)
	if n := c.count("SELECT count(*) FROM node_certs WHERE serial = $1 AND revoked_reason = 'rejoined' AND revoked_at <= now()",
		old.serial()); n != 1 {
		t.Fatalf("the old certificate is not revoked (%d rows)", n)
	}
	for _, in := range instances {
		eventually(t, 10*time.Second, "one connection of n1 on "+in.name, func() bool { return in.Links.Connections("n1") == 1 })
	}
	// The old certificate gets no connection any more.
	nc, err := c.dialNodeConn(instances[1].NodeAddr(), old)
	if err == nil {
		select {
		case <-nc.closed():
		case <-time.After(15 * time.Second):
			t.Fatal("a handshake with the revoked certificate stays connected")
		}
	}
	if row, err := c.st.GetNode(bg, "n1"); err != nil || row.Forked() || row.Status != model.NodeReady {
		t.Fatalf("node row %+v %v", row, err)
	}
}

// Scenario 1: a removed node is refused: its connections close, the node
// learns that it must join again, and its RenewCertificate gets
// PERMISSION_DENIED from the refusal server: that server answers Join so
// too, where the uplink server would answer UNIMPLEMENTED.
func TestRemovedNode(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	instances := []*instance{c.start("a"), c.start("b")}
	n2 := c.join("n2")
	tc, _ := n2.Certificate()
	id := identity{name: "n2", cert: tc, leaf: tc.Leaf, bundle: c.issuer.Bundle()}

	if r := c.call(nil, "DELETE", "/api/v1/admin/nodes/n2", adminAuth, ""); r.code != 200 {
		t.Fatalf("remove: %d %s", r.code, r.body)
	}
	if err := n2.ended(t, 60*time.Second); !errors.Is(err, testnode.ErrRefused) {
		t.Fatalf("the removed node: %v", err)
	}
	for _, in := range instances {
		eventually(t, 30*time.Second, "no connection of n2 on "+in.name, func() bool { return in.Links.Connections("n2") == 0 })
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, err := pki.CreateCSR(key, "n2")
	if err != nil {
		t.Fatal(err)
	}
	const refusal = "node n2 was removed; join it again"
	for _, in := range instances {
		up := c.rawUplink(in.NodeAddr(), id)
		_, err = up.RenewCertificate(bg, &fleetv1.RenewCertificateRequest{Csr: csr})
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != refusal {
			t.Fatalf("RenewCertificate of the removed node on %s: %v, want PERMISSION_DENIED %q", in.name, err, refusal)
		}
		_, err = up.Join(bg, &fleetv1.JoinRequest{})
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != refusal {
			t.Fatalf("Join on the removed node's uplink on %s: %v, want PERMISSION_DENIED %q", in.name, err, refusal)
		}
	}
	if row, err := c.st.GetNode(bg, "n2"); err != nil || row.Status != model.NodeRemoved {
		t.Fatalf("node row %+v %v", row, err)
	}
}

// Scenario 1: a connection closes when its certificate expires.
func TestCertificateExpiry(t *testing.T) {
	t.Parallel()
	c := newCluster(t, func(cfg *config.Config) { cfg.NodeCertTTL = 8 * time.Second })
	a := c.start("a")
	id := c.joinIdentity(a.NodeAddr(), c.joinToken("e1"), "e1")
	nc, err := c.dialNodeConn(a.NodeAddr(), id)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the node connection", func() bool { return a.Links.Connected("e1") })
	select {
	case <-nc.closed():
		if early := time.Until(id.leaf.NotAfter); early > time.Second {
			t.Fatalf("the connection closed %v before its certificate expired", early)
		}
	case <-time.After(time.Until(id.leaf.NotAfter) + 10*time.Second):
		t.Fatal("the connection outlived its certificate")
	}
	eventually(t, 5*time.Second, "no connection of e1", func() bool { return !a.Links.Connected("e1") })
}

// Scenario 1: a renewal followed by a ferun_node for the node within the
// drain window does not fork it; a handshake with the superseded
// certificate after its grace does, and the node must join again.
func TestRenewalAndFork(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	a := c.start("a")
	c.start("b")
	n1 := c.join("n1")
	sb := c.create(nil)
	if out := c.run(nil, sb, "echo before"); out != "before\n" {
		t.Fatalf("exec %q", out)
	}

	tc, _ := n1.Certificate()
	old := identity{name: "n1", cert: tc, leaf: tc.Leaf, bundle: c.issuer.Bundle()}
	// An uplink of the old certificate with an events stream that stays
	// open through the renewal: the stream keeps the uplink, which the
	// re-checks then find superseded, from closing.
	up := c.rawUplink(a.NodeAddr(), old)
	sctx, endStream := context.WithCancel(bg)
	defer endStream()
	stream, err := up.PublishEvents(sctx)
	if err != nil {
		t.Fatal(err)
	}
	start := n1.StartID()
	acked := func(what string) {
		t.Helper()
		if err := stream.Send(&fleetv1.EventBatch{StartId: start}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if ack, err := stream.Recv(); err != nil || ack.GetStartId() != start {
			t.Fatalf("%s: ack %v %v", what, ack, err)
		}
	}
	acked("the old certificate's stream before the renewal")

	if err := n1.Renew(bg); err != nil {
		t.Fatal(err)
	}
	if n1.Serial() == old.serial() {
		t.Fatal("the renewal kept the serial")
	}
	eventually(t, 30*time.Second, "the new certificate current and the old superseded", func() bool {
		row, err := c.st.GetNode(bg, "n1")
		return err == nil && row.CurrentSerial == n1.Serial() &&
			c.count("SELECT count(*) FROM node_certs WHERE serial = $1 AND revoked_reason = 'superseded' AND revoked_at > now()",
				old.serial()) == 1
	})

	// Every instance re-checks the node's connections: the old uplink is
	// superseded, within its grace. It takes no new call, and its stream
	// in flight goes on.
	if err := db.Notify(bg, c.db.Pool, db.ChanNode, "n1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the old uplink superseded", func() bool {
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		s2, err := up.PublishEvents(ctx)
		if err == nil {
			if err = s2.Send(&fleetv1.EventBatch{StartId: start}); err == nil {
				_, err = s2.Recv()
			}
		}
		return status.Code(err) == codes.Unavailable && strings.Contains(status.Convert(err).Message(), "certificate superseded")
	})
	time.Sleep(2 * time.Second)
	if row, err := c.st.GetNode(bg, "n1"); err != nil || row.Forked() {
		t.Fatalf("the node forked within the drain window: %+v %v", row, err)
	}
	acked("the old certificate's stream after the re-checks")
	endStream()
	if out := c.run(nil, sb, "echo after"); out != "after\n" {
		t.Fatalf("exec %q", out)
	}
	select {
	case <-n1.done:
		t.Fatalf("the node ended: %v", n1.err)
	default:
	}

	// The grace has passed (its revocation time is moved into the past);
	// a new handshake with the old certificate means that two machines
	// hold the identity.
	c.sql("UPDATE node_certs SET revoked_at = now() - interval '1 second' WHERE serial = $1", old.serial())
	if nc, err := c.dialNodeConn(a.NodeAddr(), old); err == nil {
		select {
		case <-nc.closed():
		case <-time.After(15 * time.Second):
			t.Fatal("the superseded certificate stays connected past its grace")
		}
	}
	eventually(t, 15*time.Second, "the node forked", func() bool {
		row, err := c.st.GetNode(bg, "n1")
		return err == nil && row.Forked()
	})
	if err := n1.ended(t, 60*time.Second); !errors.Is(err, testnode.ErrRefused) {
		t.Fatalf("the forked node: %v", err)
	}
}
