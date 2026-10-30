// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gardenlinux/feos/go/internal/pki"
)

// pathServiceCertificates is ferund's call that issues the certificate.
const pathServiceCertificates = "/api/v1/service-certificates"

// maxAnswer bounds an answer of ferund read.
const maxAnswer = 1 << 20

// identity is what one certificate gives: the certificate and its key,
// the bundle that verifies the nodes, and when to ask again.
type identity struct {
	cert    *tls.Certificate
	bundle  *x509.CertPool
	renewAt time.Time
}

// Identity holds the sink's certificate from ferund (sink.md, Identity):
// Run asks for one with the service token and again at half its life; a
// failure is retried after 5 s, doubling to a minute, while the old one
// serves.
type Identity struct {
	ferunURL, token, td, name string
	hc                        *http.Client
	log                       *slog.Logger
	cur                       atomic.Pointer[identity]
	ready                     chan struct{}
}

// NewIdentity returns the identity of instance name, not yet obtained.
func NewIdentity(ferunURL, token, td, name string, hc *http.Client, log *slog.Logger) *Identity {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Identity{ferunURL: strings.TrimSuffix(ferunURL, "/"), token: token, td: td, name: name, hc: hc, log: log,
		ready: make(chan struct{})}
}

// Ready is closed once the first certificate is there.
func (i *Identity) Ready() <-chan struct{} { return i.ready }

// Run obtains and renews the certificate until ctx ends.
func (i *Identity) Run(ctx context.Context) error {
	retry := 5 * time.Second
	for {
		var wait time.Duration
		id, err := i.obtain(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			i.log.Error("obtain the sink's certificate", "err", err, "retry", retry)
			wait, retry = retry, min(retry*2, time.Minute)
		} else {
			first := i.cur.Load() == nil
			i.cur.Store(id)
			if first {
				close(i.ready)
			}
			i.log.Info("obtained the sink's certificate", "notAfter", id.cert.Leaf.NotAfter, "renewAt", id.renewAt)
			wait, retry = max(time.Until(id.renewAt), time.Second), 5*time.Second
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

type certRequest struct {
	CSR string `json:"csr"`
}

type certAnswer struct {
	Certificate string    `json:"certificate"`
	Bundle      string    `json:"bundle"`
	NotAfter    time.Time `json:"notAfter"`
}

// obtain asks ferund for a certificate for a new Ed25519 key and checks
// it: its URI is ServerID, its key the new one, and it verifies to the
// bundle.
func (i *Identity) obtain(ctx context.Context) (*identity, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := pki.CreateCSR(key, i.name)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(certRequest{CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.ferunURL+pathServiceCertificates, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+i.token)
	req.Header.Set("Content-Type", "application/json")
	asked := time.Now()
	resp, err := i.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("POST %s: %s: %s", pathServiceCertificates, resp.Status, strings.TrimSpace(string(b)))
	}
	var a certAnswer
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("POST %s: %w", pathServiceCertificates, err)
	}
	chain, err := parseCerts(a.Certificate)
	if err != nil || len(chain) == 0 {
		return nil, fmt.Errorf("the answer's certificate: %v", errOr(err, "none"))
	}
	roots, err := parseCerts(a.Bundle)
	if err != nil || len(roots) == 0 {
		return nil, fmt.Errorf("the answer's bundle: %v", errOr(err, "none"))
	}
	leaf := chain[0]
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != pki.SinkID(i.td, i.name) {
		return nil, fmt.Errorf("the certificate's URI names are %v, not %s alone", leaf.URIs, pki.SinkID(i.td, i.name))
	}
	if pub, ok := leaf.PublicKey.(ed25519.PublicKey); !ok || !pub.Equal(key.Public()) {
		return nil, errors.New("the certificate is not of the key asked for")
	}
	pool := x509.NewCertPool()
	for _, r := range roots {
		pool.AddCert(r)
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, fmt.Errorf("the certificate does not verify to the bundle: %w", err)
	}
	der := make([][]byte, len(chain))
	for k, c := range chain {
		der[k] = c.Raw
	}
	notAfter := leaf.NotAfter
	if !a.NotAfter.IsZero() && a.NotAfter.Before(notAfter) {
		notAfter = a.NotAfter
	}
	return &identity{
		cert:    &tls.Certificate{Certificate: der, PrivateKey: key, Leaf: leaf},
		bundle:  pool,
		renewAt: asked.Add(notAfter.Sub(asked) / 2),
	}, nil
}

func errOr(err error, s string) any {
	if err != nil {
		return err
	}
	return s
}

// parseCerts reads the CERTIFICATE blocks of PEM text.
func parseCerts(text string) ([]*x509.Certificate, error) {
	rest := []byte(text)
	var out []*x509.Certificate
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return out, nil
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
}

// TLSConfig is the ingest's server configuration: the current
// certificate, TLS 1.3, and a client certificate that verifies to the
// current bundle for client authentication and names a node of the trust
// domain (pki.ParseNodeID).
func (i *Identity) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
		ClientAuth:             tls.RequireAnyClientCert,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			id := i.cur.Load()
			if id == nil {
				return nil, errors.New("no certificate yet")
			}
			return id.cert, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, err := i.verifyNode(cs.PeerCertificates)
			return err
		},
	}
}

// verifyNode checks a client's chain and returns its node's name.
func (i *Identity) verifyNode(chain []*x509.Certificate) (string, error) {
	id := i.cur.Load()
	if id == nil {
		return "", errors.New("no bundle yet")
	}
	if len(chain) == 0 {
		return "", errors.New("no client certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{Roots: id.bundle, Intermediates: inter,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return "", fmt.Errorf("client certificate: %w", err)
	}
	return pki.ParseNodeID(i.td, chain[0])
}
