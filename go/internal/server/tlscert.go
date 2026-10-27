// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// frontCertEvery is how often a running instance re-reads --tls-cert and
// --tls-key.
const frontCertEvery = time.Minute

// frontCert is the front doors' certificate, from --tls-cert and
// --tls-key. New loads it, so that a missing or bad file fails the start
// before the instance writes anything. Run re-reads both files every
// minute and serves a pair that changed from the next handshake on, so
// that a certificate renewed on disk (cert-manager, an ACME client) is
// served without a restart. A pair that does not load, such as one whose
// certificate was replaced and whose key not yet, keeps the pair before
// it.
type frontCert struct {
	certFile, keyFile string
	log               *slog.Logger
	every             time.Duration

	cert atomic.Pointer[tls.Certificate]
	// The files' contents the served pair was loaded from; only load
	// touches them, from New and then from Run alone.
	certPEM, keyPEM []byte
}

// loadFrontCert loads the pair of certFile and keyFile.
func loadFrontCert(certFile, keyFile string, log *slog.Logger) (*frontCert, error) {
	f := &frontCert{certFile: certFile, keyFile: keyFile, log: log, every: frontCertEvery}
	if _, err := f.load(); err != nil {
		return nil, err
	}
	return f, nil
}

// load reads both files and serves their pair when either changed. It
// reports whether it did.
func (f *frontCert) load() (bool, error) {
	certPEM, err := os.ReadFile(f.certFile)
	if err != nil {
		return false, fmt.Errorf("--tls-cert: %w", err)
	}
	keyPEM, err := os.ReadFile(f.keyFile)
	if err != nil {
		return false, fmt.Errorf("--tls-key: %w", err)
	}
	if f.cert.Load() != nil && bytes.Equal(certPEM, f.certPEM) && bytes.Equal(keyPEM, f.keyPEM) {
		return false, nil
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false, fmt.Errorf("--tls-cert and --tls-key: %w", err)
	}
	f.certPEM, f.keyPEM = certPEM, keyPEM
	f.cert.Store(&pair)
	return true, nil
}

// GetCertificate is the front doors' tls.Config.GetCertificate: the pair
// loaded last.
func (f *frontCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return f.cert.Load(), nil
}

// Run re-reads the files every minute until ctx ends.
func (f *frontCert) Run(ctx context.Context) error {
	t := time.NewTicker(f.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		changed, err := f.load()
		switch {
		case err != nil:
			f.log.Warn("cannot reload the front doors' certificate; serving the one loaded before", "err", err)
		case changed:
			var notAfter time.Time
			if leaf := f.cert.Load().Leaf; leaf != nil {
				notAfter = leaf.NotAfter
			}
			f.log.Info("reloaded the front doors' certificate", "file", f.certFile, "not_after", notAfter)
		}
	}
}
