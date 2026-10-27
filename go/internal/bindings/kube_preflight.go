// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The preflight dials from this instance, through the destination rule's
// dialer: the address actually dialled is judged, not only the name at
// validation.

type kubePreflightUser struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
}

type kubePreflightResult struct {
	ServerVersion string `json:"serverVersion"`
	// User is nil when the server does not answer the review (an older
	// cluster, missing RBAC): the credential works, its identity is
	// unknown.
	User *kubePreflightUser `json:"user"`
}

// kubeClient builds the one-shot client for a binding: TLS per the binding
// (its CA or the system roots, its server name, its client certificate),
// HTTP/1.1, no environment proxy, no redirects, IPv4 only, and a dialer
// that applies the destination rule to every address it connects to.
func (s *Service) kubeClient(b kubeBinding, host string) (*http.Client, *bindingErr) {
	serverName := b.ServerName
	if serverName == "" {
		serverName = host
	}
	conf := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
		NextProtos: []string{"http/1.1"},
	}
	if b.CAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(b.CAPEM)) {
			return nil, bindingErrf(codeBadCA, "caPem holds no certificate")
		}
		conf.RootCAs = pool
	}
	if b.Auth.Type == kubeAuthClientCert {
		cert, err := tls.X509KeyPair([]byte(b.Auth.CertPEM), []byte(b.Auth.KeyPEM))
		if err != nil {
			return nil, bindingErrf(codeBadClientCert, "client certificate: %v", err)
		}
		conf.Certificates = []tls.Certificate{cert}
	}
	dialer := s.dest.dialer()
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp4", addr)
			},
			TLSClientConfig:     conf,
			TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConns:        1,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// kubePreflight asks the cluster who the credential is: GET /version, then
// a SelfSubjectReview. b has passed validation and the destination rule.
func (s *Service) kubePreflight(ctx context.Context, b kubeBinding) (*kubePreflightResult, *bindingErr) {
	u, err := url.Parse(b.Server)
	if err != nil {
		return nil, bindingErrf(codeInvalidBinding, "server %q is not a URL: %v", b.Server, err)
	}
	client, kerr := s.kubeClient(b, u.Hostname())
	if kerr != nil {
		return nil, kerr
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()
	base := strings.TrimSuffix(b.Server, "/")

	status, body, err := kubeCall(ctx, client, b, http.MethodGet, base+"/version", nil)
	if err != nil {
		return nil, classifyKubeErr(b.Server, err)
	}
	res := &kubePreflightResult{}
	switch {
	case status == http.StatusUnauthorized:
		return nil, bindingErrf(codeUnauthorized, "%s rejected the credential (HTTP 401 on /version)", b.Server)
	case status == http.StatusForbidden:
		// Authenticated but not allowed to read the version: unusual, not
		// fatal; the review below still says who it is.
	case status/100 == 2:
		var v struct {
			GitVersion string `json:"gitVersion"`
		}
		if json.Unmarshal(body, &v) != nil || v.GitVersion == "" {
			return nil, bindingErrf(codeUnreachable,
				"%s answered /version without a Kubernetes version — is it a Kubernetes API server?", b.Server)
		}
		res.ServerVersion = v.GitVersion
	default:
		return nil, bindingErrf(codeUnreachable,
			"%s answered GET /version with HTTP %d — is it a Kubernetes API server?", b.Server, status)
	}

	review := []byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`)
	status, body, err = kubeCall(ctx, client, b, http.MethodPost,
		base+"/apis/authentication.k8s.io/v1/selfsubjectreviews", review)
	if err != nil {
		return nil, classifyKubeErr(b.Server, err)
	}
	switch {
	case status == http.StatusUnauthorized:
		return nil, bindingErrf(codeUnauthorized, "%s rejected the credential (HTTP 401 on the SelfSubjectReview)", b.Server)
	case status/100 == 2:
		var rv struct {
			Status struct {
				UserInfo struct {
					Username string   `json:"username"`
					Groups   []string `json:"groups"`
				} `json:"userInfo"`
			} `json:"status"`
		}
		if json.Unmarshal(body, &rv) == nil && rv.Status.UserInfo.Username != "" {
			groups := rv.Status.UserInfo.Groups
			if groups == nil {
				groups = []string{}
			}
			res.User = &kubePreflightUser{Username: rv.Status.UserInfo.Username, Groups: groups}
		}
	default:
		// 403, 404 and anything else: the credential works, the identity
		// is unknown.
	}
	return res, nil
}

// kubeCall makes one request with the binding's credential and reads at
// most 1 MiB of the answer.
func kubeCall(ctx context.Context, client *http.Client, b kubeBinding, method, target string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ferund-kube-preflight")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.Auth.Type == kubeAuthToken {
		req.Header.Set("Authorization", "Bearer "+b.Auth.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

// classifyKubeErr turns a transport error into the codes: the destination
// rule at dial time, a TLS failure, or "could not reach it" for everything
// else (refused, timed out, reset).
func classifyKubeErr(server string, err error) *bindingErr {
	var denied *errDialDenied
	if errors.As(err, &denied) {
		return bindingErrf(codeDestinationDenied, "%s: %v", server, denied.err)
	}
	if isTLSError(err) {
		return bindingErrf(codeTLSFailed, "%s: TLS: %v", server, unwrapURLError(err))
	}
	return bindingErrf(codeUnreachable, "%s: %v", server, unwrapURLError(err))
}

func isTLSError(err error) bool {
	var (
		verr  *tls.CertificateVerificationError
		uerr  x509.UnknownAuthorityError
		herr  x509.HostnameError
		ierr  x509.CertificateInvalidError
		rerr  tls.RecordHeaderError
		alert tls.AlertError
	)
	switch {
	case errors.As(err, &verr), errors.As(err, &uerr), errors.As(err, &herr),
		errors.As(err, &ierr), errors.As(err, &rerr), errors.As(err, &alert):
		return true
	}
	// A server's alert ("remote error: tls: certificate required") is an
	// unexported type; its text is the only handle on it.
	return strings.Contains(err.Error(), "tls: ")
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
