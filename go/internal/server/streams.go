// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"github.com/gardenlinux/feos/go/internal/ports"
	"github.com/gardenlinux/feos/go/internal/watch"
)

// The streams of every instance: the port door and the watches.
func init() { streams = buildStreams }

// buildStreams builds the port door and the watch service of an instance
// from its parts: the engine (which brings sandboxes up and holds them for
// the door, and tells the watches a sandbox's state), the nodes' port, the
// bindings (whose Kubernetes ports are never forwarded), the database,
// and the resolution of sandbox tokens for the door's own auth class.
// The watches' event streams end when the HTTP server's shutdown begins;
// the upgraded port connections are closed once it has returned.
func buildStreams(i *Instance) Streams {
	p := ports.New(i.Engine, i.Links, i.Bindings, i.ResolveSandbox, i.DB, i.log)
	w := watch.New(i.Engine, i.Links, i.DB, i.ID(), i.log)
	return Streams{
		Ports:         p,
		Forwarder:     p,
		Watches:       w,
		Run:           []func(context.Context) error{p.Run, w.Run},
		Loops:         w.Loops(),
		OnShutdown:    w.EndStreams,
		AfterRequests: p.CloseUpgraded,
	}
}
