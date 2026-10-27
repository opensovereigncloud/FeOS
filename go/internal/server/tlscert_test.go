// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// writePair writes a new self-signed certificate for 127.0.0.1 and its key
// to certFile and keyFile and returns the certificate's serial.
func writePair(t *testing.T, certFile, keyFile string) *big.Int {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "front"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "front"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return serial
}

func servedSerial(t *testing.T, f *frontCert) *big.Int {
	t.Helper()
	c, err := f.GetCertificate(nil)
	if err != nil || c == nil || c.Leaf == nil {
		t.Fatalf("GetCertificate: %v %v", c, err)
	}
	return c.Leaf.SerialNumber
}

// The front doors' certificate is loaded at once, and loaded again when
// its files change; a pair that does not load keeps the one before.
func TestFrontCertReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if _, err := loadFrontCert(certFile, keyFile, dbtest.Logger(t)); err == nil || !strings.Contains(err.Error(), "--tls-cert") {
		t.Fatalf("missing files: %v", err)
	}
	first := writePair(t, certFile, keyFile)
	f, err := loadFrontCert(certFile, keyFile, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := servedSerial(t, f); got.Cmp(first) != 0 {
		t.Fatalf("serves %v, want %v", got, first)
	}
	if changed, err := f.load(); changed || err != nil {
		t.Fatalf("unchanged files: %v %v", changed, err)
	}
	renewed := writePair(t, certFile, keyFile)
	if changed, err := f.load(); !changed || err != nil {
		t.Fatalf("renewed files: %v %v", changed, err)
	}
	if got := servedSerial(t, f); got.Cmp(renewed) != 0 {
		t.Fatalf("serves %v after the renewal, want %v", got, renewed)
	}
	// Half a rotation: the certificate replaced, the key not yet.
	otherDir := t.TempDir()
	writePair(t, certFile, filepath.Join(otherDir, "tls.key"))
	if _, err := f.load(); err == nil {
		t.Fatal("a certificate without its key loaded")
	}
	if got := servedSerial(t, f); got.Cmp(renewed) != 0 {
		t.Fatalf("serves %v after a failed load, want %v", got, renewed)
	}
}

// An instance with --tls-cert fails in New when a file is missing, and
// serves a certificate renewed on disk without a restart.
func TestFrontDoorsTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	cfg := testConfig(dbtest.NewDatabase(t), testCA(t))
	cfg.TLSCert, cfg.TLSKey = certFile, keyFile
	if _, err := New(context.Background(), cfg, dbtest.Logger(t)); err == nil || !strings.Contains(err.Error(), "--tls-cert") {
		t.Fatalf("New without the files: %v", err)
	}

	first := writePair(t, certFile, keyFile)
	i, err := New(context.Background(), cfg, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	i.front.every = 20 * time.Millisecond
	started(t, i)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	defer client.CloseIdleConnections()
	served := func() *big.Int {
		t.Helper()
		resp, err := client.Get("https://" + i.HTTPAddr() + "/healthz")
		if err != nil {
			return nil
		}
		resp.Body.Close()
		client.CloseIdleConnections()
		if resp.StatusCode != 200 || resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
			t.Fatalf("healthz: %d %v", resp.StatusCode, resp.TLS)
		}
		return resp.TLS.PeerCertificates[0].SerialNumber
	}
	deadline := time.Now().Add(10 * time.Second)
	for got := served(); got == nil || got.Cmp(first) != 0; got = served() {
		if time.Now().After(deadline) {
			t.Fatalf("serves %v, want %v", got, first)
		}
		time.Sleep(10 * time.Millisecond)
	}
	renewed := writePair(t, certFile, keyFile)
	deadline = time.Now().Add(10 * time.Second)
	for got := served(); got == nil || got.Cmp(renewed) != 0; got = served() {
		if time.Now().After(deadline) {
			t.Fatalf("serves %v after the renewal on disk, want %v", got, renewed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
