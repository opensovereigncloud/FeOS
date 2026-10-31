// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sinklog

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	otlplogsv1 "github.com/gardenlinux/feos/go/gen/otlp/collector/logs/v1"
	otlptracev1 "github.com/gardenlinux/feos/go/gen/otlp/collector/trace/v1"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// Identity returns the ingress's current certificate and the platform's
// trust bundle; an error while it has none.
type Identity func() (*tls.Certificate, *x509.CertPool, error)

// Client exports batches to the sink's ingest.
type Client struct {
	cc     *grpc.ClientConn
	logs   otlplogsv1.LogsServiceClient
	traces otlptracev1.TraceServiceClient
}

// NewClient returns a client of the ingest at endpoint (host:port): the
// name is resolved by gRPC's DNS resolver, each of its addresses may
// serve, and a lost connection is made again on the next call. It
// connects on the first Export.
func NewClient(endpoint, td string, id Identity) (*Client, error) {
	cc, err := grpc.NewClient("dns:///"+endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(ClientTLS(td, id))),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
	)
	if err != nil {
		return nil, fmt.Errorf("sink %s: %w", endpoint, err)
	}
	return &Client{cc: cc, logs: otlplogsv1.NewLogsServiceClient(cc), traces: otlptracev1.NewTraceServiceClient(cc)}, nil
}

// Export sends one request and returns once the sink stored it.
func (c *Client) Export(ctx context.Context, req *otlplogsv1.ExportLogsServiceRequest) error {
	_, err := c.logs.Export(ctx, req)
	return err
}

// ExportSpans sends one request of spans and returns once the sink
// stored it, with the number of spans it rejected.
func (c *Client) ExportSpans(ctx context.Context, req *otlptracev1.ExportTraceServiceRequest) (rejected int64, err error) {
	resp, err := c.traces.Export(ctx, req)
	if err != nil {
		return 0, err
	}
	return resp.GetPartialSuccess().GetRejectedSpans(), nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.cc.Close() }

// ClientTLS is the ingress's side of the ingest's TLS, as a node's
// (sink.md, The node): TLS 1.3, the current certificate of id, and a
// server certificate that verifies to the current bundle for server
// authentication, has no IP address and names a sink
// (spiffe://<td>/sink/<name>) as its one URI. The name dialled is not
// checked, so the endpoint may be an address.
func ClientTLS(td string, id Identity) *tls.Config {
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
		// Verified in VerifyConnection, by the bundle and the URI.
		InsecureSkipVerify: true,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, _, err := id()
			return cert, err
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, bundle, err := id()
			if err != nil {
				return err
			}
			return VerifySink(bundle, td, cs.PeerCertificates)
		},
	}
}

// VerifySink checks the sink's chain: it verifies to bundle for server
// authentication, its leaf has no IP address, and its one URI names a
// sink of td.
func VerifySink(bundle *x509.CertPool, td string, chain []*x509.Certificate) error {
	if len(chain) == 0 {
		return errors.New("the sink presented no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	leaf := chain[0]
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: bundle, Intermediates: inter,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return fmt.Errorf("the sink's certificate: %w", err)
	}
	if len(leaf.IPAddresses) > 0 {
		return errors.New("the sink's certificate names an IP address")
	}
	if _, err := pki.ParseSinkID(td, leaf); err != nil {
		return fmt.Errorf("the sink's certificate: %w", err)
	}
	return nil
}
