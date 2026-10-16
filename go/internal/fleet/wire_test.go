// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	fleetv1 "github.com/ironcore-dev/feos/go/gen/feos/fleet/v1"
	"github.com/ironcore-dev/feos/go/internal/db/dbtest"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/nodelink"
	"github.com/ironcore-dev/feos/go/internal/pki"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// wire is fleet behind a real nodes' port: the join, uplink and refusal
// servers of nodelink, with fleet's CheckNode and lineage.
type wire struct {
	*harness
	srv        *nodelink.Server
	serverName string
}

func newWire(t *testing.T) *wire {
	h := newHarness(t)
	serverName := h.iss.ControlPlaneDomain()
	src, err := pki.NewServerCertSource(h.iss, []string{serverName}, time.Hour, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := nodelink.New(nodelink.Config{
		Addr: "127.0.0.1:0", TrustDomain: testTD, InstanceID: "i1", Bundle: h.iss.Bundle(),
		ServerCert: src.GetCertificate, NodeWait: 200 * time.Millisecond, ConnDrainTimeout: time.Minute,
		CheckNode: h.s.CheckNode, OnPresented: h.s.OnPresented, OnNodeConn: h.s.OnNodeConn, Log: dbtest.Logger(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	// fleet re-checks and reconciles through the real port.
	h.s.links = srv
	fleetv1.RegisterFleetServiceServer(srv.Uplink(), h.s.UplinkServer())
	fleetv1.RegisterFleetServiceServer(srv.Join(), h.s.JoinServer())
	ctx, cancel := context.WithCancel(bg)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	runService(t, h)
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(bg, 10*time.Second)
		defer scancel()
		srv.Shutdown(sctx)
		cancel()
		<-served
	})
	return &wire{harness: h, srv: srv, serverName: serverName}
}

// client dials the port with ALPN h2: with cert, an uplink; without, a
// join connection, which checks the control plane against the token's
// pin before anything is sent.
func (w *wire) client(cert *tls.Certificate) fleetv1.FleetServiceClient {
	w.t.Helper()
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: w.serverName, NextProtos: []string{"h2"}}
	if cert == nil {
		cfg.InsecureSkipVerify = true // VerifyConnection checks the chain against the pin
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			return pki.VerifyJoinServer(w.iss.JoinPin(), w.serverName, testTD, rawCerts(cs))
		}
	} else {
		cfg.RootCAs = w.iss.Bundle()
		cfg.Certificates = []tls.Certificate{*cert}
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			return pki.VerifyControlPlane(w.iss.Bundle(), w.serverName, testTD, rawCerts(cs))
		}
	}
	addr := w.srv.Addr().String()
	cc, err := grpc.NewClient("passthrough:///"+w.serverName,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { cc.Close() })
	return fleetv1.NewFleetServiceClient(cc)
}

func rawCerts(cs tls.ConnectionState) [][]byte {
	raw := make([][]byte, len(cs.PeerCertificates))
	for i, c := range cs.PeerCertificates {
		raw[i] = c.Raw
	}
	return raw
}

// tlsCert makes the node's TLS certificate of an issued one.
func tlsCert(t *testing.T, c *fleetv1.Certificate, key ed25519.PrivateKey) *tls.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.GetChain()[0])
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: c.GetChain(), PrivateKey: key, Leaf: leaf}
}

func TestOverTheWire(t *testing.T) {
	w := newWire(t)
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()

	// Join over a connection without a certificate.
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, _ := pki.CreateCSR(key, "n1")
	join := w.client(nil)
	resp, err := join.Join(ctx, &fleetv1.JoinRequest{Token: w.token(""), Name: "n1", Csr: csr})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := w.st.GetNodeCert(bg, serialOf(t, resp.GetCertificate())); c.Peer == "" {
		t.Fatalf("join recorded without the peer's address: %+v", c)
	}
	// Only Join is served there.
	_, err = join.ReportTelemetry(ctx, &fleetv1.Telemetry{})
	wantCode(t, err, codes.Unimplemented, "")

	// The uplink with the new certificate: its presentation makes it the
	// node's current one; telemetry makes the node ready; events are
	// acknowledged.
	up := w.client(tlsCert(t, resp.GetCertificate(), key))
	if _, err := up.ReportTelemetry(ctx, &fleetv1.Telemetry{StartId: uuid.NewString(), Hostname: "n1"}); err != nil {
		t.Fatal(err)
	}
	if n := w.node("n1"); n.Status != model.NodeReady || n.CurrentSerial != serialOf(t, resp.GetCertificate()) {
		t.Fatalf("node after its first report %+v", n)
	}
	stream, err := up.PublishEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := uuid.New()
	if err := stream.Send(batch(start, 0, seqs(1, 2, uuid.NewString())...)); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil || ack.GetSeq() != 2 || ack.GetStartId() != start.String() {
		t.Fatalf("ack %v, %v", ack, err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	_, err = up.Join(ctx, &fleetv1.JoinRequest{})
	wantCode(t, err, codes.Unimplemented, "")

	// Renewal over the uplink.
	_, key2, _ := ed25519.GenerateKey(rand.Reader)
	csr2, _ := pki.CreateCSR(key2, "n1")
	renewed, err := up.RenewCertificate(ctx, &fleetv1.RenewCertificateRequest{Csr: csr2})
	if err != nil {
		t.Fatal(err)
	}
	up2 := w.client(tlsCert(t, renewed.GetCertificate(), key2))
	if _, err := up2.ReportTelemetry(ctx, &fleetv1.Telemetry{StartId: uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	if n := w.node("n1"); n.CurrentSerial != serialOf(t, renewed.GetCertificate()) {
		t.Fatalf("current serial after the renewal %q", n.CurrentSerial)
	}

	// The node is removed: its uplink is re-checked and closed on
	// ferun_node, and a new uplink lands on the refusal server, which
	// tells the node to join again.
	if err := w.st.InTx(bg, func(tx *store.Store) error {
		if _, err := tx.LockNode(bg, "n1", store.ForUpdate); err != nil {
			return err
		}
		_, err := tx.RemoveNode(bg, "n1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the removed node's uplink refused", 10*time.Second, func() bool {
		_, err := up2.ReportTelemetry(ctx, &fleetv1.Telemetry{StartId: uuid.NewString()})
		return err != nil
	})
	waitFor(t, "the refusal server's answer", 10*time.Second, func() bool {
		_, err := w.client(tlsCert(t, renewed.GetCertificate(), key2)).ReportTelemetry(ctx, &fleetv1.Telemetry{StartId: uuid.NewString()})
		code, msg := codes.Unknown, ""
		if err != nil {
			code, msg = statusOf(err)
		}
		return code == codes.PermissionDenied && msg == "node n1 was removed; join it again"
	})
}
