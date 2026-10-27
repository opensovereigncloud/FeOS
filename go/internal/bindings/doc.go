// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package bindings is ferund's Kubernetes and SSH bindings (ADR 0014): a
// sandbox's whole binding sets over REST, their validation with stable
// error codes, the destination rule, the preflights and the check that
// dial from this instance, the sets' storage in PostgreSQL with every
// credential sealed at rest (AES-256-GCM under --secret-key-file), the
// access route that reads the sets back with their use from the pods'
// audit records, and the source of a pod's bindings that
// IssuePodCredentials seals to the run's HPKE key.
//
// The surfaces are on only with --bindings and a key. Off, api does not
// register their routes (an unknown route answers 404 "no such route",
// which clients read as "no such surface on this platform") and the access
// route answers {"kube":null,"ssh":null,"denies":[]}. A set, preflight or
// check handler reached anyway answers that same 404. KubePorts reports
// the stored ports on or off: a pod keeps the listeners of the run it
// started with.
//
// Every PUT and DELETE runs in one transaction that holds the sandbox's
// row FOR UPDATE, which serialises them across instances: a PUT reads the
// stored set to keep generated keys and the same set twice is a no-op.
// Nothing is pushed to a node: a pod gets its bindings, sealed to its
// run's key, when its feos-authn asks for its credentials, so a PUT on a
// running sandbox reaches the pod at its next start, resume or restore.
package bindings
