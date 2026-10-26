// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ironcore-dev/feos/go/internal/apierr"
	"github.com/ironcore-dev/feos/go/internal/auth"
	"github.com/ironcore-dev/feos/go/internal/podid"
)

// The stable error codes of both surfaces, sent as "code" next to
// "error", so that a client tells "fix the binding" from "the server is
// down" without parsing prose.
const (
	codeInvalidBinding    = "invalid_binding"
	codeInsecureServer    = "insecure_server"
	codeBadCA             = "bad_ca"
	codeBadClientCert     = "bad_client_cert"
	codeDestinationDenied = "destination_denied"
	codeUnreachable       = "unreachable"
	codeTLSFailed         = "tls_failed"
	codeUnauthorized      = "unauthorized"
	codeBadKey            = "bad_key"
	codeBadHostKey        = "bad_host_key"
	codeHostKeyMismatch   = "host_key_mismatch"
	codeNoBinding         = "no_binding"
)

// maxRequestBody bounds a request body, as every REST body.
const maxRequestBody = 32 << 20

// bindingErr is a validation, preflight or check failure with its stable
// code. The HTTP status is the caller's choice: a PUT answers 400, a
// preflight 422 for everything but a malformed request.
type bindingErr struct {
	code string
	msg  string
}

func (e *bindingErr) Error() string { return e.msg }

func bindingErrf(code, format string, args ...any) *bindingErr {
	return &bindingErr{code: code, msg: fmt.Sprintf(format, args...)}
}

// as turns the failure into the API's error with status.
func (e *bindingErr) as(status int) *apierr.Error {
	return apierr.WithCode(status, e.code, "%s", e.msg)
}

// errNoSandbox is a sandbox that does not exist (any more).
var errNoSandbox = errors.New("no such sandbox")

func noSuchSandbox() *apierr.Error { return apierr.New(http.StatusNotFound, "no such sandbox") }

// decodeJSON reads a JSON object body into v and reports which keys the
// caller sent: "bindings" absent is not "bindings" empty. An empty body
// is an empty object.
func decodeJSON(r *http.Request, v any) (map[string]json.RawMessage, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		return nil, fmt.Errorf("read body: %v", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		raw = []byte("{}")
	}
	sent := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &sent); err != nil {
		return nil, fmt.Errorf("request body must be a JSON object: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, fmt.Errorf("invalid request body: %v", err)
	}
	return sent, nil
}

// sandboxOf returns the sandbox a sandbox-class request was made as; api
// has checked that its token names the path's sandbox.
func sandboxOf(r *http.Request) (podid.ID, *apierr.Error) {
	sb, ok := auth.SandboxFrom(r.Context())
	if !ok || sb.ID.IsZero() {
		return "", apierr.New(http.StatusUnauthorized, "sandbox token required")
	}
	return sb.ID, nil
}
