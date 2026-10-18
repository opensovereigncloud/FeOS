// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"net/http"
)

// The operations of the environments' REST routes (the contract's §5),
// one method each. tenant is the request's tenant (auth.Tenant.Name); env
// is the path's {env}: the environment's UUID or its name. An environment
// the tenant does not have, foreign or absent, is 404 "no such
// environment". Errors are *apierr.Error with their statuses, or an
// InvalidSpecError (422) or a BaseMismatchError (409), which WriteError
// answers; any other error is a 500.

// CreateEnv is POST /api/v1/environments: the new environment's status
// and created true (201), or the existing one's and false when the same
// ID and name exist (200); 409 CodeNameTaken or CodeIDTaken. A deleting
// tenant is refused.
func (s *Service) CreateEnv(ctx context.Context, tenant string, req CreateEnvRequest) (EnvStatus, bool, error) {
	return EnvStatus{}, false, errNotImplemented("creating an environment")
}

// ListEnvs is GET /api/v1/environments: the tenant's environments with
// every label of labels (?label=k=v, repeatable, AND), by name.
func (s *Service) ListEnvs(ctx context.Context, tenant string, labels map[string]string) (EnvList, error) {
	return EnvList{}, errNotImplemented("listing environments")
}

// GetEnv is GET /api/v1/environments/{env}: its status (§4.9), a read
// that wakes nothing.
func (s *Service) GetEnv(ctx context.Context, tenant, env string) (EnvStatus, error) {
	return EnvStatus{}, errNotImplemented("reading an environment")
}

// DeleteEnv is DELETE /api/v1/environments/{env}: marks it deleting
// (202); the controller deletes its instances, volumes and groups, then
// the environment.
func (s *Service) DeleteEnv(ctx context.Context, tenant, env string) error {
	return errNotImplemented("deleting an environment")
}

// PlanSpec is POST /api/v1/environments/plan: a spec validated without an
// environment (spec.ModeSpec).
func (s *Service) PlanSpec(ctx context.Context, tenant string, req PlanSpecRequest) (PlanSpecAnswer, error) {
	return PlanSpecAnswer{}, errNotImplemented("planning a spec")
}

// Plan is POST /api/v1/environments/{env}/plan: parse, validate, resolve
// the images (unresolvable: a warning), normalise, and diff against
// req.Base or the current release. It changes nothing.
func (s *Service) Plan(ctx context.Context, tenant, env string, req PlanRequest) (PlanAnswer, error) {
	return PlanAnswer{}, errNotImplemented("planning a release")
}

// Apply is POST /api/v1/environments/{env}/releases: plan, refuse on any
// error (InvalidSpecError), pin the images (unresolvable: an error), and
// in one transaction check req.Base against the current release
// (BaseMismatchError), insert release current+1 pending, supersede the
// older pending and rolling ones and wake the controller. A normalised
// spec equal to the current release's answers that release Unchanged
// (200), unless it failed.
func (s *Service) Apply(ctx context.Context, tenant, env string, req ApplyRequest) (ApplyAnswer, error) {
	return ApplyAnswer{}, errNotImplemented("applying a release")
}

// Releases is GET /api/v1/environments/{env}/releases: the newest limit
// releases (default 50), newest first.
func (s *Service) Releases(ctx context.Context, tenant, env string, limit int) (ReleaseList, error) {
	return ReleaseList{}, errNotImplemented("listing releases")
}

// Release is GET /api/v1/environments/{env}/releases/{n}: the release,
// its spec and its diff against release n-1; 404 "no such release".
func (s *Service) Release(ctx context.Context, tenant, env string, number int) (ReleaseDetail, error) {
	return ReleaseDetail{}, errNotImplemented("reading a release")
}

// Revert is POST /api/v1/environments/{env}/releases/{n}/revert: Apply of
// release n's spec with origin revert, originRef n, and n's annotations
// plus "ferun.revertOf": n. A secret version n pins that was deleted is an
// error finding.
func (s *Service) Revert(ctx context.Context, tenant, env string, number int, req RevertRequest) (ApplyAnswer, error) {
	return ApplyAnswer{}, errNotImplemented("reverting to a release")
}

// PutObject is PUT /api/v1/environments/{env}/objects/{kind}/{name}: the
// current release's spec with the object set, applied (origin object) or,
// with DryRun, planned. kind is spec.KindService, KindJob, KindAddon,
// KindRoute or KindSecret.
func (s *Service) PutObject(ctx context.Context, tenant, env, kind, name string, req PutObjectRequest) (ObjectAnswer, error) {
	return ObjectAnswer{}, errNotImplemented("putting an object")
}

// DeleteObject is DELETE /api/v1/environments/{env}/objects/{kind}/{name}:
// the current release's spec without the object, applied or planned.
func (s *Service) DeleteObject(ctx context.Context, tenant, env, kind, name string, req DeleteObjectRequest) (ObjectAnswer, error) {
	return ObjectAnswer{}, errNotImplemented("deleting an object")
}

