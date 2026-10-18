// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

// Mode is what a validation is for: it decides which checks run and how
// hard an unresolvable image is.
type Mode string

const (
	// ModeSpec is POST /api/v1/environments/plan: no environment, so no
	// check that needs one (secrets, the current release); the tenant's
	// sandboxes are checked.
	ModeSpec Mode = "spec"
	// ModePlan is a plan of an environment: an image that cannot be
	// resolved is a warning.
	ModePlan Mode = "plan"
	// ModeApply is an apply (a release, a revert, an object put or
	// delete, a clone): an image that cannot be resolved is an error.
	ModeApply Mode = "apply"
)

// ValidateContext is what Validate looks up beyond the spec. It answers
// from memory: the caller reads what the parsed spec names (the
// sandboxes, the secrets, the images' resolution, which runs outside any
// transaction) before it validates, so that Validate makes no database,
// node or registry call and needs no context.
type ValidateContext interface {
	// Mode is what the validation is for.
	Mode() Mode
	// Sandbox reports the tenant's sandbox of id (a UUID string, as the
	// spec names it); ok false when the tenant has no sandbox of that ID,
	// or it is an environment's instance row.
	Sandbox(id string) (info SandboxInfo, ok bool)
	// Secret reports a tenant secret of the environment (never a system
	// secret "addon.…"); ok false when no version of it exists that is not
	// deleted. Not asked in ModeSpec.
	Secret(name string) (info SecretInfo, ok bool)
	// Blueprint returns the addon type typ; ok false when no blueprint of
	// that type is registered.
	Blueprint(typ string) (b Blueprint, ok bool)
	// Image returns the image pinned to its digest
	// ("<registry>/<repo>@sha256:…"), or why it could not be resolved
	// (err non-nil). An image that already is a digest reference resolves
	// to itself.
	Image(image string) (pinned string, err error)
}

// SandboxInfo is a sandbox an attached service may run in.
type SandboxInfo struct {
	// ID is the sandbox's UUID, in lowercase.
	ID string
	// Attachable: the sandbox was created attachable (its pod has the
	// sbx- security group). One that is not cannot reach the
	// environment's image pods.
	Attachable bool
	// AttachedPorts are the ports the attached services of other
	// environments use in it: two attached services in one sandbox must
	// not share a port number.
	AttachedPorts []AttachedPort
}

// AttachedPort is a port an attached service uses in a sandbox.
type AttachedPort struct {
	// Env is the environment's name, Service the service's.
	Env, Service string
	Port         int
}

// SecretInfo is a tenant secret of an environment.
type SecretInfo struct {
	// Latest is its newest version that is not deleted.
	Latest int
	// Versions are its versions that are not deleted, ascending.
	Versions []int
}

// Blueprint is what Validate asks of an addon type. The environment
// service adapts an internal/env/addons blueprint to it (addons imports
// this package for Finding, so this package cannot import addons).
type Blueprint interface {
	// Validate checks addon name of this type against the same addon of
	// the current release (prev nil: none, or another type): its version,
	// plan and options. Paths are relative to the addon: "version",
	// "options.roles[1]".
	Validate(name string, prev *Addon, next Addon) []Finding
	// Ref checks a reference {"addon": name, "key": key, …fields} to the
	// addon a: nil when it resolves, else why not ("no key x; the keys
	// are url, host, …").
	Ref(name string, a Addon, key string, fields map[string]string) error
}

// Validate checks a parsed spec against the rules of the contract's §3
// and returns every finding, ordered by path. prev is the environment's
// current release's spec (nil: none, and always nil in ModeSpec): an
// addon is validated against its previous self (a major version change
// is an error). Findings about the spec as written; Normalise adds its
// own (dropped secrets).
func Validate(vc ValidateContext, s Spec, prev *Spec) []Finding {
	return []Finding{notImplemented("validation")}
}

// notImplemented is the one finding of a function that a later lane
// implements.
func notImplemented(what string) Finding {
	return Finding{Severity: SeverityError, Path: "", Problem: what + " of release specs is not implemented yet",
		Remedy: "wait for a ferund that implements environments"}
}
