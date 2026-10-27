// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

// ServerCertSource holds an instance's certificate for the nodes' port. It
// is issued by the issuer at construction and issued again, in memory, at
// three quarters of its lifetime by Run; an issuance that fails is retried
// every minute while the old certificate serves.
type ServerCertSource struct {
	iss   *Issuer
	names []string
	ttl   time.Duration
	log   *slog.Logger
	cur   atomic.Pointer[tls.Certificate]
	// issued is when the current certificate was issued; Run renews at
	// issued + ttl·3/4 (NotBefore is backdated and says nothing about it).
	issued atomic.Int64
	// retry is the wait after a failed issuance.
	retry time.Duration
	// issue is i.iss.IssueServer, replaceable by tests.
	issue func(names []string, ttl time.Duration) (*tls.Certificate, error)
}

// NewServerCertSource issues the first certificate for names with lifetime
// ttl and fails when it cannot.
func NewServerCertSource(iss *Issuer, names []string, ttl time.Duration, log *slog.Logger) (*ServerCertSource, error) {
	if iss == nil {
		return nil, errors.New("no issuer")
	}
	if log == nil {
		log = slog.Default()
	}
	s := &ServerCertSource{
		iss:   iss,
		names: append([]string(nil), names...),
		ttl:   ttl,
		log:   log,
		retry: time.Minute,
		issue: iss.IssueServer,
	}
	if err := s.renew(); err != nil {
		return nil, err
	}
	return s, nil
}

// GetCertificate returns the current certificate; it is tls.Config's
// GetCertificate.
func (s *ServerCertSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := s.cur.Load()
	if c == nil {
		return nil, errors.New("no server certificate")
	}
	return c, nil
}

// Run re-issues the certificate at three quarters of its lifetime until ctx
// ends.
func (s *ServerCertSource) Run(ctx context.Context) error {
	t := time.NewTimer(s.untilRenewal())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if err := s.renew(); err != nil {
			s.log.Error("renew the nodes' port certificate", "err", err, "retry", s.retry)
			t.Reset(s.retry)
			continue
		}
		s.log.Info("renewed the nodes' port certificate", "notAfter", s.cur.Load().Leaf.NotAfter)
		t.Reset(s.untilRenewal())
	}
}

func (s *ServerCertSource) renew() error {
	now := time.Now()
	c, err := s.issue(s.names, s.ttl)
	if err != nil {
		return err
	}
	s.cur.Store(c)
	s.issued.Store(now.UnixNano())
	return nil
}

func (s *ServerCertSource) untilRenewal() time.Duration {
	at := time.Unix(0, s.issued.Load()).Add(s.ttl / 4 * 3)
	return max(time.Until(at), 0)
}
