// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/store"
)

// The states of a domain (domains.state, the answers' state). The first
// three are its verification's when it is not verified. Certificates,
// and with them served, dns-moved and failed (which the schema allows),
// come in a later step with the ingress.
const (
	StateUnverified    = store.VerificationUnverified
	StateLapsed        = store.VerificationLapsed
	StateSuperseded    = store.VerificationSuperseded
	StateUnbound       = "unbound"
	StateUnpublished   = "unpublished"
	StateWaitingForDNS = "waiting-for-dns"
	StateIssuing       = "issuing"
	StateInactive      = "inactive"
)

// The reasons of the states that wait for something.
const (
	ReasonUnverified     = "waiting for the TXT record with the claim's token"
	ReasonLapsed         = "the TXT record with the claim's token was missing for longer than the grace; put it back to verify the claim again"
	ReasonSuperseded     = "another claim's token took the name; this claim is no longer checked"
	ReasonUnbound        = "not bound to an environment"
	ReasonNotPublic      = "the environment is not public"
	ReasonEnvDeleting    = "the environment is being deleted"
	ReasonInactive       = "the name resolved elsewhere than the edge for longer than the grace; route it to the edge again"
	ReasonWaitingForDNS  = "waiting for the name to resolve to the edge: a CNAME to the edge's name, or A and AAAA records of its addresses"
	ReasonNoCertificates = "certificates are not available yet: the ingress serves custom domains in a later step"
)

// Facts are what a domain's state is derived from: its row, its
// environment, the platform's grace and the database's time.
type Facts struct {
	Verification string // store.Verification*
	Bound        bool
	// EnvPublic and EnvDeleting are the bound environment's.
	EnvPublic, EnvDeleting bool
	// DNSMovedSince is the routing grace's start; zero: none runs.
	DNSMovedSince time.Time
	// Grace is --domain-grace.
	Grace time.Duration
	// Now is the database's time.
	Now    time.Time
	Routed bool
}

// FactsOf are the facts of a domain's view, with the platform's grace.
func FactsOf(v store.DomainView, grace time.Duration) Facts {
	return Facts{Verification: v.Verification, Bound: v.EnvID != uuid.Nil, EnvPublic: v.EnvPublic,
		EnvDeleting: v.EnvDeleting, DNSMovedSince: v.DNSMovedSince, Grace: grace, Now: v.Now, Routed: v.Routed}
}

// Derive is a domain's state and reason, the first rule that matches:
//
//  1. not verified: its verification (unverified, lapsed, superseded);
//  2. unbound;
//  3. its environment is not public, or is being deleted: unpublished;
//  4. its name has resolved elsewhere for the grace: inactive;
//  5. not routed: waiting-for-dns (also while a routing grace runs);
//  6. issuing, which waits for certificates, a later step.
func Derive(f Facts) (state, reason string) {
	switch {
	case f.Verification == store.VerificationUnverified:
		return StateUnverified, ReasonUnverified
	case f.Verification == store.VerificationLapsed:
		return StateLapsed, ReasonLapsed
	case f.Verification != store.VerificationVerified:
		return StateSuperseded, ReasonSuperseded
	case !f.Bound:
		return StateUnbound, ReasonUnbound
	case f.EnvDeleting:
		return StateUnpublished, ReasonEnvDeleting
	case !f.EnvPublic:
		return StateUnpublished, ReasonNotPublic
	case !f.DNSMovedSince.IsZero() && !f.DNSMovedSince.Add(f.Grace).After(f.Now):
		return StateInactive, ReasonInactive
	case !f.Routed:
		return StateWaitingForDNS, ReasonWaitingForDNS
	}
	return StateIssuing, ReasonNoCertificates
}

// deactivated reports whether a domain in state was deactivated by a
// grace that ran out.
func deactivated(state string) bool { return state == StateLapsed || state == StateInactive }
