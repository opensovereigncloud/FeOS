// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"cmp"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/routing"
)

// The proxy's plain answers.
const (
	textNoTable    = "the ingress has no routing table yet"
	textNoHost     = "no such host"
	textDotDot     = "the path has a .. segment"
	textDot        = "the path has a . segment"
	textBadEscape  = "the path is not escaped correctly"
	textNoAnswer   = "the service did not answer"
	textBadUpgrade = "the service's protocol switch failed"
	textTooMany    = "too many requests waiting"
	textDraining   = "the ingress is shutting down, try again"
	textConnect    = "CONNECT is not supported"
	textTrailers   = "request trailers are not supported"
)

// textTooManyFields answers a request with more than maxHeaderFields
// header fields.
var textTooManyFields = "the request has more than " + strconv.Itoa(maxHeaderFields) + " header fields"

// The answers of the limits: an environment's share of the held and the
// forwarded requests is taken, the instance's cap of forwarded requests
// is, or no connection to the instance came free in time.
const (
	textTooManyHere = "too many requests waiting for this host"
	textBusyHere    = "too many requests to this host, try again"
	textBusy        = "the ingress is busy, try again"
	textNoConn      = "the service is busy, try again"
)

// table is the routing the proxy serves, built from the agent's state by
// SetState and never changed afterwards.
type table struct {
	// src is the state the table was built from, to tell a change of
	// the routing from a heartbeat.
	src   *wire.State
	hosts map[string]*envRoutes
	// zone is the apps zone, lower case and without a trailing dot.
	zone string
	// admitted is the version of the group ferun-ingress at which this
	// ingress entered it (Self.AdmittedVersion).
	admitted uint64
	// addrs is the set of endpoint addresses, dials what the proxy
	// dials for them: an address and a port each ("[fd00::1]:8080"),
	// with what it reaches. routed holds envService of every routed
	// service. All three cover only environments that serve a host.
	addrs  map[string]bool
	dials  map[string]dialTarget
	routed map[string]bool
}

// dialTarget is what a dial address reaches: an endpoint (its address)
// of a service of an environment (its ID).
type dialTarget struct {
	env, service, address string
}

// envService is the key of a service of an environment in table.routed.
func envService(env, service string) string {
	return env + "/" + service
}

// envRoutes is one environment's routes: sorted by the number of their
// path's segments, longest first, then by name, with prefixes in the
// same order.
type envRoutes struct {
	id       string
	tenant   string
	routes   []wire.Route
	prefixes []string
	services map[string]*service
}

// service is one routed service: its endpoints in the table's order, the
// counts of its other instances, and the round robin's position.
type service struct {
	name      string
	idle      bool
	endpoints []endpoint
	asleep    int
	waking    int
	pending   int
	next      atomic.Uint64
}

// endpoint is one instance that answers (wire.Endpoint).
type endpoint struct {
	instance     string
	address      string
	node         string
	ports        map[string]int
	groupVersion uint64
}

// canonicalHost is a request's host as the table keys it: lower case,
// without a port and a trailing dot.
func canonicalHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// buildTable builds the routing of a state. Of two environments that
// claim one host (the control plane never sends that), the one whose ID
// sorts first serves it.
func buildTable(s *wire.State, log *slog.Logger) *table {
	t := &table{
		src:      s.Clone(),
		hosts:    map[string]*envRoutes{},
		zone:     canonicalHost(s.Self.AppsZone),
		admitted: s.Self.AdmittedVersion,
		addrs:    map[string]bool{},
		dials:    map[string]dialTarget{},
		routed:   map[string]bool{},
	}
	for _, id := range slices.Sorted(maps.Keys(s.Envs)) {
		e := s.Envs[id]
		er := buildEnv(e)
		served := false
		for _, h := range e.Hosts {
			h = canonicalHost(h)
			if h == "" {
				continue
			}
			if other, ok := t.hosts[h]; ok {
				log.Warn("two environments claim one host", "host", h, "env", id, "served", other.id)
				continue
			}
			t.hosts[h] = er
			served = true
		}
		if served {
			t.add(er)
		}
	}
	return t
}

