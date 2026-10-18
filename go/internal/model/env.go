// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

// The values of the environments' tables (migration 0006, ADR 0018),
// which the environment service, its controller and the API share, and
// which FeCode reads in the REST answers.

// The states of a release (env_releases.state).
const (
	// ReleasePending: applied, its rollout not begun.
	ReleasePending = "pending"
	// ReleaseRolling: the controller brings the environment to it.
	ReleaseRolling = "rolling"
	// ReleaseRealised: every object matched it and was ready.
	ReleaseRealised = "realised"
	// ReleaseFailed: its rollout failed (env_releases.reason); the
	// environment keeps serving its realised release.
	ReleaseFailed = "failed"
	// ReleaseSuperseded: a later release took its place before or after
	// it was realised.
	ReleaseSuperseded = "superseded"
)

// What made a release (env_releases.origin).
const (
	OriginApply  = "apply"
	OriginRevert = "revert"
	OriginClone  = "clone"
	OriginObject = "object"
)

// The phases of an instance (env_instances.phase) and of an attached
// service (env_attached.phase, which has neither succeeded nor
// retiring).
const (
	PhaseStarting  = "starting"
	PhaseReady     = "ready"
	PhaseUnready   = "unready"
	PhaseExited    = "exited"
	PhaseSucceeded = "succeeded"
	PhaseFailed    = "failed"
	PhaseRetiring  = "retiring"
)

// The phases of a job's run (env_job_runs.phase).
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
)

// The conditions of an incident (env_incidents.condition).
const (
	ConditionRolloutFailed     = "rollout_failed"
	ConditionCrashLoop         = "crash_loop"
	ConditionJobFailed         = "job_failed"
	ConditionNoCapacity        = "no_capacity"
	ConditionAddonUnhealthy    = "addon_unhealthy"
	ConditionVolumeUnavailable = "volume_unavailable"
	ConditionAttachFailed      = "attach_failed"
)

// The states of an environment in the list (GET /api/v1/environments)
// besides its latest release's state.
const (
	// EnvNone: the environment has no release yet.
	EnvNone = "none"
	// EnvDeleting: the environment is being deleted.
	EnvDeleting = "deleting"
)
