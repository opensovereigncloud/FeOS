// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// status assembles an environment's status (§4.9) from the tables: its
// latest release's rollout; the objects of its target release (the
// latest, or the realised one while the latest failed) with their
// instances (env_instances with their sandbox rows: state, node,
// address), attached services (env_attached) and jobs' latest runs
// (env_job_runs); the routes of its realised release (the latest when
// none is realised); and its open incidents. It reads only.
func (s *Service) status(ctx context.Context, e store.Environment) (EnvStatus, error) {
	st := EnvStatus{ID: e.ID.String(), Name: e.Name, Labels: e.Labels, Release: e.Release, Realised: e.Realised,
		Deleting: e.Deleting, Objects: []ObjectStatus{}, Routes: []RouteStatus{}, CreatedAt: rfc3339(e.CreatedAt)}
	if st.Labels == nil {
		st.Labels = map[string]string{}
	}
	open, err := s.st.CountOpenIncidents(ctx, e.ID)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("incidents: %w", err)
	}
	st.OpenIncidents = int(open)
	if e.Release == 0 {
		return st, nil
	}
	latest, err := s.st.GetRelease(ctx, e.ID, e.Release)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("release %d: %w", e.Release, err)
	}
	st.Rollout = &RolloutStatus{Release: latest.Number, State: latest.State, StartedAt: rfc3339(latest.StartedAt),
		Reason: latest.Reason}
	target := e.Release
	if latest.State == model.ReleaseFailed && e.Realised > 0 {
		target = e.Realised
	}
	targetSpec, err := s.releaseSpec(ctx, e.ID, target)
	if err != nil {
		return EnvStatus{}, err
	}
	routed := e.Release
	if e.Realised > 0 {
		routed = e.Realised
	}
	routeSpec, err := s.releaseSpec(ctx, e.ID, routed)
	if err != nil {
		return EnvStatus{}, err
	}
	instances, err := s.st.ListEnvInstanceRows(ctx, e.ID)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("instances: %w", err)
	}
	attached, err := s.st.ListAttached(ctx, e.ID)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("attached services: %w", err)
	}
	runs, err := s.st.LatestJobRuns(ctx, e.ID)
	if err != nil {
		return EnvStatus{}, fmt.Errorf("job runs: %w", err)
	}
	st.Objects = objectsOf(targetSpec, instances, attached, runs)
	st.Routes = routesOf(routeSpec)
	return st, nil
}

