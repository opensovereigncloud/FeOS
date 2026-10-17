// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package e2e holds ferund's end-to-end tests (§17 of the design): several
// instances built by internal/server run in the test's process on one
// PostgreSQL database, test nodes (internal/testnode) join them over the
// wire, and clients reach them over real HTTP as FeCode and the SPA do:
// REST, MCP with the go-sdk client, the port door and the watches. Where a
// scenario needs what no client sends (a Join repeated with the same key,
// a node connection with a superseded certificate, a crafted event batch),
// the tests speak feos.fleet.v1 themselves.
//
// The package has no code besides its tests. The tests need a PostgreSQL
// (FERUN_TEST_DATABASE_URL, as every database test of the module) and skip
// without one.
package e2e
