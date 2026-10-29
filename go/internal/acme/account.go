// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"

	xacme "golang.org/x/crypto/acme"

	"github.com/gardenlinux/feos/go/internal/store"
)

// maxRetries bounds the retries of one request to the CA: a bad nonce,
// or the CA's 5xx. A rate limit (429) is not retried: the order fails
// and backs off.
const maxRetries = 3

// account returns the client of the platform's account at the
// directory: the account's key from acme_accounts, made and kept by the
// first instance that needs it, registered at the CA once, its contact
// kept up to date. Nil and nil while a failure before backs off.
func (s *Issuer) account(ctx context.Context) (*xacme.Client, error) {
	s.mu.Lock()
	cl, at := s.client, s.accountAt
	s.mu.Unlock()
	if cl != nil {
		return cl, nil
	}
	if time.Now().Before(at) {
		return nil, nil
	}
	cl, err := s.loadAccount(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil
		}
		s.accountAt = time.Now().Add(s.accountBk)
		wait := s.accountBk
		s.accountBk = min(2*s.accountBk, accountBackoffMax)
		return nil, fmt.Errorf("the ACME account at %s (again in %v): %w", s.cfg.Directory, wait, err)
	}
	s.client, s.accountBk = cl, accountBackoffMin
	return cl, nil
}

// loadAccount reads, or makes, the account's key and registers it.
func (s *Issuer) loadAccount(ctx context.Context) (*xacme.Client, error) {
	dir := s.cfg.Directory
	aad := store.ACMEAccountAAD(dir)
	a, err := s.d.Store.GetACMEAccount(ctx, dir)
	if errors.Is(err, store.ErrNoRow) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("make the account's key: %w", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			return nil, fmt.Errorf("make the account's key: %w", err)
		}
		sealed, err := s.d.Sealer.Seal(der, aad)
		if err != nil {
			return nil, fmt.Errorf("seal the account's key: %w", err)
		}
		// The directory's primary key is the lock: another instance's
		// key, inserted first, is the one returned.
		if a, err = s.d.Store.InsertACMEAccount(ctx, store.ACMEAccount{Directory: dir, KeySealed: sealed}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("read the account: %w", err)
	}
	der, err := s.d.Sealer.Open(a.KeySealed, aad)
	if err != nil {
		return nil, fmt.Errorf("open the account's key: %w", err)
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("the account's key: %w", err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("the account's key is a %T, not ECDSA P-256", k)
	}
	cl := &xacme.Client{Key: key, DirectoryURL: dir, HTTPClient: s.cfg.HTTPClient, RetryBackoff: retryBackoff,
		UserAgent: s.cfg.UserAgent}
	contact := contactOf(s.cfg.Email)
	uri := a.URI
	switch {
	case uri == "":
		acct, err := cl.Register(ctx, &xacme.Account{Contact: contact}, xacme.AcceptTOS)
		switch {
		case errors.Is(err, xacme.ErrAccountAlreadyExists):
			// Registered before, by a run that did not record it.
			uri = string(cl.KID)
		case err != nil:
			return nil, fmt.Errorf("register: %w", err)
		default:
			uri = acct.URI
		}
		if uri == "" {
			return nil, errors.New("register: the CA named no account URL")
		}
		cl.KID = xacme.KeyID(uri)
		if errors.Is(err, xacme.ErrAccountAlreadyExists) && a.Email != s.cfg.Email {
			if _, err := cl.UpdateReg(ctx, &xacme.Account{Contact: contact}); err != nil {
				return nil, fmt.Errorf("update the account's contact: %w", err)
			}
		}
		s.log.Info("acme: account registered", "directory", dir, "account", uri)
	case a.Email != s.cfg.Email:
		cl.KID = xacme.KeyID(uri)
		if _, err := cl.UpdateReg(ctx, &xacme.Account{Contact: contact}); err != nil {
			return nil, fmt.Errorf("update the account's contact: %w", err)
		}
		s.log.Info("acme: account contact updated", "directory", dir, "account", uri)
	default:
		cl.KID = xacme.KeyID(uri)
	}
	if uri != a.URI || a.Email != s.cfg.Email {
		if err := s.d.Store.SetACMEAccount(ctx, dir, uri, s.cfg.Email); err != nil {
			return nil, err
		}
	}
	return cl, nil
}

// contactOf is the account's contact for email.
func contactOf(email string) []string {
	if email == "" {
		return nil
	}
	return []string{"mailto:" + email}
}

// retryBackoff is the client's RetryBackoff: a request is tried again
// after 1, 2, 3 s (or the CA's Retry-After, up to 10 s), maxRetries
// times; a rate limit not at all.
func retryBackoff(n int, _ *http.Request, res *http.Response) time.Duration {
	if n > maxRetries || (res != nil && res.StatusCode == http.StatusTooManyRequests) {
		return -1
	}
	d := time.Duration(n) * time.Second
	if res != nil {
		if s := res.Header.Get("Retry-After"); s != "" {
			if t, err := time.ParseDuration(s + "s"); err == nil && t > d {
				d = min(t, 10*time.Second)
			}
		}
	}
	return d
}

// accountURI is the client's account URL (tests).
func (s *Issuer) accountURI() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return ""
	}
	return string(s.client.KID)
}