// Clone is POST /api/v1/environments/{env}/clone: a new environment
// (201, its status) with the secrets of req.Secrets, whose release 1
// (origin clone, originRef "<env id>/<number>", annotations copied) is
// req.Spec or the source release's spec without its attached services.
// No data is copied.
func (s *Service) Clone(ctx context.Context, tenant, env string, req CloneRequest) (EnvStatus, error) {
	return EnvStatus{}, errNotImplemented("cloning an environment")
}

// Tags is GET /api/v1/environments/{env}/tags.
func (s *Service) Tags(ctx context.Context, tenant, env string) (TagList, error) {
	return TagList{}, errNotImplemented("listing tags")
}

// Tag is GET /api/v1/environments/{env}/tags/{tag}; 404 "no such tag".
func (s *Service) Tag(ctx context.Context, tenant, env, tag string) (Tag, error) {
	return Tag{}, errNotImplemented("reading a tag")
}

// PutTag is PUT /api/v1/environments/{env}/tags/{tag}: set or move the
// tag to a release; 404 "no such release".
func (s *Service) PutTag(ctx context.Context, tenant, env, tag string, req PutTagRequest) (Tag, error) {
	return Tag{}, errNotImplemented("moving a tag")
}

// DeleteTag is DELETE /api/v1/environments/{env}/tags/{tag} (204).
func (s *Service) DeleteTag(ctx context.Context, tenant, env, tag string) error {
	return errNotImplemented("deleting a tag")
}

// Secrets is GET /api/v1/environments/{env}/secrets; 503 without a
// sealer.
func (s *Service) Secrets(ctx context.Context, tenant, env string) (SecretList, error) {
	return SecretList{}, errNotImplemented("listing secrets")
}

// PutSecret is PUT /api/v1/environments/{env}/secrets/{name}: a new
// version, sealed (200); 503 without a sealer.
func (s *Service) PutSecret(ctx context.Context, tenant, env, name string, req PutSecretRequest) (SecretVersion, error) {
	return SecretVersion{}, errNotImplemented("setting a secret")
}

// DeleteSecret is DELETE /api/v1/environments/{env}/secrets/{name}: every
// version soft-deleted (204); 409 while the latest release pins it.
func (s *Service) DeleteSecret(ctx context.Context, tenant, env, name string) error {
	return errNotImplemented("deleting a secret")
}

// Incidents is GET /api/v1/environments/{env}/incidents: newest first,
// only the open ones with openOnly.
func (s *Service) Incidents(ctx context.Context, tenant, env string, openOnly bool) (IncidentList, error) {
	return IncidentList{}, errNotImplemented("listing incidents")
}

// Incident is GET /api/v1/environments/{env}/incidents/{id}; 404 "no
// such incident".
func (s *Service) Incident(ctx context.Context, tenant, env string, id int64) (Incident, error) {
	return Incident{}, errNotImplemented("reading an incident")
}

// Logs is GET /api/v1/environments/{env}/objects/{kind}/{name}/logs: the
// last lines lines (default 200) of replica replica's log
// (ctl.Controller.LogTail); 501 when the node serves no container logs,
// 409 "paused" for a paused instance (never woken).
func (s *Service) Logs(ctx context.Context, tenant, env, kind, name string, lines, replica int) (LogsAnswer, error) {
	return LogsAnswer{}, errNotImplemented("reading logs")
}

// RestartService is POST /api/v1/environments/{env}/services/{name}/restart
// (202).
func (s *Service) RestartService(ctx context.Context, tenant, env, name string) error {
	return errNotImplemented("restarting a service")
}

// RunJob is POST /api/v1/environments/{env}/jobs/{name}/run (202).
func (s *Service) RunJob(ctx context.Context, tenant, env, name string) error {
	return errNotImplemented("running a job")
}

// CreateRouteToken is POST /api/v1/environments/{env}/route-tokens: a new
// token of the route door (32 random bytes, base64url), shown once (201).
func (s *Service) CreateRouteToken(ctx context.Context, tenant, env string) (RouteToken, error) {
	return RouteToken{}, errNotImplemented("minting a route token")
}

// RevokeRouteTokens is DELETE /api/v1/environments/{env}/route-tokens:
// every route token of the environment revoked (204).
func (s *Service) RevokeRouteTokens(ctx context.Context, tenant, env string) error {
	return errNotImplemented("revoking route tokens")
}

// ServeRoute is the route door, ANY
// /api/v1/environments/{env}/http/{path...}, an auth class of its own: a
// route token of the environment as Bearer (401 "route token required";
// a tenant token is refused), the longest segment-aware prefix match over
// the routes of the realised release (the latest when none is), and the
// request forwarded to a ready instance of the route's service (a paused
// one is woken; none: 503 with Retry-After 5) or to an attached service's
// sandbox. Its answers are plain text.
func (s *Service) ServeRoute(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "the route door is not implemented yet", http.StatusNotImplemented)
}
