// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/ironcore-dev/feos/go/internal/apierr"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
)

// The codes of the environments' error answers {"error", "code", …},
// which clients (FeCode) match.
const (
	// CodeInvalidSpec: 422, the spec has error findings (InvalidSpecError).
	CodeInvalidSpec = "invalid_spec"
	// CodeBaseMismatch: 409, the request's base is not the current release
	// (BaseMismatchError).
	CodeBaseMismatch = "base_mismatch"
	// CodeNameTaken: 409, the tenant has an environment of that name with
	// another ID.
	CodeNameTaken = "name_taken"
	// CodeIDTaken: 409, an environment of that ID exists with another name
	// or another owner.
	CodeIDTaken = "id_taken"
)

// The texts of the environments' plain answers.
const (
	textNoEnvironment = "no such environment"
	textNoSecrets     = "secrets need --secret-key-file"
)

// InvalidSpecError refuses a spec with error findings: 422
// {"error", "code": "invalid_spec", "findings"}, every finding (warnings
// and infos too).
type InvalidSpecError struct {
	Findings []spec.Finding
}

func (e *InvalidSpecError) Error() string {
	n := 0
	first := ""
	for _, f := range e.Findings {
		if f.Severity == spec.SeverityError {
			if n == 0 {
				first = f.Path + ": " + f.Problem
				if f.Path == "" {
					first = f.Problem
				}
			}
			n++
		}
	}
	if n == 1 {
		return "the spec has an error: " + first
	}
	return fmt.Sprintf("the spec has %d errors, the first: %s", n, first)
}

// Status is the answer's HTTP status.
func (e *InvalidSpecError) Status() int { return http.StatusUnprocessableEntity }

// Body is the answer's body.
func (e *InvalidSpecError) Body() any {
	return struct {
		Error    string         `json:"error"`
		Code     string         `json:"code"`
		Findings []spec.Finding `json:"findings"`
	}{e.Error(), CodeInvalidSpec, nonNilFindings(e.Findings)}
}

// BaseMismatchError refuses a change made on another release than the
// environment's current one: 409 {"error", "code": "base_mismatch",
// "current"}. The client plans again on Current.
type BaseMismatchError struct {
	Base, Current int
}

func (e *BaseMismatchError) Error() string {
	return fmt.Sprintf("base %d is not the current release %d: plan again on release %d", e.Base, e.Current, e.Current)
}

// Status is the answer's HTTP status.
func (e *BaseMismatchError) Status() int { return http.StatusConflict }

// Body is the answer's body.
func (e *BaseMismatchError) Body() any {
	return struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Current int    `json:"current"`
	}{e.Error(), CodeBaseMismatch, e.Current}
}

// bodyError is an error with an answer of its own beyond {"error",
// "code"}.
type bodyError interface {
	error
	Status() int
	Body() any
}

// WriteError answers err as the environments' routes do: an
// InvalidSpecError or a BaseMismatchError with its own body, anything
// else as the API does (apierr.Write: an *apierr.Error with its status,
// any other error a 500).
func WriteError(w http.ResponseWriter, err error) {
	var be bodyError
	if errors.As(err, &be) {
		apierr.WriteJSON(w, be.Status(), be.Body())
		return
	}
	apierr.Write(w, err)
}

// errNotImplemented is the answer of an operation that a later lane
// implements: 501.
func errNotImplemented(what string) error {
	return apierr.New(http.StatusNotImplemented, "environments: %s is not implemented yet", what)
}

// errNoEnvironment is the answer for an environment the tenant does not
// have (absent or foreign alike).
func errNoEnvironment() error { return apierr.New(http.StatusNotFound, textNoEnvironment) }

// errNoSecrets is the answer of the secrets routes without a sealer.
func errNoSecrets() error { return apierr.New(http.StatusServiceUnavailable, textNoSecrets) }

func nonNilFindings(fs []spec.Finding) []spec.Finding {
	if fs == nil {
		return []spec.Finding{}
	}
	return fs
}