// objectsOf are the objects of a target spec: services, addons and jobs,
// each by name.
func objectsOf(sp *spec.Spec, instances []store.EnvInstanceRow, attached []store.EnvAttached,
	runs []store.EnvJobRun) []ObjectStatus {
	byObject := map[string][]store.EnvInstanceRow{}
	for _, row := range instances {
		k := row.Instance.Kind + "/" + row.Instance.Object
		byObject[k] = append(byObject[k], row)
	}
	attachedOf := map[string]store.EnvAttached{}
	for _, a := range attached {
		attachedOf[a.Service] = a
	}
	runOf := map[string]store.EnvJobRun{}
	for _, r := range runs {
		runOf[r.Job] = r
	}
	out := []ObjectStatus{}
	for _, name := range slices.Sorted(maps.Keys(sp.Services)) {
		svc := sp.Services[name]
		if svc.Runtime == spec.RuntimeAttached {
			o := ObjectStatus{Kind: spec.KindService, Name: name, Runtime: spec.RuntimeAttached, Sandbox: svc.Sandbox,
				Phase: model.PhaseStarting}
			if a, ok := attachedOf[name]; ok {
				o.Sandbox, o.Phase, o.Restarts, o.LastExit, o.Detail = a.SandboxID.String(), a.Phase, a.Restarts,
					a.LastExit, a.Detail
			}
			out = append(out, o)
			continue
		}
		o := ObjectStatus{Kind: spec.KindService, Name: name, Runtime: spec.RuntimeImage, Desired: spec.DefaultReplicas}
		if svc.Replicas != nil {
			o.Desired = *svc.Replicas
		}
		for _, row := range byObject[spec.KindService+"/"+name] {
			if row.Instance.Phase == model.PhaseReady && row.Sandbox.ClientState == model.StateRunning {
				o.Ready++
			}
			o.Instances = append(o.Instances, instanceStatus(row))
		}
		out = append(out, o)
	}
	for _, name := range slices.Sorted(maps.Keys(sp.Addons)) {
		a := sp.Addons[name]
		o := ObjectStatus{Kind: spec.KindAddon, Name: name, Type: a.Type, Version: a.Version, Plan: a.Plan,
			Phase: model.PhaseStarting}
		var current *store.EnvInstance
		for _, row := range byObject[spec.KindAddon+"/"+name] {
			o.Instances = append(o.Instances, instanceStatus(row))
			if row.Instance.Phase != model.PhaseRetiring {
				i := row.Instance
				current = &i
			}
		}
		if current != nil {
			o.Phase, o.Provisioned = current.Phase, current.ProvisionedHash != ""
		}
		out = append(out, o)
	}
	for _, name := range slices.Sorted(maps.Keys(sp.Jobs)) {
		o := ObjectStatus{Kind: spec.KindJob, Name: name}
		if r, ok := runOf[name]; ok {
			o.LastRun = &JobRunStatus{Release: r.Release, Attempt: r.Attempt, Phase: r.Phase, ExitCode: r.ExitCode,
				FinishedAt: rfc3339(r.FinishedAt), Detail: r.Detail}
		}
		out = append(out, o)
	}
	return out
}

// instanceStatus is an instance with its sandbox row.
func instanceStatus(row store.EnvInstanceRow) InstanceStatus {
	i, sb := row.Instance, row.Sandbox
	detail := i.Detail
	if detail == "" && sb.ClientState != model.StateRunning {
		detail = sb.StatusDescription
	}
	return InstanceStatus{ID: sb.ID.String(), Replica: i.Replica, Phase: i.Phase, State: sb.ClientState, Node: sb.Node,
		Address: sb.Address, Release: i.Release, Restarts: i.Restarts, LastExit: i.LastExit,
		ReadySince: rfc3339(i.ReadySince), Detail: detail}
}

// routesOf are a spec's routes by name, their ports resolved.
func routesOf(sp *spec.Spec) []RouteStatus {
	out := []RouteStatus{}
	for _, name := range slices.Sorted(maps.Keys(sp.Routes)) {
		r := sp.Routes[name]
		port, _ := spec.PortNumber(sp.Services[r.Service].Ports, r.Port)
		out = append(out, RouteStatus{Name: name, Path: r.Path, Service: r.Service, Port: port})
	}
	return out
}

// summaryOf is an environment as its tenant's listing shows it.
func summaryOf(r store.EnvSummaryRow) EnvSummary {
	state := r.ReleaseState
	switch {
	case r.Deleting:
		state = model.EnvDeleting
	case state == "":
		state = model.EnvNone
	}
	labels := r.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return EnvSummary{ID: r.ID.String(), Name: r.Name, Labels: labels, Release: r.Release, Realised: r.Realised,
		State: state, OpenIncidents: int(r.OpenIncidents), CreatedAt: rfc3339(r.CreatedAt)}
}

// incidentOf is an incident as the answers show it.
func incidentOf(i store.EnvIncident) Incident {
	var ev IncidentEvidence
	if len(i.Evidence) > 0 {
		_ = json.Unmarshal(i.Evidence, &ev)
	}
	return Incident{ID: fmt.Sprint(i.ID), Object: i.Object, Release: i.Release, Condition: i.Condition,
		OpenedAt: rfc3339(i.OpenedAt), ClosedAt: rfc3339(i.ClosedAt), Summary: i.Summary, Evidence: ev}
}
