// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

// Diff lists the changes from prev to next, both normalised, one entry
// per changed object, ordered by kind (services, jobs, addons, routes,
// secrets) and name; nil prev is an environment without a release (every
// object an add), nil next one whose objects all go. An update lists its
// changed fields and the effect on the running pods: "in place" (routes,
// idle, a job's on), "restarts", EffectReplaces(n) (a service's template:
// a rolling replacement of its n replicas), "recreates (downtime)" (an
// addon's version or plan), "deletes data" (an addon removed),
// "reprovisions" (an addon's provisioning options). Secret values never
// appear; versions do.
func Diff(prev, next *Spec) []DiffEntry {
	return nil
}
