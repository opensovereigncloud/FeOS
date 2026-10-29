// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/store"
)

// Derive follows the contract's rules in their order: the first that
// matches decides.
func TestDerive(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	grace := 72 * time.Hour
	// routed is a verified domain, bound to a public environment, routed;
	// each case changes it.
	routed := Facts{Verification: store.VerificationVerified, Bound: true, EnvPublic: true, Grace: grace, Now: now,
		Routed: true}
	with := func(f func(*Facts)) Facts {
		c := routed
		f(&c)
		return c
	}
	for _, c := range []struct {
		name          string
		facts         Facts
		state, reason string
	}{
		{"routed: waits for certificates", routed, StateIssuing, ReasonNoCertificates},
		{"unverified", with(func(f *Facts) { f.Verification = store.VerificationUnverified }), StateUnverified, ReasonUnverified},
		{"lapsed", with(func(f *Facts) { f.Verification = store.VerificationLapsed }), StateLapsed, ReasonLapsed},
		{"superseded", with(func(f *Facts) { f.Verification = store.VerificationSuperseded; f.Bound = false }),
			StateSuperseded, ReasonSuperseded},
		{"unbound", with(func(f *Facts) { f.Bound = false }), StateUnbound, ReasonUnbound},
		{"unbound wins over an unpublished environment", with(func(f *Facts) { f.Bound = false; f.EnvPublic = false }),
			StateUnbound, ReasonUnbound},
		{"not public", with(func(f *Facts) { f.EnvPublic = false }), StateUnpublished, ReasonNotPublic},
		{"deleting", with(func(f *Facts) { f.EnvDeleting = true }), StateUnpublished, ReasonEnvDeleting},
		{"unpublished wins over inactive",
			with(func(f *Facts) { f.EnvPublic = false; f.DNSMovedSince = now.Add(-100 * time.Hour) }),
			StateUnpublished, ReasonNotPublic},
		{"dns moved within the grace", with(func(f *Facts) { f.DNSMovedSince = now.Add(-time.Hour); f.Routed = false }),
			StateWaitingForDNS, ReasonWaitingForDNS},
		{"dns moved at the grace's end",
			with(func(f *Facts) { f.DNSMovedSince = now.Add(-grace); f.Routed = false }), StateInactive, ReasonInactive},
		{"inactive wins over routed", with(func(f *Facts) { f.DNSMovedSince = now.Add(-grace - time.Second) }),
			StateInactive, ReasonInactive},
		{"not routed", with(func(f *Facts) { f.Routed = false }), StateWaitingForDNS, ReasonWaitingForDNS},
	} {
		state, reason := Derive(c.facts)
		if state != c.state || reason != c.reason {
			t.Errorf("%s: %s %q, want %s %q", c.name, state, reason, c.state, c.reason)
		}
	}
}
