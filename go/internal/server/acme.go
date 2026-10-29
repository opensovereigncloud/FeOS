// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"github.com/gardenlinux/feos/go/internal/acme"
	"github.com/gardenlinux/feos/go/internal/seal"
)

// newACME builds the ACME client of the domains' certificates (ADR 0021,
// internal/acme) with --acme-directory and --acme-email; it seals the
// keys with sealer, --secret-key-file's (nil: none, which config refuses
// with a directory). Its loop is the leader's.
func (i *Instance) newACME(sealer *seal.Sealer) (*acme.Issuer, error) {
	var sl acme.Sealer
	if sealer != nil {
		sl = sealer
	}
	return acme.New(acme.Config{Directory: i.cfg.ACMEDirectory, Email: i.cfg.ACMEEmail, Profile: i.cfg.ACMEProfile, UserAgent: "ferund/" + i.version,
		Log: i.log}, acme.Deps{Store: i.Store, Sealer: sl, Elector: i.Elector, Refresher: domainsRefresher(i)})
}

// domainsRefresher is what ACME calls after each order: the domains
// core's refresh of a domain's state and reason.
func domainsRefresher(i *Instance) acme.Refresher {
	if i.Domains == nil {
		return acme.NoRefresh{}
	}
	return i.Domains
}
