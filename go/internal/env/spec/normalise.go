// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

// Pins are what Normalise pins a spec to, resolved by the caller outside
// any transaction.
type Pins struct {
	// Images maps an image as written to its digest reference
	// ("<registry>/<repo>@sha256:…"); an image missing here is kept as
	// written (a plan that could not resolve it warned).
	Images map[string]string
	// Secrets maps a secret's name to the version a reference without a
	// version is pinned to: its latest.
	Secrets map[string]int
}

// Normalise returns the spec's normalised form, the one ferund stores,
// hashes and diffs: defaults filled (replicas, resources, readiness
// timing, rollout and job timeouts, job on and retries), Runtime explicit
// on every service, durations as Go's time.Duration.String writes them
// ("15m0s"), Image pinned (and ImageAsWritten kept) for every image found
// in p.Images or already a digest, every referenced secret in Secrets
// with a version (p.Secrets for those without), unreferenced Secrets
// entries dropped (an info finding each). Fields that do not apply are
// cleared (an attached service has no replicas). s must have passed
// Validate without errors.
func Normalise(s Spec, p Pins) (Spec, []Finding) {
	return s, []Finding{notImplemented("normalisation")}
}
