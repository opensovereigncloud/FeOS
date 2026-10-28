// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pairPEM returns a self-signed certificate for cn and its key, in PEM.
func pairPEM(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
}

// writePair writes a self-signed pair for cn into dir and returns the
// files' names.
func writePair(t *testing.T, dir, cn string) (certFile, keyFile string) {
	t.Helper()
	certPEM, keyPEM := pairPEM(t, cn)
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeFile(t, certFile, certPEM)
	writeFile(t, keyFile, keyPEM)
	return certFile, keyFile
}

func writeFile(t *testing.T, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(name, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func servedCN(t *testing.T, c *Certs) string {
	t.Helper()
	pair, err := c.GetCertificate(&tls.ClientHelloInfo{ServerName: "any.example"})
	if err != nil || pair == nil || pair.Leaf == nil {
		t.Fatalf("certificate %v %v", pair, err)
	}
	return pair.Leaf.Subject.CommonName
}

// A changed pair is loaded; one that does not load keeps the
// certificate in use, and is logged once.
func TestCertsReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "*.one.example")
	var logs bytes.Buffer
	c, err := LoadPair(certFile, keyFile, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got := servedCN(t, c); got != "*.one.example" {
		t.Fatalf("served %q", got)
	}
	if changed, err := c.reload(); changed || err != nil {
		t.Fatalf("unchanged files: %v %v", changed, err)
	}
	writePair(t, dir, "*.two.example")
	c.turn()
	if got := servedCN(t, c); got != "*.two.example" || c.Leaf().Subject.CommonName != "*.two.example" {
		t.Fatalf("after a change: %q", got)
	}
	if !strings.Contains(logs.String(), "loaded a new TLS certificate") {
		t.Errorf("logs: %s", logs.String())
	}
	// A new certificate beside the old key: no pair.
	certPEM, _ := pairPEM(t, "*.three.example")
	writeFile(t, certFile, certPEM)
	c.turn()
	c.turn()
	if got := servedCN(t, c); got != "*.two.example" {
		t.Fatalf("a bad pair replaced the certificate: %q", got)
	}
	if n := strings.Count(logs.String(), "do not load"); n != 1 {
		t.Errorf("logged a bad pair %d times: %s", n, logs.String())
	}
	// Garbage and a missing file as well.
	writeFile(t, certFile, []byte("not a certificate"))
	if _, err := c.reload(); err == nil {
		t.Error("loaded garbage")
	}
	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	if _, err := c.reload(); err == nil {
		t.Error("loaded without a key")
	}
	if got := servedCN(t, c); got != "*.two.example" {
		t.Fatalf("served %q", got)
	}
	// The pair completed: loaded.
	writePair(t, dir, "*.four.example")
	c.turn()
	if got := servedCN(t, c); got != "*.four.example" {
		t.Fatalf("served %q", got)
	}
	if _, err := LoadPair(filepath.Join(dir, "missing"), keyFile, nil); err == nil {
		t.Error("loaded a missing pair")
	}
	cfg := c.TLSConfig()
	if cfg.MinVersion != tls.VersionTLS12 || strings.Join(cfg.NextProtos, ",") != "h2,http/1.1" {
		t.Errorf("TLS config %v %v", cfg.MinVersion, cfg.NextProtos)
	}
}