// add records the endpoints and services of an environment that serves
// a host.
func (t *table) add(er *envRoutes) {
	for _, svc := range er.services {
		t.routed[envService(er.id, svc.name)] = true
		for _, ep := range svc.endpoints {
			t.addrs[ep.address] = true
			for _, port := range ep.ports {
				if port > 0 {
					d := net.JoinHostPort(ep.address, strconv.Itoa(port))
					t.dials[d] = dialTarget{env: er.id, service: svc.name, address: ep.address}
				}
			}
		}
	}
}

func buildEnv(e wire.Env) *envRoutes {
	er := &envRoutes{id: e.ID, tenant: e.Tenant, routes: slices.Clone(e.Routes), services: map[string]*service{}}
	segments := func(r wire.Route) int {
		p := strings.Trim(r.Path, "/")
		if p == "" {
			return 0
		}
		return strings.Count(p, "/") + 1
	}
	slices.SortStableFunc(er.routes, func(a, b wire.Route) int {
		return cmp.Or(cmp.Compare(segments(b), segments(a)), cmp.Compare(a.Name, b.Name))
	})
	for _, r := range er.routes {
		er.prefixes = append(er.prefixes, r.Path)
	}
	for _, s := range e.Services {
		svc := &service{name: s.Name, idle: s.Idle, asleep: s.Asleep, waking: s.Waking, pending: s.Pending}
		for _, ep := range s.Endpoints {
			if ep.Address == "" {
				continue
			}
			svc.endpoints = append(svc.endpoints, endpoint{
				instance:     ep.Instance,
				address:      ep.Address,
				node:         ep.Node,
				ports:        maps.Clone(ep.Ports),
				groupVersion: ep.GroupVersion,
			})
		}
		er.services[s.Name] = svc
	}
	return er
}

// target is where the routing sends a request: the environment, the
// route, its service and the path the upstream gets (unescaped, and
// escaped when that is not the default escaping).
type target struct {
	env     *envRoutes
	route   *wire.Route
	svc     *service
	path    string
	rawPath string
}

// resolve routes a request by its canonical host and its escaped path,
// or returns the answer when the routing does not send it anywhere.
func (t *table) resolve(host, raw string) (target, *reply) {
	if t == nil {
		return target{}, &reply{http.StatusServiceUnavailable, textNoTable, 5}
	}
	e := t.hosts[host]
	if e == nil {
		return target{}, &reply{http.StatusNotFound, textNoHost, 0}
	}
	switch routing.DotSegment(raw) {
	case "..":
		return target{}, &reply{http.StatusBadRequest, textDotDot, 0}
	case ".":
		return target{}, &reply{http.StatusBadRequest, textDot, 0}
	}
	path, err := url.PathUnescape(raw)
	if err != nil {
		return target{}, &reply{http.StatusBadRequest, textBadEscape, 0}
	}
	i, n, ok := routing.Match(e.prefixes, raw)
	if !ok {
		return target{}, &reply{http.StatusNotFound, "no route for " + path, 0}
	}
	r := &e.routes[i]
	if r.Door {
		return target{}, &reply{http.StatusNotFound, "route " + r.Path + " is served by the route door", 0}
	}
	svc := e.services[r.Service]
	if svc == nil {
		return target{}, &reply{http.StatusServiceUnavailable, "no ready instance of " + r.Service, 5}
	}
	tg := target{env: e, route: r, svc: svc, path: path, rawPath: routing.RawOf(path, raw)}
	if r.Strip {
		if tg.path, tg.rawPath, err = routing.StripSegments(raw, n); err != nil {
			return target{}, &reply{http.StatusBadRequest, textBadEscape, 0}
		}
	}
	return tg, nil
}

// underZone reports whether a canonical host lies under the apps zone.
func (t *table) underZone(host string) bool {
	return t != nil && t.zone != "" && strings.HasSuffix(host, "."+t.zone)
}
